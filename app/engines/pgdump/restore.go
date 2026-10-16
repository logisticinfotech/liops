// SPDX-License-Identifier: Apache-2.0

package pgdump

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/postgres"
)

// System databases that must never be dropped or replaced from the UI.
var systemDBs = []string{"postgres", "template0", "template1"}

// postgresMaxIdent is PostgreSQL's NAMEDATALEN-1 limit for identifiers.
const postgresMaxIdent = 63

// RestoreReport is called for each restore phase (step 1..4) before that phase runs.
type RestoreReport func(step int, detail string)

// Restore downloads the dump at key, renames the live database to <db>_old (or
// <db>_old_2, … if needed), creates <db> again, and runs pg_restore into it.
// Connections to the live database are terminated first. The dump object key must
// be for db (see ParseKey).
//
// keptAs is the name of the renamed previous database, or "" if there was none.
// report may be nil. Steps: 1 download, 2 rename, 3 create, 4 pg_restore.
//
// If pg_restore fails after the rename, the new database is dropped and the old
// name is restored when possible.
func (e *Engine) Restore(ctx context.Context, db, key string, report RestoreReport) (keptAs string, err error) {
	if report == nil {
		report = func(int, string) {}
	}
	if err := checkRestoreTarget(db, key); err != nil {
		return "", err
	}

	report(1, "Downloading dump…")
	path, err := e.downloadDump(ctx, key)
	if err != nil {
		return "", err
	}
	defer os.Remove(path)

	if err := e.chownPostgres(ctx, path); err != nil {
		return "", err
	}

	report(2, "Renaming live database…")
	exists, err := e.dbExists(ctx, db)
	if err != nil {
		return "", err
	}
	if exists {
		keptAs, err = e.pickOldName(ctx, db)
		if err != nil {
			return "", err
		}
		if err := e.renameDatabase(ctx, db, keptAs); err != nil {
			return "", fmt.Errorf("rename %s to %s: %w", db, keptAs, err)
		}
	} else {
		report(2, "No live database to rename…")
	}

	report(3, "Creating database…")
	if _, err := e.Cluster.Run.Run(ctx, e.Cluster.Command("createdb", db)); err != nil {
		if keptAs != "" {
			if rerr := e.renameDatabase(ctx, keptAs, db); rerr != nil {
				return keptAs, fmt.Errorf("create database %s: %w (also could not rename %s back to %s: %v)", db, err, keptAs, db, rerr)
			}
			return "", fmt.Errorf("create database %s: %w (previous database restored as %s)", db, err, db)
		}
		return "", fmt.Errorf("create database %s: %w", db, err)
	}

	report(4, "Running pg_restore…")
	cmd := e.Cluster.Command("pg_restore", "--exit-on-error", "-d", postgres.Conn(db), path)
	if _, err := e.Cluster.Run.Run(ctx, cmd); err != nil {
		// Drop the partial new DB and put the old name back when we have one.
		_, _ = e.Cluster.Run.Run(ctx, e.Cluster.Command("dropdb", "--if-exists", "-f", db))
		if keptAs != "" {
			if rerr := e.renameDatabase(ctx, keptAs, db); rerr != nil {
				return keptAs, fmt.Errorf("pg_restore into %s: %w (previous database is still %s; could not rename it back: %v)", db, err, keptAs, rerr)
			}
			return "", fmt.Errorf("pg_restore into %s: %w (previous database restored as %s)", db, err, db)
		}
		return "", fmt.Errorf("pg_restore into %s: %w", db, err)
	}
	return keptAs, nil
}

func checkRestoreTarget(db, key string) error {
	if db == "" {
		return errors.New("database name is empty")
	}
	if slices.Contains(systemDBs, db) {
		return fmt.Errorf("refusing to restore system database %q", db)
	}
	parsed, _, ok := ParseKey(key)
	if !ok {
		return fmt.Errorf("not a dump key: %s", key)
	}
	if parsed != db {
		return fmt.Errorf("dump key is for %q, not %q", parsed, db)
	}
	return nil
}

func (e *Engine) downloadDump(ctx context.Context, key string) (path string, err error) {
	rc, err := e.Store.Get(ctx, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()

	f, err := os.CreateTemp("", "pgtijori-restore-*.dump")
	if err != nil {
		return "", fmt.Errorf("temp dump file: %w", err)
	}
	path = f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(path)
		}
	}()
	if _, err = io.Copy(f, rc); err != nil {
		return "", fmt.Errorf("download %s: %w", key, err)
	}
	if err = f.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func (e *Engine) chownPostgres(ctx context.Context, path string) error {
	user := e.Cluster.Cfg.OSUser
	if user == "" {
		user = "postgres"
	}
	// pgtijori runs as root; do not set Cmd.User (that would run chown via runuser).
	_, err := e.Cluster.Run.Run(ctx, command.Cmd{Name: "chown", Args: []string{user + ":", path}})
	if err != nil {
		return fmt.Errorf("chown dump for %s: %w", user, err)
	}
	return nil
}

func (e *Engine) dbExists(ctx context.Context, name string) (bool, error) {
	rows, err := e.Cluster.Query(ctx, "postgres",
		"SELECT 1 FROM pg_database WHERE datname = "+quoteLiteral(name))
	if err != nil {
		return false, fmt.Errorf("check database %s: %w", name, err)
	}
	return len(rows) > 0, nil
}

// pickOldName chooses db_old, or db_old_2, … that does not already exist and fits
// in PostgreSQL's identifier length limit.
func (e *Engine) pickOldName(ctx context.Context, db string) (string, error) {
	for n := 1; n <= 100; n++ {
		var candidate string
		if n == 1 {
			candidate = db + "_old"
		} else {
			candidate = fmt.Sprintf("%s_old_%d", db, n)
		}
		if utf8.RuneCountInString(candidate) > postgresMaxIdent {
			return "", fmt.Errorf("cannot form a free _old name for %q: names would exceed %d characters", db, postgresMaxIdent)
		}
		if slices.Contains(systemDBs, candidate) {
			continue
		}
		exists, err := e.dbExists(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !exists {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot find a free name like %s_old for the previous database", db)
}

func (e *Engine) renameDatabase(ctx context.Context, from, to string) error {
	// Best-effort: cut sessions so ALTER DATABASE RENAME can proceed.
	_, _ = e.Cluster.Query(ctx, "postgres",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = "+quoteLiteral(from)+" AND pid <> pg_backend_pid()")
	sql := "ALTER DATABASE " + quoteIdent(from) + " RENAME TO " + quoteIdent(to)
	cmd := e.Cluster.Command("psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-d", postgres.Conn("postgres"), "-c", sql)
	if _, err := e.Cluster.Run.Run(ctx, cmd); err != nil {
		return err
	}
	return nil
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
