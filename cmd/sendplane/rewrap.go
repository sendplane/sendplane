package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"slices"
	"syscall"

	"github.com/sendplane/sendplane"
)

const secretsUsage = `usage: sendplane secrets rewrap [--dry-run] [--config <file>]

Re-encrypts every stored secret (transport passwords, DKIM private keys,
bounce and probe mailbox passwords) so that its ciphertext is bound to the row
it belongs to (sendplane.RewrapSecrets). Secrets written by a version before
the binding are opened with secrets.key and no associated data, and sealed
again under the same key with the row's context. Rows already bound are left
alone, so the command is safe to run again.

There is no fallback: once the new version runs, a sender, bounce poller or
probe that meets a secret that has not been rewrapped fails to decrypt it.
Run this after deploying the new version (and its migrations) and before the
senders need the secrets — or with the sender and bounce roles stopped — and
check that it reports no failures.

Platform secrets in config.yaml are never in the database and are not touched.

flags:
`

// runSecrets dispatches `sendplane secrets <subcommand>`.
func runSecrets(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "rewrap" {
		return errors.New("usage: sendplane secrets rewrap [--dry-run] [--config <file>]")
	}
	return runRewrap(args[1:], stdout)
}

func runRewrap(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("sendplane secrets rewrap", flag.ContinueOnError)
	configPath := fs.String("config", envOr("SENDPLANE_CONFIG", "config.yaml"), "path to config.yaml (env SENDPLANE_CONFIG)")
	dryRun := fs.Bool("dry-run", false, "decrypt and report everything, write nothing")
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), secretsUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("secrets rewrap: unexpected argument %q", fs.Arg(0))
	}

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	// The same file the deployment serves with, validated the same way.
	if err := cfg.Validate(true); err != nil {
		return err
	}
	key, err := decodeSecretKey(cfg.Secrets.Key)
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	cipher, err := newAESGCMCipher(key)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	provider, err := openStore(ctx, cfg.Store)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer func() { _ = provider.Close() }()

	// The raw provider, deliberately not wrapped in the platform overlay:
	// platform secrets come from config.yaml and are never stored.
	rep, err := sendplane.RewrapSecrets(ctx, provider, cipher.decryptLegacy, cipher,
		sendplane.RewrapOptions{DryRun: *dryRun})
	printRewrapReport(stdout, rep)
	if err != nil {
		return fmt.Errorf("secrets rewrap: %w", err)
	}
	if n := rep.FailedTotal(); n > 0 {
		return fmt.Errorf("secrets rewrap: %d secret(s) could not be rewrapped; "+
			"the rows above will fail to decrypt until they are fixed or their secret is set again", n)
	}
	return nil
}

// printRewrapReport writes the report as one line per kind and one per
// failed row. It names rows, never values.
func printRewrapReport(w io.Writer, rep sendplane.RewrapReport) {
	if rep.DryRun {
		_, _ = fmt.Fprintln(w, "dry run: nothing was written; \"migrated\" counts what would be")
	}
	kinds := make([]string, 0, len(rep.Kinds))
	for k := range rep.Kinds {
		kinds = append(kinds, k)
	}
	slices.Sort(kinds)
	for _, k := range kinds {
		c := rep.Kinds[k]
		_, _ = fmt.Fprintf(w, "%s: migrated %d, already migrated %d, failed %d\n",
			k, c.Migrated, c.AlreadyMigrated, len(c.Failed))
		for _, f := range c.Failed {
			_, _ = fmt.Fprintf(w, "  failed %s/%s: %s\n", f.TenantID, f.ID, f.Reason)
		}
	}
}
