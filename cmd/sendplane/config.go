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
}

// ToHost converts to host.ProbeConfig, decoding the base64 HMAC key.
func (p ProbeConfig) ToHost() (host.ProbeConfig, error) {
	out := host.ProbeConfig{
		Enabled:     p.HMACKey != "",
		Nameservers: p.Nameservers,
		Interval:    time.Duration(p.Interval),
		Timeout:     time.Duration(p.Timeout),
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

	switch c.Log.Format {
	case "json", "text", "":
	default:
		errs = append(errs, fmt.Sprintf("log.format %q is not json or text", c.Log.Format))
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
