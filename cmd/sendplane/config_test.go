package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
