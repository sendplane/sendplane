package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sendplane/sendplane/store"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	path := writeConfig(t, `
store:
  driver: postgres
  dsn: postgres://localhost/sendplane
secrets:
  key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sender.Lanes != defaultLanes {
		t.Errorf("Sender.Lanes = %+v, want default %+v", cfg.Sender.Lanes, defaultLanes)
	}
	if cfg.Sender.ClaimBatch != 200 {
		t.Errorf("Sender.ClaimBatch = %d, want 200", cfg.Sender.ClaimBatch)
	}
	if time.Duration(cfg.Sender.LeaseFor) != 5*time.Minute {
		t.Errorf("Sender.LeaseFor = %v, want 5m", time.Duration(cfg.Sender.LeaseFor))
	}
	if cfg.Sender.WorkerID == "" {
		t.Error("Sender.WorkerID should default to the hostname, got empty")
	}
	if cfg.Auth.Mode != "none" {
		t.Errorf("Auth.Mode = %q, want none", cfg.Auth.Mode)
	}
	if cfg.Authz.Mode != "allow_all" {
		t.Errorf("Authz.Mode = %q, want allow_all", cfg.Authz.Mode)
	}
	if cfg.Log.Level != "info" || cfg.Log.Format != "json" {
		t.Errorf("Log = %+v, want info/json", cfg.Log)
	}
	if err := cfg.Validate(true); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestLoadConfigKeepsExplicitValues(t *testing.T) {
	path := writeConfig(t, `
store:
  driver: mongo
  dsn: mongodb://localhost
  mongo_db: sendplane
secrets:
  key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
sender:
  worker_id: worker-1
  claim_batch: 50
  lease_for: 90s
  lanes:
    transactional: 1
    bulk: 2
    probe: 3
log:
  level: debug
  format: text
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Sender.WorkerID != "worker-1" {
		t.Errorf("WorkerID = %q, want worker-1", cfg.Sender.WorkerID)
	}
	if cfg.Sender.ClaimBatch != 50 {
		t.Errorf("ClaimBatch = %d, want 50", cfg.Sender.ClaimBatch)
	}
	if time.Duration(cfg.Sender.LeaseFor) != 90*time.Second {
		t.Errorf("LeaseFor = %v, want 90s", time.Duration(cfg.Sender.LeaseFor))
	}
	want := LanesConfig{Transactional: 1, Bulk: 2, Probe: 3}
	if cfg.Sender.Lanes != want {
		t.Errorf("Lanes = %+v, want %+v", cfg.Sender.Lanes, want)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != "text" {
		t.Errorf("Log = %+v, want debug/text", cfg.Log)
	}
}

func TestLoadConfigExpandsEnv(t *testing.T) {
	t.Setenv("SP_TEST_DSN", "postgres://env-expanded/sendplane")
	path := writeConfig(t, `
store:
  driver: postgres
  dsn: ${SP_TEST_DSN}
secrets:
  key: AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Store.DSN != "postgres://env-expanded/sendplane" {
		t.Errorf("Store.DSN = %q, want the expanded value", cfg.Store.DSN)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestValidateRejectsMissingStoreDriver(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	if err := cfg.Validate(false); err == nil {
		t.Fatal("expected an error for an empty store.driver")
	}
}

func TestValidateRejectsMongoWithoutDatabase(t *testing.T) {
	cfg := &Config{Store: StoreConfig{Driver: "mongo", DSN: "mongodb://localhost"}}
	cfg.applyDefaults()
	if err := cfg.Validate(false); err == nil {
		t.Fatal("expected an error for a missing store.mongo_db")
	}
}

func TestValidateRequiresSecretsKeyOnlyWhenAsked(t *testing.T) {
	cfg := &Config{Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"}}
	cfg.applyDefaults()
	if err := cfg.Validate(false); err != nil {
		t.Errorf("Validate(false) should not require secrets.key: %v", err)
	}
	if err := cfg.Validate(true); err == nil {
		t.Fatal("Validate(true) should require secrets.key")
	}
}

func TestValidateRejectsBadSecretsKeyLength(t *testing.T) {
	cfg := &Config{
		Store:   StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Secrets: SecretsConfig{Key: "dG9vc2hvcnQ="}, // "tooshort", not 32 bytes
	}
	cfg.applyDefaults()
	if err := cfg.Validate(true); err == nil {
		t.Fatal("expected an error for a secrets.key that is not 32 bytes")
	}
}

func TestValidateRejectsAPIKeyModeWithoutKeys(t *testing.T) {
	cfg := &Config{
		Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Auth:  AuthConfig{Mode: "apikey"},
	}
	cfg.applyDefaults()
	if err := cfg.Validate(false); err == nil {
		t.Fatal("expected an error for apikey mode with no api_keys")
	}
}

func TestValidateRejectsWebhookURLWithoutSecret(t *testing.T) {
	cfg := &Config{
		Store:  StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Events: EventsConfig{Webhook: WebhookConfig{URL: "https://example.com/hook"}},
	}
	cfg.applyDefaults()
	if err := cfg.Validate(false); err == nil {
		t.Fatal("expected an error for a webhook URL with no secret")
	}
}

// The config file's sender and bounce sections have to reach the library
// unchanged: a lease_for the operator shortened for faster recovery is worth
// nothing if senderConfigFrom drops it on the floor.
func TestRoleConfigsReachTheLibrary(t *testing.T) {
	cfg := SenderConfig{
		WorkerID:             "w-7",
		Lanes:                LanesConfig{Transactional: 2, Bulk: 3, Probe: 1},
		ClaimBatch:           17,
		LeaseFor:             Duration(60 * time.Second),
		EHLOName:             "mail.example.com",
		DefaultRatePerSecond: 12.5,
	}
	got := senderConfigFrom(cfg)
	if got.LeaseFor != 60*time.Second {
		t.Errorf("LeaseFor = %v, want 60s", got.LeaseFor)
	}
	if got.WorkerID != "w-7" || got.ClaimBatch != 17 || got.EHLOName != "mail.example.com" {
		t.Errorf("sender config = %+v", got)
	}
	if got.DefaultRatePerSecond != 12.5 {
		t.Errorf("DefaultRatePerSecond = %v", got.DefaultRatePerSecond)
	}
	if got.Lanes[store.LaneTransactional] != 2 || got.Lanes[store.LaneBulk] != 3 || got.Lanes[store.LaneProbe] != 1 {
		t.Errorf("lanes = %v", got.Lanes)
	}

	idle := false
	b := bounceConfigFrom(BounceConfig{
		WorkerID: "b-7", PollInterval: Duration(30 * time.Second),
		RefreshInterval: Duration(2 * time.Minute), UseIdle: &idle,
	})
	if b.WorkerID != "b-7" || b.PollInterval != 30*time.Second ||
		b.RefreshInterval != 2*time.Minute || b.UseIdle {
		t.Errorf("bounce config = %+v", b)
	}
}

// `probe.enabled: true` with no hmac_key used to be accepted and then
// silently overruled - ToHost derives Enabled from the key - so a deployment
// that asked for probing got none and nothing said why.
func TestValidateRejectsProbeEnabledWithoutKey(t *testing.T) {
	cfg := &Config{
		Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Probe: ProbeConfig{Enabled: ptrTo(true)},
	}
	cfg.applyDefaults()
	err := cfg.Validate(false)
	if err == nil {
		t.Fatal("probe.enabled: true with an empty hmac_key was accepted")
	}
	if !strings.Contains(err.Error(), "probe.hmac_key") {
		t.Fatalf("error does not name the field to fix: %v", err)
	}

	// Off, or on with a key, is fine.
	cfg.Probe = ProbeConfig{Enabled: ptrTo(false)}
	if err := cfg.Validate(false); err != nil {
		t.Errorf("probe.enabled: false must not require a key: %v", err)
	}
	cfg.Probe = ProbeConfig{Enabled: ptrTo(true), HMACKey: "9Qe1sYpVqz3dK7mCw0oXhR2tNbGvLuAjE5FrZ8SdI4w="}
	if err := cfg.Validate(false); err != nil {
		t.Errorf("probe.enabled with a key: %v", err)
	}

	cfg.Probe.HMACKey = "not base64!!"
	if err := cfg.Validate(false); err == nil {
		t.Fatal("a probe.hmac_key that is not base64 was accepted")
	}
}

// Every probe.webhooks failure is one that would otherwise only surface as
// "the probe never completed", hours later and blamed on the sender, so all of
// them are refused at startup (ADR-0016).
func TestValidateProbeWebhooks(t *testing.T) {
	base := func(ws ...ProbeWebhookConfig) *Config {
		cfg := &Config{
			Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
			Probe: ProbeConfig{Webhooks: ws},
		}
		cfg.applyDefaults()
		return cfg
	}

	if err := base(ProbeWebhookConfig{Provider: "sendplane", Secret: "s"}).Validate(false); err != nil {
		t.Fatalf("a registered provider with a secret was rejected: %v", err)
	}
	// `secrets` is the real field and `secret` the sugar; either alone is
	// enough, and both together is a rotation.
	if err := base(ProbeWebhookConfig{Provider: "sendplane", Secrets: []string{"a", "b"}}).Validate(false); err != nil {
		t.Fatalf("a secrets list was rejected: %v", err)
	}

	err := base(ProbeWebhookConfig{Provider: "postmarkk", Secret: "s"}).Validate(false)
	if err == nil {
		t.Fatal("an unknown provider name was accepted")
	}
	// The message has to name the alternatives: a typo here is otherwise a
	// route that silently never fires.
	if !strings.Contains(err.Error(), "postmarkk") || !strings.Contains(err.Error(), "sendplane") {
		t.Fatalf("error does not list the registered providers: %v", err)
	}

	err = base(ProbeWebhookConfig{Provider: "sendplane"}).Validate(false)
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("an unsigned inbound endpoint was accepted: %v", err)
	}
	// An unset ${VAR} expands to the empty string, so a blank secret is the
	// shape a missing environment variable actually arrives in.
	err = base(ProbeWebhookConfig{Provider: "sendplane", Secrets: []string{"", "  "}}).Validate(false)
	if err == nil || !strings.Contains(err.Error(), "secret") {
		t.Fatalf("a blank secret was accepted: %v", err)
	}

	err = base(
		ProbeWebhookConfig{Provider: "sendplane", Secret: "s"},
		ProbeWebhookConfig{Provider: "sendplane", Secret: "s2"},
	).Validate(false)
	if err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("two webhooks on the same path were accepted: %v", err)
	}

	// A path override makes the pair legal again.
	if err := base(
		ProbeWebhookConfig{Provider: "sendplane", Secret: "s"},
		ProbeWebhookConfig{Provider: "sendplane", Secret: "s2", Path: "/hooks/sendplane-2"},
	).Validate(false); err != nil {
		t.Fatalf("distinct paths were rejected: %v", err)
	}

	err = base(ProbeWebhookConfig{Provider: "sendplane", Secret: "s", Path: "hooks"}).Validate(false)
	if err == nil || !strings.Contains(err.Error(), "must start with /") {
		t.Fatalf("a relative path was accepted: %v", err)
	}

	if err := base(ProbeWebhookConfig{
		Provider: "sendplane", Secret: "s", Tolerance: Duration(2 * time.Minute),
	}).Validate(false); err != nil {
		t.Fatalf("a tolerance was rejected for a provider whose signature is timestamped: %v", err)
	}
	err = base(ProbeWebhookConfig{
		Provider: "sendplane", Secret: "s", Tolerance: Duration(-time.Minute),
	}).Validate(false)
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Fatalf("a negative tolerance was accepted: %v", err)
	}
}

// ToHost folds `secret` into `secrets`, which is what the rest of the process
// reads; nothing downstream should have to know the sugar exists.
func TestProbeWebhookToHost(t *testing.T) {
	cfg := ProbeConfig{Webhooks: []ProbeWebhookConfig{{
		Provider: " sendplane ", Secret: "sugar", Secrets: []string{"listed", ""},
		Path: " /hooks/in ", Tolerance: Duration(90 * time.Second),
	}}}
	out, err := cfg.ToHost()
	if err != nil {
		t.Fatalf("ToHost: %v", err)
	}
	if len(out.Webhooks) != 1 {
		t.Fatalf("got %d webhooks", len(out.Webhooks))
	}
	got := out.Webhooks[0]
	if got.Provider != "sendplane" || got.Path != "/hooks/in" || got.Tolerance != 90*time.Second {
		t.Errorf("got %+v", got)
	}
	if len(got.Secrets) != 2 || got.Secrets[0] != "listed" || got.Secrets[1] != "sugar" {
		t.Errorf("secrets = %q, want [listed sugar]", got.Secrets)
	}
}

func ptrTo[T any](v T) *T { return &v }

// `auth.tenant_header.enabled: true` with no roles is a feature that is on and
// unusable: nobody could select a tenant, including the system tenant it
// exists for, so the console it was switched on for would 403 on every
// switch. auth.mode=none is the exception, where there is one fixed local
// principal and the role list would only be ceremony (ADR-0017).
func TestValidateRejectsTenantHeaderWithoutRoles(t *testing.T) {
	cfg := &Config{
		Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Auth: AuthConfig{
			Mode: "apikey", APIKeys: []APIKeyEntry{{KeyHash: "abc", Tenant: "acme"}},
			TenantHeader: TenantHeaderConfig{Enabled: ptrTo(true)},
		},
	}
	cfg.applyDefaults()
	err := cfg.Validate(false)
	if err == nil {
		t.Fatal("tenant_header.enabled with no roles was accepted under auth.mode=apikey")
	}
	if !strings.Contains(err.Error(), "auth.tenant_header.roles") {
		t.Fatalf("error does not name the field to fix: %v", err)
	}

	// A role makes it legal.
	cfg.Auth.TenantHeader.Roles = []string{"admin"}
	if err := cfg.Validate(false); err != nil {
		t.Fatalf("tenant_header with a role was rejected: %v", err)
	}
}

func TestValidateAllowsTenantHeaderWithoutRolesInModeNone(t *testing.T) {
	cfg := &Config{
		Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Auth: AuthConfig{
			Mode:         "none",
			TenantHeader: TenantHeaderConfig{Enabled: ptrTo(true)},
		},
	}
	cfg.applyDefaults()
	if err := cfg.Validate(false); err != nil {
		t.Fatalf("auth.mode=none needs no roles: %v", err)
	}
}

// A header name with a space or a colon in it is not a header name, and the
// symptom would be a header nothing ever sends and a switch that silently
// never happens.
func TestValidateRejectsABadTenantHeaderName(t *testing.T) {
	cfg := &Config{
		Store: StoreConfig{Driver: "postgres", DSN: "postgres://localhost/x"},
		Auth: AuthConfig{
			Mode: "none",
			TenantHeader: TenantHeaderConfig{
				Enabled: ptrTo(true), Header: "X-Sendplane Tenant:",
			},
		},
	}
	cfg.applyDefaults()
	err := cfg.Validate(false)
	if err == nil {
		t.Fatal("a header name containing a space and a colon was accepted")
	}
	if !strings.Contains(err.Error(), "auth.tenant_header.header") {
		t.Fatalf("error does not name the field to fix: %v", err)
	}
}
