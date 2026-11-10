package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sendplane/sendplane"
	"github.com/sendplane/sendplane/store"
)

func TestSecretsSubcommandDispatch(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"secrets"}, &out); err == nil || !strings.Contains(err.Error(), "secrets rewrap") {
		t.Errorf("bare `secrets` = %v, want a usage error", err)
	}
	if err := run([]string{"secrets", "rotate"}, &out); err == nil {
		t.Error("an unknown secrets subcommand was accepted")
	}
	if err := run([]string{"secrets", "rewrap", "--config", t.TempDir() + "/missing.yaml"}, &out); err == nil {
		t.Error("rewrap with a missing config file succeeded")
	}
	if err := run([]string{"secrets", "rewrap", "extra"}, &out); err == nil {
		t.Error("rewrap accepted a stray argument")
	}
}

func TestPrintRewrapReport(t *testing.T) {
	var out bytes.Buffer
	printRewrapReport(&out, sendplane.RewrapReport{DryRun: true, Kinds: map[string]*sendplane.RewrapCounts{
		store.PurposeTransportPassword: {Migrated: 2, AlreadyMigrated: 1, Failed: []sendplane.RewrapFailure{
			{TenantID: "t1", ID: "row-1", Reason: "legacy decryption failed"},
		}},
		store.PurposeDomainDKIMPrivateKey: {},
	}})
	got := out.String()
	for _, want := range []string{
		"dry run",
		"sending_domain.dkim_private_key: migrated 0, already migrated 0, failed 0",
		"transport.password: migrated 2, already migrated 1, failed 1",
		"failed t1/row-1: legacy decryption failed",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report lacks %q:\n%s", want, got)
		}
	}
}
