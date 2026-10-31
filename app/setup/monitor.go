// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/engines/postgres"
)

// MonitorUser is the monitoring service's OS user, and its login in both databases: it
// logs in through the socket as itself (PostgreSQL peer authentication, MySQL
// auth_socket), so there is no password anywhere.
const MonitorUser = "liops-monitor"

// monitorConfName is the PostgreSQL settings file liops writes for monitoring, next to
// 10-pgbackrest.conf.
const monitorConfName = "20-liops-monitor.conf"

// PostgresMonitorAccess gives the monitoring service a read-only login and query
// statistics:
//
//   - the role liops-monitor, in pg_monitor: it can read statistics and settings, never
//     the apps' rows;
//   - the pg_stat_statements extension, and the library in shared_preload_libraries,
//     added to the libraries already there, in conf.d/20-liops-monitor.conf.
//
// Loading a library needs a restart, which it never does: it reports whether one is
// needed. Safe to run again.
func PostgresMonitorAccess(ctx context.Context, c postgres.Cluster) (needsRestart bool, err error) {
	if _, err := c.Query(ctx, "postgres", `DO $$ BEGIN
IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '`+MonitorUser+`') THEN CREATE ROLE "`+MonitorUser+`" LOGIN; END IF;
END $$; GRANT pg_monitor TO "`+MonitorUser+`"`); err != nil {
		return false, fmt.Errorf("create the role %s: %w", MonitorUser, err)
	}
	if _, err := c.Query(ctx, "postgres", "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		return false, fmt.Errorf("create the extension pg_stat_statements (it comes with PostgreSQL; on Ubuntu in the postgresql-%d package): %w", c.Cfg.Version, err)
	}
	libs, err := c.Setting(ctx, "shared_preload_libraries")
	if err != nil {
		return false, err
	}
	if !slices.Contains(splitLibraries(libs), "pg_stat_statements") {
		want := strings.Join(append(splitLibraries(libs), "pg_stat_statements"), ",")
		conf := "# Written by liops: monitoring's query statistics (pg_stat_statements).\n" +
			"# The libraries that were already loaded are kept.\n" +
			"shared_preload_libraries = '" + want + "'\n"
		dir := filepath.Dir(c.ConfDPath())
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return false, err
		}
		if err := writeAtomic(filepath.Join(dir, monitorConfName), conf, 0o644); err != nil {
			return false, err
		}
		if _, err := c.Query(ctx, "postgres", "SELECT pg_reload_conf()"); err != nil {
			return false, err
		}
	}
	// pending_restart says whether the file's value waits for a restart; a value set
	// later (postgresql.auto.conf, from ALTER SYSTEM) would win over the file.
	for range 10 {
		rows, err := c.Query(ctx, "postgres", `SELECT pending_restart, (SELECT count(*) FROM pg_file_settings
WHERE name = 'shared_preload_libraries' AND sourcefile LIKE '%`+monitorConfName+`' AND NOT applied AND error IS NULL)
FROM pg_settings WHERE name = 'shared_preload_libraries'`)
		if err != nil {
			return false, err
		}
		if len(rows) == 1 && len(rows[0]) == 2 {
			if rows[0][0] == "t" {
				return true, nil
			}
			if rows[0][1] != "0" && !strings.Contains(libs, "pg_stat_statements") {
				return false, fmt.Errorf("shared_preload_libraries is also set in postgresql.auto.conf (ALTER SYSTEM), which wins over %s: add pg_stat_statements there, or ALTER SYSTEM RESET shared_preload_libraries", monitorConfName)
			}
		}
		if libs, _ = c.Setting(ctx, "shared_preload_libraries"); slices.Contains(splitLibraries(libs), "pg_stat_statements") {
			return false, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return false, fmt.Errorf("PostgreSQL didn't pick up %s after a reload", monitorConfName)
}

func splitLibraries(s string) []string {
	var out []string
	for _, l := range strings.Split(s, ",") {
		if l = strings.Trim(strings.TrimSpace(l), `"`); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// mysqlMonitorGrants are the monitoring login's privileges: the process list and
// InnoDB's transactions (PROCESS), statement and lock statistics (performance_schema,
// sys), and table and index names without their rows (REFERENCES lets information_schema
// show them).
const mysqlMonitorGrants = `GRANT PROCESS, REFERENCES ON *.* TO %[1]s;
GRANT SELECT ON performance_schema.* TO %[1]s;
GRANT SELECT, EXECUTE ON sys.* TO %[1]s;
`

// MySQLMonitorAccess gives the monitoring service its read-only MySQL login,
// 'liops-monitor'@'localhost' IDENTIFIED WITH auth_socket. It logs in as MySQL's root
// (through the socket, or with rootPassword, which is used and forgotten). Safe to run
// again.
func MySQLMonitorAccess(ctx context.Context, inst mysql.Instance, rootPassword string) error {
	root, done, err := rootLogin(ctx, inst, rootPassword)
	if err != nil {
		return err
	}
	defer done()
	account := mysql.Account(MonitorUser, "localhost")
	// Ubuntu's MySQL loads auth_socket (its root logs in with it); upstream's doesn't.
	rows, err := root.Query(ctx, "", "SELECT 1 FROM information_schema.PLUGINS WHERE PLUGIN_NAME = 'auth_socket' AND PLUGIN_STATUS = 'ACTIVE'")
	if err != nil {
		return err
	}
	sql := ""
	if len(rows) == 0 {
		sql += "INSTALL PLUGIN auth_socket SONAME 'auth_socket.so';\n"
	}
	sql += "CREATE USER IF NOT EXISTS " + account + " IDENTIFIED WITH auth_socket;\n" + fmt.Sprintf(mysqlMonitorGrants, account)
	script := filepath.Join(filepath.Dir(root.Cfg.DefaultsFile), "monitor-user.sql")
	if err := os.WriteFile(script, []byte(sql), 0o600); err != nil {
		return err
	}
	if out, err := inst.Run.Run(ctx, root.Command("mysql", "--batch", "--execute=source "+script)); err != nil {
		return fmt.Errorf("create MySQL account %s: %w\n%s", account, err, out)
	}
	return nil
}
