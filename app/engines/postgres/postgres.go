// SPDX-License-Identifier: Apache-2.0

// Package postgres runs SQL and client programs against the local cluster, as the
// postgres OS user over the Unix socket (peer authentication, no password).
package postgres

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
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

// HomebrewPrefix is where Homebrew is installed: /opt/homebrew on Apple silicon,
// /usr/local on Intel Macs.
var HomebrewPrefix = func() string {
	if runtime.GOARCH == "arm64" {
		return "/opt/homebrew"
	}
	return "/usr/local"
}()

// Homebrew reports whether the cluster is Homebrew's (postgres.layout: homebrew).
func (c Cluster) Homebrew() bool { return c.Cfg.Layout == config.LayoutHomebrew }

// Bin returns the path of a PostgreSQL client program matching the server version:
// postgres.bin_dir if set, else e.g. /usr/lib/postgresql/18/bin/pg_dump on
// Debian/Ubuntu or /opt/homebrew/opt/postgresql@18/bin/pg_dump with Homebrew.
func (c Cluster) Bin(name string) string {
	switch {
	case c.Cfg.BinDir != "":
		return filepath.Join(c.Cfg.BinDir, name)
	case c.Homebrew():
		return filepath.Join(HomebrewPrefix, "opt", "postgresql@"+strconv.Itoa(c.Cfg.Version), "bin", name)
	}
	return filepath.Join("/usr/lib/postgresql", strconv.Itoa(c.Cfg.Version), "bin", name)
}

// ConfDPath is where pgtijori's PostgreSQL settings go. Debian/Ubuntu clusters
// include /etc/postgresql/<version>/<cluster>/conf.d/ by default. Homebrew keeps
// postgresql.conf in the data directory, and setup adds an include_dir for conf.d/ there.
func (c Cluster) ConfDPath() string {
	if c.Homebrew() {
		return filepath.Join(c.Cfg.DataDir, "conf.d", "10-pgbackrest.conf")
	}
	return filepath.Join("/etc/postgresql", strconv.Itoa(c.Cfg.Version), c.Cfg.Cluster, "conf.d", "10-pgbackrest.conf")
}

// ServiceName is what runs the cluster: the systemd unit (postgresql@18-main) or
// the Homebrew service (postgresql@18).
func (c Cluster) ServiceName() string {
	if c.Homebrew() {
		return "postgresql@" + strconv.Itoa(c.Cfg.Version)
	}
	return fmt.Sprintf("postgresql@%d-%s", c.Cfg.Version, c.Cfg.Cluster)
}

// RestartCmd restarts the cluster.
func (c Cluster) RestartCmd() command.Cmd {
	if c.Homebrew() {
		return command.Cmd{Name: "brew", Args: []string{"services", "restart", c.ServiceName()}}
	}
	return command.Cmd{Name: "systemctl", Args: []string{"restart", c.ServiceName()}}
}

// RestartCommand is what to type to restart the cluster.
func (c Cluster) RestartCommand() string {
	if c.Homebrew() {
		return "brew services restart " + c.ServiceName()
	}
	return "sudo systemctl restart " + c.ServiceName()
}

// StartCommand is what to type to start the cluster.
func (c Cluster) StartCommand() string {
	if c.Homebrew() {
		return "brew services start " + c.ServiceName()
	}
	return "sudo systemctl start " + c.ServiceName()
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
