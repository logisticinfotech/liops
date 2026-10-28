// SPDX-License-Identifier: Apache-2.0

// Package mysqldump is layer 2 for MySQL: a nightly mysqldump of every database,
// gzipped and streamed straight to storage with no file on the server's disk.
package mysqldump

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/dumps"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// Ext is the file extension of a MySQL dump: plain SQL, gzipped.
const Ext = ".sql.gz"

// systemSchemas are MySQL's own databases. They are never dumped.
var systemSchemas = []string{"information_schema", "mysql", "performance_schema", "sys"}

// systemAccounts are MySQL's internal accounts, and Ubuntu's maintenance account
// (its password belongs to that one server). Their users and grants aren't dumped.
var systemAccounts = []string{"mysql.infoschema", "mysql.session", "mysql.sys", "debian-sys-maint"}

// Engine dumps the databases of one MySQL server into a Store.
type Engine struct {
	Cfg      config.Dumps
	Instance mysql.Instance
	Store    storage.Store
}

func New(cfg *config.Config, run command.Runner, store storage.Store) *Engine {
	return &Engine{Cfg: cfg.Dumps, Instance: mysql.Instance{Cfg: cfg.MySQL, Run: run}, Store: store}
}

// Key is the object key of a dump of db taken at t: dumps/<db>/<UTC time>.sql.gz.
func Key(db string, t time.Time) string { return dumps.Key(db, t, Ext) }

// Discover returns the databases to dump: every database except MySQL's own, minus
// the configured exclusions.
func (e *Engine) Discover(ctx context.Context) ([]string, error) {
	// Hex-encoded so that no database name can break the line/column parsing.
	rows, err := e.Instance.Query(ctx, "", "SELECT HEX(SCHEMA_NAME) FROM information_schema.SCHEMATA ORDER BY SCHEMA_NAME")
	if err != nil {
		return nil, err
	}
	var dbs []string
	for _, row := range rows {
		name, err := hex.DecodeString(row[0])
		if err != nil {
			return nil, fmt.Errorf("discover databases: %w", err)
		}
		if db := string(name); !slices.Contains(systemSchemas, db) && !slices.Contains(e.Cfg.Exclude, db) {
			dbs = append(dbs, db)
		}
	}
	return dbs, nil
}

// Labels runs the configured label query and returns database name → label.
// It returns nil when no label query is configured.
func (e *Engine) Labels(ctx context.Context) (map[string]string, error) {
	if e.Cfg.Labels.SQL == "" {
		return nil, nil
	}
	rows, err := e.Instance.Query(ctx, e.Cfg.Labels.Database, e.Cfg.Labels.SQL)
	if err != nil {
		return nil, fmt.Errorf("label query: %w", err)
	}
	labels := make(map[string]string, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			return nil, fmt.Errorf("label query: must return two columns (database name, label), got %d", len(row))
		}
		labels[row[0]] = row[1]
	}
	return labels, nil
}

// Dump streams a gzipped SQL dump of db to storage. --single-transaction gives a
// consistent dump of InnoDB tables without locking them. --set-gtid-purged=OFF keeps
// the dump loadable into any server, which is what a single-database dump is for.
func (e *Engine) Dump(ctx context.Context, db string, at time.Time) (key string, size int64, err error) {
	key = Key(db, at)
	cmd := e.Instance.Command("mysqldump",
		"--single-transaction", "--quick", "--routines", "--triggers", "--events", "--hex-blob",
		"--set-gtid-purged=OFF", "--no-tablespaces",
		"--", db) // "--": a database name starting with "-" is still a name
	size, err = dumps.Upload(ctx, e.Store, key, func(ctx context.Context, w io.Writer) error {
		zw := gzip.NewWriter(w)
		if err := e.Instance.Run.Stream(ctx, cmd, zw); err != nil {
			return err
		}
		return zw.Close()
	})
	return key, size, err
}

// DumpGlobals stores the server's users and grants as SQL. Restoring a single database
// dump on a fresh server needs its users first. (mysqlpump, which could do this, was
// removed in MySQL 8.4.)
func (e *Engine) DumpGlobals(ctx context.Context, at time.Time) (key string, size int64, err error) {
	sql, err := e.UsersSQL(ctx, at)
	if err != nil {
		return "", 0, err
	}
	key = dumps.GlobalsKey(at)
	size, err = e.Store.Put(ctx, key, strings.NewReader(sql))
	return key, size, err
}

// defaultRoleRE finds the DEFAULT ROLE clause in SHOW CREATE USER output, which comes
// right before REQUIRE.
var defaultRoleRE = regexp.MustCompile(` DEFAULT ROLE (.+?) REQUIRE `)

// UsersSQL returns CREATE USER and GRANT statements for every account except MySQL's
// own. Every account (roles included) is created before any grant, and default roles
// are set last, so nothing refers to a role that doesn't exist yet.
func (e *Engine) UsersSQL(ctx context.Context, at time.Time) (string, error) {
	rows, err := e.Instance.Query(ctx, "", "SELECT HEX(User), HEX(Host) FROM mysql.user ORDER BY User, Host")
	if err != nil {
		return "", fmt.Errorf("list users: %w", err)
	}
	var creates, grants, defaultRoles []string
	for _, row := range rows {
		if len(row) != 2 {
			return "", fmt.Errorf("list users: unexpected output %q", row)
		}
		user, err1 := hex.DecodeString(row[0])
		host, err2 := hex.DecodeString(row[1])
		if err1 != nil || err2 != nil {
			return "", fmt.Errorf("list users: unexpected output %q", row)
		}
		if slices.Contains(systemAccounts, string(user)) {
			continue
		}
		account := mysql.Account(string(user), string(host))
		// Password hashes as hex: caching_sha2_password hashes contain bytes that don't
		// survive as text.
		out, err := e.Instance.Query(ctx, "", "SET SESSION print_identified_with_as_hex = ON; SHOW CREATE USER "+account+"; SHOW GRANTS FOR "+account)
		if err != nil {
			return "", fmt.Errorf("users and grants of %s: %w", account, err)
		}
		if len(out) == 0 {
			continue
		}
		create, found := strings.CutPrefix(out[0][0], "CREATE USER ")
		if !found {
			return "", fmt.Errorf("SHOW CREATE USER %s: unexpected output %q", account, out[0][0])
		}
		if m := defaultRoleRE.FindStringSubmatch(create); m != nil {
			create = strings.Replace(create, m[0], " REQUIRE ", 1)
			defaultRoles = append(defaultRoles, "SET DEFAULT ROLE "+m[1]+" TO "+account+";")
		}
		creates = append(creates, "CREATE USER IF NOT EXISTS "+create+";")
		for _, g := range out[1:] {
			grants = append(grants, g[0]+";")
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "-- MySQL users and grants, taken by liops at %s.\n", at.UTC().Format(time.RFC3339))
	b.WriteString("-- Restore on a new server with: mysql < this-file\n")
	b.WriteString("-- Accounts that already exist are kept as they are; their grants are applied again.\n\n")
	for _, s := range creates {
		b.WriteString(s + "\n")
	}
	b.WriteString("\n")
	for _, s := range grants {
		b.WriteString(s + "\n")
	}
	if len(defaultRoles) > 0 {
		b.WriteString("\n")
		for _, s := range defaultRoles {
			b.WriteString(s + "\n")
		}
	}
	return b.String(), nil
}
