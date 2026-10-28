// SPDX-License-Identifier: Apache-2.0

// Package mysql is the local MySQL server: where its client programs are, and how to
// run them and queries with liops's own login.
package mysql

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/config"
)

// MinVersion is the oldest MySQL liops supports: 8.4 LTS.
const MinVersion = "8.4"

// Instance is the MySQL server to back up.
type Instance struct {
	Cfg config.MySQL
	Run command.Runner
}

// Bin returns the path of a MySQL client program.
func (i Instance) Bin(name string) string {
	dir := i.Cfg.BinDir
	if dir == "" {
		dir = "/usr/bin"
	}
	return filepath.Join(dir, name)
}

// Command builds a client program command (mysql, mysqldump) that logs in with the
// defaults file, so the password never appears on a command line.
func (i Instance) Command(program string, args ...string) command.Cmd {
	// --defaults-extra-file must come first: MySQL programs ignore it anywhere else.
	return command.Cmd{
		Name: i.Bin(program),
		Args: append([]string{"--defaults-extra-file=" + i.Cfg.DefaultsFile, "--socket=" + i.Cfg.Socket}, args...),
	}
}

// Query runs sql (in database db, if not empty) and returns its rows. The mysql client
// escapes tabs, newlines and backslashes in batch mode; the values come back unescaped.
func (i Instance) Query(ctx context.Context, db, sql string) ([][]string, error) {
	args := []string{"--batch", "--skip-column-names"}
	if db != "" {
		args = append(args, "--database="+db)
	}
	out, err := i.Run.Run(ctx, i.Command("mysql", append(args, "--execute="+sql)...))
	if err != nil {
		if db == "" {
			return nil, fmt.Errorf("mysql query: %w", err)
		}
		return nil, fmt.Errorf("mysql query %s: %w", db, err)
	}
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		cols := strings.Split(line, "\t")
		for j, c := range cols {
			cols[j] = unescape(c)
		}
		rows = append(rows, cols)
	}
	return rows, nil
}

// unescape undoes the mysql client's batch-mode escaping (\t, \n, \\ and \0).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i == len(s)-1 {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case '0':
			b.WriteByte(0)
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

// Value runs a query that returns a single value.
func (i Instance) Value(ctx context.Context, sql string) (string, error) {
	rows, err := i.Query(ctx, "", sql)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return "", fmt.Errorf("%s: unexpected output %q", sql, rows)
	}
	return rows[0][0], nil
}

// Version returns the server version, e.g. "8.4.3".
func (i Instance) Version(ctx context.Context) (string, error) {
	return i.Value(ctx, "SELECT VERSION()")
}

// CheckVersion returns an error when version is older than MinVersion.
func CheckVersion(version string) error {
	major, minor, ok := parseVersion(version)
	if !ok {
		return fmt.Errorf("couldn't read the MySQL version from %q", version)
	}
	if major < 8 || (major == 8 && minor < 4) {
		return fmt.Errorf("MySQL %s is too old: liops needs MySQL %s or newer (upgrade to the 8.4 LTS release)", version, MinVersion)
	}
	return nil
}

// Series returns a version's release series, e.g. "8.4" for "8.4.11-0ubuntu0.26.04.1",
// or "" if it can't be read.
func Series(version string) string {
	major, minor, ok := parseVersion(version)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d.%d", major, minor)
}

// parseVersion reads the major and minor version from e.g. "8.4.3-0ubuntu0.24.04.1".
func parseVersion(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(v, ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	digits := func(s string) string {
		end := strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' })
		if end < 0 {
			return s
		}
		return s[:end]
	}
	major, err1 := strconv.Atoi(parts[0])
	minor, err2 := strconv.Atoi(digits(parts[1]))
	return major, minor, err1 == nil && err2 == nil
}

// Quote returns s as a single-quoted SQL string literal.
func Quote(s string) string {
	return "'" + strings.NewReplacer(`\`, `\\`, `'`, `''`, "\x00", `\0`).Replace(s) + "'"
}

// Account returns 'user'@'host', for statements such as SHOW GRANTS FOR.
func Account(user, host string) string { return Quote(user) + "@" + Quote(host) }
