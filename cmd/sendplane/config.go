// Config parsing for the reference binary. The schema is documented in
// config.example.yaml next to this file; see cmd/sendplane/README.md for a
// short overview in Korean.
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/probe/inbound"
)

// Duration wraps time.Duration so config.yaml can spell it "5m" instead of a
// number of nanoseconds.
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (interface{}, error) {
	return time.Duration(d).String(), nil
}

// Config is the top-level shape of config.yaml.
type Config struct {
	Store   StoreConfig   `yaml:"store"`
	Secrets SecretsConfig `yaml:"secrets"`
	Auth    AuthConfig    `yaml:"auth"`
	Authz   AuthzConfig   `yaml:"authz"`
	Events  EventsConfig  `yaml:"events"`
	Sender  SenderConfig  `yaml:"sender"`
	Bounce  BounceConfig  `yaml:"bounce"`
	Probe   ProbeConfig   `yaml:"probe"`
	Limits  LimitsConfig  `yaml:"limits"`
	Log     LogConfig     `yaml:"log"`
	Console ConsoleConfig `yaml:"console"`
}

// StoreConfig selects and configures the store.Provider.
type StoreConfig struct {
	Driver  string `yaml:"driver"` // "postgres" | "mongo"
	DSN     string `yaml:"dsn"`
	MongoDB string `yaml:"mongo_db"` // required when driver is "mongo"
}

// SecretsConfig configures the AES-256-GCM SecretCipher.
type SecretsConfig struct {
	// Key is 32 bytes, base64-encoded (standard encoding).
	Key string `yaml:"key"`
}

// APIKeyEntry is one accepted API key, stored as a SHA-256 hash so the
// plaintext key never lives on disk.
type APIKeyEntry struct {
	KeyHash   string   `yaml:"key_hash"` // hex-encoded sha256(key)
	Tenant    string   `yaml:"tenant"`
	Principal string   `yaml:"principal"`
	Roles     []string `yaml:"roles"`
}

// JWTConfig configures JWKS-verified bearer tokens.
type JWTConfig struct {
	Issuer      string `yaml:"issuer"`
	Audience    string `yaml:"audience"`
	JWKSURL     string `yaml:"jwks_url"`
	TenantClaim string `yaml:"tenant_claim"` // default "tenant"
	RolesClaim  string `yaml:"roles_claim"`  // default "roles"
}

// AuthConfig selects the Authenticator.
type AuthConfig struct {
	Mode    string        `yaml:"mode"` // "apikey" | "jwt" | "none"
	APIKeys []APIKeyEntry `yaml:"api_keys"`
	JWT     JWTConfig     `yaml:"jwt"`
}

// AuthzConfig selects the Authorizer.
type AuthzConfig struct {
	Mode  string              `yaml:"mode"` // "allow_all" | "roles"
	Roles map[string][]string `yaml:"roles"`
}

// WebhookConfig configures the webhook host.EventSink.
type WebhookConfig struct {
	URL                  string   `yaml:"url"`
	Secret               string   `yaml:"secret"` // HMAC-SHA256 key for X-Sendplane-Signature
	Timeout              Duration `yaml:"timeout"`
	AllowPrivateNetworks bool     `yaml:"allow_private_networks"`
}

// EventsConfig configures event delivery to the host.
type EventsConfig struct {
	Webhook WebhookConfig `yaml:"webhook"`
}

// LanesConfig is the sender worker pool size per lane.
type LanesConfig struct {
	Transactional int `yaml:"transactional"`
	Bulk          int `yaml:"bulk"`
	Probe         int `yaml:"probe"`
}

// SenderConfig configures the sender role.
type SenderConfig struct {
	WorkerID             string      `yaml:"worker_id"` // default: os.Hostname()
	Lanes                LanesConfig `yaml:"lanes"`
	ClaimBatch           int         `yaml:"claim_batch"`
	LeaseFor             Duration    `yaml:"lease_for"`
	EHLOName             string      `yaml:"ehlo_name"`
	DefaultRatePerSecond float64     `yaml:"default_rate_per_second"`
}

