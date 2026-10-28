// SPDX-License-Identifier: Apache-2.0

package mysql

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/config"
)

func TestCommandLogsInWithDefaultsFile(t *testing.T) {
	i := Instance{Cfg: config.Default().MySQL}
	c := i.Command("mysqldump", "app")
	if c.Name != "/usr/bin/mysqldump" || c.User != "" {
		t.Errorf("command = %+v", c)
	}
	if c.Args[0] != "--defaults-extra-file=/etc/liops/mysql.cnf" {
		t.Errorf("--defaults-extra-file must be the first argument: %q", c.Args)
	}
	if !slices.Contains(c.Args, "--socket=/var/run/mysqld/mysqld.sock") {
		t.Errorf("missing --socket: %q", c.Args)
	}

	i.Cfg.BinDir = "/opt/homebrew/opt/mysql@8.4/bin"
	if got := i.Bin("mysql"); got != "/opt/homebrew/opt/mysql@8.4/bin/mysql" {
		t.Errorf("Bin = %q", got)
	}
}

func TestQueryUnescapesBatchOutput(t *testing.T) {
	var args []string
	fake := &command.Fake{Handle: func(_ string, c command.Cmd) ([]byte, error) {
		args = c.Args
		return []byte("tenant_abc\tline\\none\\ttab\\\\slash\nother\tx\n"), nil
	}}
	rows, err := Instance{Cfg: config.Default().MySQL, Run: fake}.Query(context.Background(), "central", "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0][1] != "line\none\ttab\\slash" || rows[1][0] != "other" {
		t.Errorf("rows = %q", rows)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--database=central") || !strings.Contains(joined, "--execute=SELECT 1") {
		t.Errorf("args = %q", args)
	}
}

func TestCheckVersion(t *testing.T) {
	for _, ok := range []string{"8.4.0", "8.4.3-0ubuntu0.24.04.1", "9.1.0"} {
		if err := CheckVersion(ok); err != nil {
			t.Errorf("CheckVersion(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"8.0.39", "5.7.44-log", "garbage"} {
		if err := CheckVersion(bad); err == nil {
			t.Errorf("CheckVersion(%q) should fail", bad)
		}
	}
}

func TestQuote(t *testing.T) {
	if got := Account(`o'brien`, `%`); got != `'o''brien'@'%'` {
		t.Errorf("Account = %s", got)
	}
	if got := Quote(`a\b`); got != `'a\\b'` {
		t.Errorf("Quote = %s", got)
	}
}
