// Command sendplane is the reference binary described in
// docs/architecture.md 2-3 and ADR-0001: it wraps the sendplane library with
// config-file-driven API Key / JWT authentication, a webhook EventSink and
// AES-GCM secret encryption, and runs any combination of the control, sender
// and bounce roles from one binary and one image (--roles).
//
// See config.example.yaml for the configuration schema and README.md for an
// overview in Korean.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/cmd/sendplane/console"
	"github.com/sendplane/sendplane/store"
	"github.com/sendplane/sendplane/store/mongo"
	"github.com/sendplane/sendplane/store/postgres"
)

// drainTimeout is how long a shutdown waits for in-flight HTTP requests
// before giving up, per the deliverable's "graceful shutdown on SIGTERM
// (drain 30s)".
const drainTimeout = 30 * time.Second

// readHeaderTimeout bounds how long a client may take to send its request
// headers. It is deliberately generous: the API is called by servers, not by
// browsers on bad mobile links, and the only thing it has to stop is a
// connection held open forever with a trickle of header bytes.
const readHeaderTimeout = 20 * time.Second

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "sendplane:", err)
		os.Exit(1)
	}
}

// cliFlags is the parsed command line, kept as a struct so run is testable
// without touching package-level flag state.
type cliFlags struct {
	configPath     string
	roles          string
	listen         string
	migrate        bool
	migrateOnStart bool
}

