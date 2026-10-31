// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Database engines the monitor reads.
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
)

// Where the database servers' sockets are on Ubuntu.
const (
	DefaultPGSocketDir = "/var/run/postgresql"
	DefaultMySQLSocket = "/var/run/mysqld/mysqld.sock"
)

// DBServer is a database server on this machine. The monitor logs in as its own OS user
// (liops-monitor) through the socket: PostgreSQL by peer authentication, MySQL by
// auth_socket. Root liops creates those accounts (setup.MonitorAccess); they can read
// statistics, never the apps' rows.
type DBServer struct {
	Engine string
	Socket string // PostgreSQL: the socket directory; MySQL: the socket file
	Port   int    // PostgreSQL only
	// User is the login: the monitor's own OS user (the clients would otherwise take
	// it from $USER, which isn't always right).
	User string
	// Exec runs a client program; tests replace it.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Label names the server in series and the UI: "postgres" or "postgres-5433", "mysql".
func (d DBServer) Label() string {
	if d.Engine == EnginePostgres && d.Port != 5432 {
		return fmt.Sprintf("postgres-%d", d.Port)
	}
	return d.Engine
}

// Title is the server for people: "PostgreSQL" or "PostgreSQL (port 5433)", "MySQL".
func (d DBServer) Title() string {
	if d.Engine == EngineMySQL {
		return "MySQL"
	}
	if d.Port != 5432 {
		return fmt.Sprintf("PostgreSQL (port %d)", d.Port)
	}
	return "PostgreSQL"
}

// FindDBServers lists the database servers whose sockets exist: one per PostgreSQL port
// in pgDir, and MySQL's socket.
func FindDBServers(pgDir, mysqlSocket string) []DBServer {
	var out []DBServer
	matches, _ := filepath.Glob(filepath.Join(pgDir, ".s.PGSQL.*"))
	sort.Strings(matches)
	for _, m := range matches {
		if strings.HasSuffix(m, ".lock") {
			continue
		}
		if port, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(m), ".s.PGSQL.")); err == nil {
			out = append(out, DBServer{Engine: EnginePostgres, Socket: pgDir, Port: port, User: osUser(), Exec: execOutput})
		}
	}
	if _, err := os.Stat(mysqlSocket); err == nil {
		out = append(out, DBServer{Engine: EngineMySQL, Socket: mysqlSocket, User: osUser(), Exec: execOutput})
	}
	return out
}

// osUser is the name of the user the monitor runs as.
func osUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func execOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "PGCONNECT_TIMEOUT=5", "LC_ALL=C")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("%s: %v: %s", filepath.Base(name), err, strings.TrimSpace(string(ee.Stderr)))
	}
	return out, err
}

// Query runs sql in database db (PostgreSQL: "" means "postgres") and returns its rows,
// columns split on tabs. Text that may hold tabs or new lines must be hex-encoded in the
// SQL (see unhex).
func (d DBServer) Query(ctx context.Context, db, sql string) ([][]string, error) {
	var out []byte
	var err error
	if d.Engine == EngineMySQL {
		args := []string{"--no-defaults", "--socket=" + d.Socket, "--batch", "--skip-column-names", "--raw"}
		if d.User != "" {
			args = append(args, "--user="+d.User)
		}
		if db != "" {
			args = append(args, "--database="+db)
		}
		out, err = d.Exec(ctx, "mysql", append(args, "--execute="+sql)...)
	} else {
		if db == "" {
			db = "postgres"
		}
		conn := "dbname='" + strings.NewReplacer(`\`, `\\`, `'`, `\'`).Replace(db) + "'"
		args := []string{"-X", "-q", "-A", "-t", "-F", "\t", "-v", "ON_ERROR_STOP=1", "-h", d.Socket, "-p", strconv.Itoa(d.Port), "-d", conn}
		if d.User != "" {
			args = append(args, "-U", d.User)
		}
		out, err = d.Exec(ctx, "psql", append(args, "-c", sql)...)
	}
	if err != nil {
		return nil, err
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line != "" {
			rows = append(rows, strings.Split(line, "\t"))
		}
	}
	return rows, nil
}

// unhex decodes a hex-encoded column; on bad input it returns it as it is.
func unhex(s string) string {
	b, err := hex.DecodeString(s)
	if err != nil {
		return s
	}
	return string(b)
}

func atof(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}