// BounceConfig configures the bounce role.
type BounceConfig struct {
	WorkerID        string   `yaml:"worker_id"` // default: "bounce-" + hostname
	PollInterval    Duration `yaml:"poll_interval"`
	RefreshInterval Duration `yaml:"refresh_interval"`
	// UseIdle is on by default; set it to false to force plain polling.
	UseIdle *bool `yaml:"use_idle"`
}

// ProbeConfig configures the loopback health probe (architecture 11). The
// mailboxes are tenant rows managed through the API; this is the process-wide
// half. Probing is enabled by the presence of this section's hmac_key or an
// explicit `enabled: true`.
type ProbeConfig struct {
	Enabled *bool `yaml:"enabled"`
	// HMACKey is base64 (standard encoding). It signs the probe token, so a
	// mail somebody else drops in the probe mailbox cannot produce a verdict.
	HMACKey string `yaml:"hmac_key"`
	// Nameservers are queried directly for the DNS diagnostic layer; an entry
	// may carry a port. Empty uses /etc/resolv.conf.
	Nameservers []string `yaml:"nameservers"`
	Interval    Duration `yaml:"interval"`
	Timeout     Duration `yaml:"timeout"`
	// MailboxCheckInterval is how often the mailbox-check leader loop logs in
	// to every enabled probe and bounce mailbox. Zero uses 15m. The loop runs
	// whether or not probing is enabled (architecture 11.5).
	MailboxCheckInterval Duration `yaml:"mailbox_check_interval"`
	// Webhooks are the inbound endpoints a receiving provider posts delivered
	// probe mail to (ADR-0016). They are global: one endpoint serves every
	// tenant, and the tenant comes out of the probe token on the message.
	Webhooks []ProbeWebhookConfig `yaml:"webhooks"`
}

// ProbeWebhookConfig is one inbound webhook. The provider name has to be one
// internal/probe/inbound knows; Validate lists the registered ones when it is
// not, because a typo here is otherwise a route that silently never fires.
type ProbeWebhookConfig struct {
	Provider string `yaml:"provider"`
	// Secrets verify the provider's request signature. At least one is
	// required, and any one matching is enough — which is what makes a
	// rotation a matter of listing the new secret next to the old one for as
	// long as both may arrive, rather than a flag day.
	Secrets []string `yaml:"secrets"`
	// Secret is sugar for a one-element Secrets, because a deployment that
	// has never rotated anything should not have to write a list.
	Secret string `yaml:"secret"`
	// Path overrides the default "/probe/inbound/<provider>" route, for a
	// deployment whose reverse proxy already owns that prefix.
	Path string `yaml:"path"`
	// Tolerance is how far a signed timestamp may be from this process's
	// clock. Zero uses the provider's own default (5m for the `sendplane`
	// format). Only a provider whose signature carries a timestamp accepts
	// it; setting it for one that does not is refused at startup.
	Tolerance Duration `yaml:"tolerance"`
}

