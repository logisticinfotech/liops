// SPDX-License-Identifier: Apache-2.0

// Package postgres runs SQL and client programs against the local cluster, as the
// postgres OS user over the Unix socket (peer authentication, no password).
package postgres

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/config"
)

// Cluster is the local PostgreSQL cluster.
type Cluster struct {
	Cfg config.Postgres
	Run command.Runner
}

// Bin returns the path of a PostgreSQL client program matching the server version,
// e.g. /usr/lib/postgresql/18/bin/pg_dump on Debian/Ubuntu, or in postgres.bin_dir.
func (c Cluster) Bin(name string) string {
	if c.Cfg.BinDir != "" {
		return filepath.Join(c.Cfg.BinDir, name)
	}
	return filepath.Join("/usr/lib/postgresql", strconv.Itoa(c.Cfg.Version), "bin", name)
}

// ConfDPath is where pgtijori's PostgreSQL settings go. Debian/Ubuntu clusters
// include conf.d/ from postgresql.conf by default.
func (c Cluster) ConfDPath() string {
	return filepath.Join("/etc/postgresql", strconv.Itoa(c.Cfg.Version), c.Cfg.Cluster, "conf.d", "10-pgbackrest.conf")
}

// Conn is a libpq connection string for one database. Passing a bare name to
// -d/--dbname is unsafe: a name containing "=" would be read as a connection string.
func Conn(db string) string {
	r := strings.NewReplacer(`\`, `\\`, `'`, `\'`)
	return "dbname='" + r.Replace(db) + "'"
}

// Command builds a client program command (psql, pg_dump, ...) run as the cluster's OS user.
func (c Cluster) Command(program string, args ...string) command.Cmd {
	return command.Cmd{
		Name: c.Bin(program),
		Args: append([]string{"-p", strconv.Itoa(c.Cfg.Port)}, args...),
		User: c.Cfg.OSUser,
	}
}

// Query runs sql in database db and returns its rows, with columns split on tabs.
func (c Cluster) Query(ctx context.Context, db, sql string) ([][]string, error) {
	cmd := c.Command("psql", "-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-d", Conn(db), "-c", sql)
	out, err := c.Run.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", db, err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		rows = append(rows, strings.Split(line, "\t"))
	}
	return rows, nil
}

// Setting returns the current value of a server setting (SHOW name).
func (c Cluster) Setting(ctx context.Context, name string) (string, error) {
	rows, err := c.Query(ctx, "postgres", "SHOW "+name)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("SHOW %s: unexpected output %q", name, rows)
	}
	return rows[0][0], nil
}