func parseFlags(args []string) (cliFlags, error) {
	fs := newFlagSet()
	f := cliFlags{}

	defaultConfig := envOr("SENDPLANE_CONFIG", "config.yaml")
	defaultRoles := envOr("SENDPLANE_ROLES", "control,sender,bounce")

	fs.StringVar(&f.configPath, "config", defaultConfig, "path to config.yaml (env SENDPLANE_CONFIG)")
	fs.StringVar(&f.roles, "roles", defaultRoles, "comma-separated roles to run: control,sender,bounce (env SENDPLANE_ROLES)")
	fs.StringVar(&f.listen, "listen", ":8080", "address the HTTP server (API + /healthz) listens on")
	fs.BoolVar(&f.migrate, "migrate", false, "run store migrations and exit")
	fs.BoolVar(&f.migrateOnStart, "migrate-on-start", false, "run store migrations before starting the selected roles")

	if err := fs.Parse(args); err != nil {
		return cliFlags{}, err
	}
	return f, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// roleSet parses the --roles flag into a set, validating every entry.
type roleSet struct {
	control, sender, bounce bool
}

func parseRoles(s string) (roleSet, error) {
	var rs roleSet
	for _, part := range strings.Split(s, ",") {
		switch strings.TrimSpace(part) {
		case "control":
			rs.control = true
		case "sender":
			rs.sender = true
		case "bounce":
			rs.bounce = true
		case "":
			// tolerate "control,,sender"
		default:
			return roleSet{}, fmt.Errorf("unknown role %q (want control, sender or bounce)", part)
		}
	}
	if !rs.control && !rs.sender && !rs.bounce {
		return roleSet{}, fmt.Errorf("--roles named no known role")
	}
	return rs, nil
}

func run(args []string, stdout io.Writer) error {
	flags, err := parseFlags(args)
	if err != nil {
		return err
	}
	roles, err := parseRoles(flags.roles)
	if err != nil {
		return err
	}

	cfg, err := LoadConfig(flags.configPath)
	if err != nil {
		return err
	}
	if err := cfg.Validate(!flags.migrate); err != nil {
		return err
	}

	logger, err := newLogger(cfg.Log, stdout)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	provider, err := openStore(ctx, cfg.Store)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() {
		if err := provider.Close(); err != nil {
			logger.Error("store close failed", "error", err)
		}
	}()

	if flags.migrate {
		logger.Info("running migrations")
		if err := provider.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		logger.Info("migrations complete")
		return nil
	}
	if flags.migrateOnStart {
		logger.Info("running migrations before start")
		if err := provider.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	sp, err := buildSendplane(ctx, cfg, provider, logger)
	if err != nil {
		return err
	}

	return serve(ctx, cfg, flags, roles, sp, logger)
}

// buildSendplane wires config into sendplane.Options and constructs the
// engine instance.
func buildSendplane(ctx context.Context, cfg *Config, provider store.Provider, logger *slog.Logger) (*sendplane.Sendplane, error) {
	authenticator, err := buildAuthenticator(ctx, cfg.Auth, logger)
	if err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	authorizer := buildAuthorizer(cfg.Authz)

	secretKey, err := decodeSecretKey(cfg.Secrets.Key)
	if err != nil {
		return nil, fmt.Errorf("secrets: %w", err)
	}
	cipher, err := newAESGCMCipher(secretKey)
	if err != nil {
		return nil, err
	}

	var hooks sendplane.Hooks
	if cfg.Events.Webhook.URL != "" {
		hooks.Events = newWebhookSink(cfg.Events.Webhook, logger)
	}

	probeCfg, err := cfg.Probe.ToHost()
	if err != nil {
		return nil, err
	}
	if probeCfg.Enabled && len(probeCfg.HMACKey) == 0 {
		logger.Warn("probe is enabled with no probe.hmac_key: " +
			"any mail in a probe mailbox can produce a verdict")
	}

	sp, err := sendplane.New(sendplane.Options{
		Store:   provider,
		Auth:    authenticator,
		Authz:   authorizer,
		Hooks:   hooks,
		Secrets: cipher,
		Limits:  cfg.Limits.ToHost(),
		Probe:   probeCfg,
		Logger:  logger,
	})
	if err != nil {
		return nil, err
	}
	return sp, nil
}

func buildAuthenticator(ctx context.Context, cfg AuthConfig, logger *slog.Logger) (sendplane.Authenticator, error) {
	switch cfg.Mode {
	case "apikey":
		return newAPIKeyAuthenticator(cfg.APIKeys)
	case "jwt":
		return newJWTAuthenticator(ctx, cfg.JWT)
	case "none", "":
		return newNoneAuthenticator(logger), nil
	default:
		return nil, fmt.Errorf("unknown auth.mode %q", cfg.Mode)
	}
}

func buildAuthorizer(cfg AuthzConfig) sendplane.Authorizer {
	switch cfg.Mode {
	case "roles":
		return newRolesAuthorizer(cfg.Roles)
	default: // "allow_all", ""
		return allowAllAuthorizer{}
	}
}

// openStore builds the store.Provider named by cfg.Driver.
func openStore(ctx context.Context, cfg StoreConfig) (store.Provider, error) {
	switch cfg.Driver {
	case "postgres":
		return postgres.Open(ctx, cfg.DSN)
	case "mongo":
		return mongo.Open(ctx, cfg.DSN, cfg.MongoDB)
	default:
		return nil, fmt.Errorf("unknown store.driver %q", cfg.Driver)
	}
}

// buildMux assembles the HTTP server's top-level routing.
//
// sendplane's API handler is mounted at "/" only when the control role is
// present; other roles serve /healthz alone. The embedded console
// (cmd/sendplane/console) is mounted under cfg.Console.Path — "/console" by
// default, never "/" — alongside it, also only for the control role: a
// sender- or bounce-only pod has no API for the console to call. Because the
// console's own path is more specific than sp.Handler()'s "/" mount,
// net/http's ServeMux routes it there without the two colliding.
func buildMux(cfg *Config, roles roleSet, sp *sendplane.Sendplane) *http.ServeMux {
	mux := http.NewServeMux()
	if roles.control {
		// sendplane's own router already serves GET /healthz, and it answers
		// the JSON body api/openapi.yaml documents. Registering a plain-text
		// one here as well would shadow it: net/http's mux prefers the more
		// specific pattern, so "/healthz" would win over "/".
		mux.Handle("/", sp.Handler())

		if cfg.Console.Enabled != nil && *cfg.Console.Enabled {
			base := cfg.Console.Path
			if base == "" {
				base = "/console"
			}
			pattern := base
			if pattern != "/" {
				pattern += "/"
			}
			mux.Handle(pattern, console.Handler(base))
		}
	} else {
		// A sender- or bounce-only pod serves no API, but Kubernetes still
		// probes it.
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
	}
	return mux
}

// serve runs the HTTP server and the selected roles until ctx is cancelled,
// then drains for up to drainTimeout.
//
// The HTTP server always starts (not only when roles.control is set): it is
// how every role, including a sender-only or bounce-only pod, answers
// Kubernetes' /healthz probe.
func serve(ctx context.Context, cfg *Config, flags cliFlags, roles roleSet, sp *sendplane.Sendplane, logger *slog.Logger) error {
	mux := buildMux(cfg, roles, sp)
	httpServer := &http.Server{
		Addr:    flags.listen,
		Handler: mux,
		// Bounds the slow-header attack: the body size is already bounded by
		// host.Limits, the header phase was not (gosec G112).
		ReadHeaderTimeout: readHeaderTimeout,
	}

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		logger.Info("http server listening", "addr", flags.listen, "control", roles.control)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
		defer cancel()
		logger.Info("shutting down http server", "drain", drainTimeout)
		return httpServer.Shutdown(shutdownCtx)
	})

	if roles.control {
		g.Go(func() error {
			logger.Info("control role starting")
			return sp.RunControl(gctx)
		})
	}
	if roles.sender {
		g.Go(func() error {
			logger.Info("sender role starting")
			return sp.RunSender(gctx, senderConfigFrom(cfg.Sender))
		})
	}
	if roles.bounce {
		g.Go(func() error {
			logger.Info("bounce role starting", "worker_id", cfg.Bounce.WorkerID)
			return sp.RunBounce(gctx, bounceConfigFrom(cfg.Bounce))
		})
	}

	return g.Wait()
}

// bounceConfigFrom maps the config file's bounce section onto
// sendplane.BounceConfig.
func bounceConfigFrom(cfg BounceConfig) sendplane.BounceConfig {
	out := sendplane.BounceConfig{
		WorkerID:        cfg.WorkerID,
		PollInterval:    time.Duration(cfg.PollInterval),
		RefreshInterval: time.Duration(cfg.RefreshInterval),
	}
	if cfg.UseIdle != nil {
		out.UseIdle = *cfg.UseIdle
	}
	return out
}

// senderConfigFrom maps the config file's sender section onto
// sendplane.SenderConfig.
func senderConfigFrom(cfg SenderConfig) sendplane.SenderConfig {
	return sendplane.SenderConfig{
		WorkerID: cfg.WorkerID,
		Lanes: map[store.Lane]int{
			store.LaneTransactional: cfg.Lanes.Transactional,
			store.LaneBulk:          cfg.Lanes.Bulk,
			store.LaneProbe:         cfg.Lanes.Probe,
		},
		ClaimBatch:           cfg.ClaimBatch,
		LeaseFor:             time.Duration(cfg.LeaseFor),
		EHLOName:             cfg.EHLOName,
		DefaultRatePerSecond: cfg.DefaultRatePerSecond,
	}
}