// secrets is the effective secret list: `secrets` plus `secret`, blanks
// dropped. A blank is what an unset ${VAR} expands to (LoadConfig runs
// os.ExpandEnv over the file), so dropping it here is what turns "the
// environment variable is missing" into the startup error below rather than
// into an endpoint verifying against the empty string.
func (w ProbeWebhookConfig) secrets() []string {
	out := make([]string, 0, len(w.Secrets)+1)
	for _, s := range append(append([]string(nil), w.Secrets...), w.Secret) {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ToHost converts to host.ProbeConfig, decoding the base64 HMAC key.
func (p ProbeConfig) ToHost() (host.ProbeConfig, error) {
	out := host.ProbeConfig{
		Enabled:              p.HMACKey != "",
		Nameservers:          p.Nameservers,
		Interval:             time.Duration(p.Interval),
		Timeout:              time.Duration(p.Timeout),
		MailboxCheckInterval: time.Duration(p.MailboxCheckInterval),
	}
	for _, w := range p.Webhooks {
		out.Webhooks = append(out.Webhooks, host.ProbeWebhook{
			Provider:  strings.TrimSpace(w.Provider),
			Secrets:   w.secrets(),
			Path:      strings.TrimSpace(w.Path),
			Tolerance: time.Duration(w.Tolerance),
		})
	}
	if p.Enabled != nil {
		out.Enabled = *p.Enabled
	}
	if p.HMACKey != "" {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(p.HMACKey))
		if err != nil {
			return host.ProbeConfig{}, fmt.Errorf("probe.hmac_key: not base64: %w", err)
		}
		out.HMACKey = key
	}
	return out, nil
}

// LimitsConfig maps onto host.Limits.
type LimitsConfig struct {
	MaxRecipientsPerCampaign int   `yaml:"max_recipients_per_campaign"`
	MaxVarsBytes             int   `yaml:"max_vars_bytes"`
	MaxBodyBytes             int64 `yaml:"max_body_bytes"`
	MaxRecipientLineBytes    int   `yaml:"max_recipient_line_bytes"`
}

// ToHost converts to host.Limits. Zero fields fall back to host.DefaultLimits
// through host.Limits.WithDefaults.
func (l LimitsConfig) ToHost() host.Limits {
	return host.Limits{
		MaxRecipientsPerCampaign: l.MaxRecipientsPerCampaign,
		MaxVarsBytes:             l.MaxVarsBytes,
		MaxBodyBytes:             l.MaxBodyBytes,
		MaxRecipientLineBytes:    l.MaxRecipientLineBytes,
	}.WithDefaults()
}

// LogConfig configures the slog logger.
type LogConfig struct {
	Level  string `yaml:"level"`  // "debug" | "info" | "warn" | "error"
	Format string `yaml:"format"` // "json" | "text"
}

// ConsoleConfig configures the embedded Vue ops console (cmd/sendplane/console,
// ADR-0010). It is only ever mounted alongside the control role (serve in
// main.go): a sender- or bounce-only pod has no API for it to talk to.
//
// Path defaults to "/console", not "/": the control role's REST API already
// owns "/api/v1" and tracking owns "/t/" under sp.Handler()'s "/" mount, and
// mounting the console SPA's catch-all fallback at "/" too would contend
// with net/http.ServeMux for the same prefix. "/console" keeps the three
// apart with no special-casing in serve().
type ConsoleConfig struct {
	Enabled *bool  `yaml:"enabled"`
	Path    string `yaml:"path"`
}

// defaultLanes is used when config.yaml sets no lane at all (every field
// zero), matching internal/sender's own defaults (architecture 8.1).
var defaultLanes = LanesConfig{Transactional: 8, Bulk: 32, Probe: 1}

// applyDefaults fills the fields that have a sensible default so config.yaml
// only has to name what differs.
func (c *Config) applyDefaults() {
	if c.Sender.Lanes == (LanesConfig{}) {
		c.Sender.Lanes = defaultLanes
	}
	if c.Sender.ClaimBatch == 0 {
		c.Sender.ClaimBatch = 200
	}
	if c.Sender.LeaseFor == 0 {
		c.Sender.LeaseFor = Duration(5 * time.Minute)
	}
	if c.Sender.WorkerID == "" {
		if h, err := os.Hostname(); err == nil && h != "" {
			c.Sender.WorkerID = h
		} else {
			c.Sender.WorkerID = "sendplane-sender"
		}
	}
	if c.Bounce.WorkerID == "" {
		// A different lease owner from the sender's: the two roles hold
		// different locks, and sharing one ID makes a log line ambiguous.
		c.Bounce.WorkerID = "bounce-" + c.Sender.WorkerID
	}
	if c.Bounce.UseIdle == nil {
		on := true
		c.Bounce.UseIdle = &on
	}
	if c.Auth.Mode == "" {
		c.Auth.Mode = "none"
	}
	if c.Authz.Mode == "" {
		c.Authz.Mode = "allow_all"
	}
	if c.Auth.JWT.TenantClaim == "" {
		c.Auth.JWT.TenantClaim = "tenant"
	}
	if c.Auth.JWT.RolesClaim == "" {
		c.Auth.JWT.RolesClaim = "roles"
	}
	if c.Events.Webhook.Timeout == 0 {
		c.Events.Webhook.Timeout = Duration(10 * time.Second)
	}
	if c.Log.Level == "" {
		c.Log.Level = "info"
	}
	if c.Log.Format == "" {
		c.Log.Format = "json"
	}
	if c.Console.Enabled == nil {
		on := true
		c.Console.Enabled = &on
	}
	if c.Console.Path == "" {
		c.Console.Path = "/console"
	}
}

// LoadConfig reads path, expands ${VAR} references against the process
// environment (so Helm can inject DSN/keys through a Secret without a
// templating engine), unmarshals it and applies defaults. It does not
// validate; call Validate separately once the caller knows whether the
// process actually needs secrets (a --migrate run does not).
func LoadConfig(path string) (*Config, error) {
	// The path is this process's own --config flag (or SENDPLANE_CONFIG), not
	// anything a request can influence, so reading it by name is the point.
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied config path
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	expanded := os.ExpandEnv(string(raw))
	var c Config
	if err := yaml.Unmarshal([]byte(expanded), &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	return &c, nil
}

// Validate checks the fields every run needs. needSecrets is true unless the
// process is only running --migrate.
func (c *Config) Validate(needSecrets bool) error {
	var errs []string

	switch c.Store.Driver {
	case "postgres":
		if c.Store.DSN == "" {
			errs = append(errs, "store.dsn is required for driver postgres")
		}
	case "mongo":
		if c.Store.DSN == "" {
			errs = append(errs, "store.dsn is required for driver mongo")
		}
		if c.Store.MongoDB == "" {
			errs = append(errs, "store.mongo_db is required for driver mongo")
		}
	case "":
		errs = append(errs, "store.driver is required (postgres|mongo)")
	default:
		errs = append(errs, fmt.Sprintf("store.driver %q is not postgres or mongo", c.Store.Driver))
	}

	if needSecrets {
		if c.Secrets.Key == "" {
			errs = append(errs, "secrets.key is required")
		} else if _, err := decodeSecretKey(c.Secrets.Key); err != nil {
			errs = append(errs, "secrets.key: "+err.Error())
		}
	}

	switch c.Auth.Mode {
	case "apikey":
		if len(c.Auth.APIKeys) == 0 {
			errs = append(errs, "auth.api_keys must not be empty in apikey mode")
		}
		for i, k := range c.Auth.APIKeys {
			if k.KeyHash == "" {
				errs = append(errs, fmt.Sprintf("auth.api_keys[%d].key_hash is required", i))
			}
			if k.Tenant == "" {
				errs = append(errs, fmt.Sprintf("auth.api_keys[%d].tenant is required", i))
			}
		}
	case "jwt":
		if c.Auth.JWT.JWKSURL == "" {
			errs = append(errs, "auth.jwt.jwks_url is required in jwt mode")
		}
	case "none":
		// nothing to validate; New{Authenticator} logs the warning.
	default:
		errs = append(errs, fmt.Sprintf("auth.mode %q is not apikey, jwt or none", c.Auth.Mode))
	}

	switch c.Authz.Mode {
	case "allow_all", "":
	case "roles":
		if len(c.Authz.Roles) == 0 {
			errs = append(errs, "authz.roles must not be empty in roles mode")
		}
	default:
		errs = append(errs, fmt.Sprintf("authz.mode %q is not allow_all or roles", c.Authz.Mode))
	}

	if c.Events.Webhook.URL != "" {
		if c.Events.Webhook.Secret == "" {
			errs = append(errs, "events.webhook.secret is required when events.webhook.url is set")
		}
	}

	// `probe.enabled: true` used to be silently overruled by an empty
	// hmac_key: Enabled follows the key unless the operator says otherwise,
	// so a deployment that asked for probing got none and nothing said why.
	// Saying it here is the whole fix.
	if c.Probe.Enabled != nil && *c.Probe.Enabled && strings.TrimSpace(c.Probe.HMACKey) == "" {
		errs = append(errs,
			"probe.hmac_key is required when probe.enabled is true "+
				"(32 random bytes, base64: openssl rand -base64 32)")
	}
	if key := strings.TrimSpace(c.Probe.HMACKey); key != "" {
		if _, err := base64.StdEncoding.DecodeString(key); err != nil {
			errs = append(errs, "probe.hmac_key: not valid base64")
		}
	}
	errs = append(errs, validateProbeWebhooks(c.Probe.Webhooks)...)

	switch c.Log.Format {
	case "json", "text", "":
	default:
		errs = append(errs, fmt.Sprintf("log.format %q is not json or text", c.Log.Format))
	}

	if p := c.Console.Path; p != "" {
		if !strings.HasPrefix(p, "/") {
			errs = append(errs, fmt.Sprintf("console.path %q must start with /", p))
		}
		// sp.Handler() already owns "/api/v1" (the REST API) and "/t/"
		// (tracking) under its "/" mount; a console.path there would shadow
		// (or be shadowed by) them instead of getting its own prefix.
		if p == "/" || p == "/t" || strings.HasPrefix(p, "/t/") ||
			p == "/api" || strings.HasPrefix(p, "/api/") ||
			p == "/healthz" {
			errs = append(errs, fmt.Sprintf("console.path %q collides with the API, tracking or healthz routes", p))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid config:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

// decodeSecretKey base64-decodes k and checks it is 32 bytes, the size
// AES-256-GCM requires.
func decodeSecretKey(k string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(k)
	if err != nil {
		return nil, fmt.Errorf("not valid base64: %w", err)
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("must decode to 32 bytes, got %d", len(b))
	}
	return b, nil
}

// validateProbeWebhooks checks the inbound webhook list (ADR-0016). Every
// failure here is one that would otherwise only show up as "the probe never
// completed", hours later and blamed on the sender, so all three are refused
// at startup rather than logged.
func validateProbeWebhooks(ws []ProbeWebhookConfig) []string {
	var errs []string
	paths := map[string]int{}
	for i, w := range ws {
		name := strings.TrimSpace(w.Provider)
		if name == "" {
			errs = append(errs, fmt.Sprintf("probe.webhooks[%d].provider is required", i))
			continue
		}
		if _, ok := inbound.Lookup(name); !ok {
			errs = append(errs, fmt.Sprintf("probe.webhooks[%d].provider %q is not a known inbound provider (have: %s)",
				i, name, strings.Join(inbound.Names(), ", ")))
		}
		if len(w.secrets()) == 0 {
			errs = append(errs, fmt.Sprintf(
				"probe.webhooks[%d]: at least one of secret/secrets is required: an "+
					"unsigned inbound endpoint lets anybody complete anybody's probe run", i))
		}
		switch p, ok := inbound.Lookup(name); {
		case w.Tolerance < 0:
			errs = append(errs, fmt.Sprintf("probe.webhooks[%d].tolerance must not be negative", i))
		case w.Tolerance > 0 && ok:
			// A tolerance only means something to a signature that carries a
			// timestamp. Accepting it for a provider that has none would be a
			// replay window an operator believes they configured.
			if _, timestamped := p.(inbound.ToleranceSetter); !timestamped {
				errs = append(errs, fmt.Sprintf(
					"probe.webhooks[%d].tolerance: provider %q has no timestamp in its signature, "+
						"so there is nothing to tolerate", i, name))
			}
		}
		path := strings.TrimSpace(w.Path)
		if path == "" {
			path = inbound.DefaultPath(name)
		}
		if !strings.HasPrefix(path, "/") {
			errs = append(errs, fmt.Sprintf("probe.webhooks[%d].path %q must start with /", i, path))
			continue
		}
		if first, dup := paths[path]; dup {
			errs = append(errs, fmt.Sprintf("probe.webhooks[%d].path %q is already used by probe.webhooks[%d]",
				i, path, first))
			continue
		}
		paths[path] = i
	}
	return errs
}
