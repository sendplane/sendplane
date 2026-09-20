package postgres

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID guards concurrent Migrate calls (several control replicas
// start at once). It is an arbitrary constant, only this package uses it.
const migrationLockID int64 = 0x5e4d_9137

// Migrate applies the embedded SQL migrations that have not run yet and
// records them in schema_migrations. It is idempotent: running it twice, or
// from two replicas at once, applies nothing the second time.
func (p *Provider) Migrate(ctx context.Context) error {
	if err := p.check(); err != nil {
		return err
	}
	files, err := migrationFiles()
	if err != nil {
		return err
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return mapErr(err)
	}
	defer conn.Release()

	const createTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
	    version    text PRIMARY KEY,
	    applied_at timestamptz NOT NULL DEFAULT now()
	)`
	if _, err := conn.Exec(ctx, createTable); err != nil {
		return mapErr(err)
	}

	for _, name := range files {
		version := strings.TrimSuffix(name, ".sql")
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("postgres: read migration %s: %w", name, err)
		}
		// One transaction per migration, with an advisory lock so that two
		// replicas starting together do not both run the DDL.
		tx, err := conn.Begin(ctx)
		if err != nil {
			return mapErr(err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return mapErr(err)
		}
		var applied bool
		err = tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`,
			version).Scan(&applied)
		if err != nil {
			_ = tx.Rollback(ctx)
			return mapErr(err)
		}
		if applied {
			_ = tx.Rollback(ctx)
			continue
		}
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("postgres: migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version); err != nil {
			_ = tx.Rollback(ctx)
			return mapErr(err)
		}
		if err := tx.Commit(ctx); err != nil {
			return mapErr(err)
		}
	}
	return nil
}

// migrationFiles lists the embedded migrations in lexical order, which is the
// order they are applied in.
func migrationFiles() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("postgres: read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names, nil
}
