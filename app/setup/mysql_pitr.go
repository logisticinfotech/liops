// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
)

// minBinlogExpiry is the least time MySQL keeps binlogs on its own disk, so that a
// storage outage of a few days doesn't lose any before they are shipped.
const minBinlogExpiry = 7 * 24 * 3600

// gtidModes are gtid_mode's values, in the order MySQL lets them change online.
var gtidModes = []string{"OFF", "OFF_PERMISSIVE", "ON_PERMISSIVE", "ON"}

// ErrNoXtraBackup: Percona XtraBackup isn't installed.
var ErrNoXtraBackup = errors.New("Percona XtraBackup isn't installed; point-in-time backups need it. Install XtraBackup 8.4:\n" +
	"  curl -fsSLO https://repo.percona.com/apt/percona-release_latest.generic_all.deb\n" +
	"  sudo apt install ./percona-release_latest.generic_all.deb\n" +
	"  sudo percona-release enable pxb-84-lts release\n" +
	"  sudo apt install percona-xtrabackup-84\n" +
	"then run setup again")

// MySQLChange is a server setting point-in-time backups need, and what setup changes it to.
type MySQLChange struct{ Name, Now, Want string }

func (c MySQLChange) String() string { return c.Name + ": " + c.Now + " → " + c.Want }

// mysqlSettings are the server settings point-in-time backups depend on.
type mysqlSettings struct {
	logBin, format, gtidMode, enforceGTID string
	expiry                                int // binlog_expire_logs_seconds
}

// CheckPointInTime checks what point-in-time backups need from the server: XtraBackup
// for its version, binary logging in row format, GTIDs, and binlogs kept long enough.
// What setup can't change is an error; the settings it can change are returned for a
// person to approve, with the current values.
func CheckPointInTime(ctx context.Context, inst mysql.Instance, serverVersion string) ([]MySQLChange, mysqlSettings, error) {
	var cur mysqlSettings
	out, err := inst.Run.Run(ctx, command.Cmd{Name: inst.Bin("xtrabackup"), Args: []string{"--version"}})
	if err != nil {
		return nil, cur, ErrNoXtraBackup
	}
	m := regexp.MustCompile(`version (\d+\.\d+\.\S+)`).FindStringSubmatch(string(out))
	if m == nil {
		return nil, cur, fmt.Errorf("couldn't read XtraBackup's version from %q", command.LastLines(string(out), 1))
	}
	if mine, theirs := mysql.Series(m[1]), mysql.Series(serverVersion); mine != theirs {
		return nil, cur, fmt.Errorf("XtraBackup %s can't back up MySQL %s: install the XtraBackup for MySQL %s (percona-xtrabackup-%s)",
			m[1], serverVersion, theirs, regexp.MustCompile(`\D`).ReplaceAllString(theirs, ""))
	}

	rows, err := inst.Query(ctx, "", "SELECT @@log_bin, @@binlog_format, @@gtid_mode, @@enforce_gtid_consistency, @@binlog_expire_logs_seconds")
	if err != nil {
		return nil, cur, fmt.Errorf("read MySQL's binlog settings: %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 5 {
		return nil, cur, fmt.Errorf("read MySQL's binlog settings: unexpected output %q", rows)
	}
	r := rows[0]
	cur = mysqlSettings{logBin: r[0], format: r[1], gtidMode: r[2], enforceGTID: r[3]}
	if cur.expiry, err = strconv.Atoi(r[4]); err != nil {
		return nil, cur, fmt.Errorf("binlog_expire_logs_seconds: unexpected value %q", r[4])
	}
	if cur.logBin != "1" {
		return nil, cur, errors.New("binary logging is off on this MySQL server, and point-in-time backups need it. " +
			"Remove skip-log-bin (or disable_log_bin) from the MySQL settings, restart MySQL when it suits you, then run setup again")
	}
	if cur.format != "ROW" {
		return nil, cur, fmt.Errorf("binlog_format is %s; point-in-time backups need ROW (MySQL's default). "+
			"Set binlog_format = ROW in the MySQL settings, restart MySQL when it suits you, then run setup again", cur.format)
	}

	var pending []MySQLChange
	if cur.enforceGTID != "ON" {
		pending = append(pending, MySQLChange{"enforce_gtid_consistency", cur.enforceGTID, "ON"})
	}
	if cur.gtidMode != "ON" {
		pending = append(pending, MySQLChange{"gtid_mode", cur.gtidMode, "ON"})
	}
	if cur.expiry < minBinlogExpiry {
		pending = append(pending, MySQLChange{"binlog_expire_logs_seconds", strconv.Itoa(cur.expiry), strconv.Itoa(minBinlogExpiry)})
	}
	return pending, cur, nil
}

// ApplyMySQLSettings makes approved changes as MySQL's root, online with SET GLOBAL: no
// restart. It refuses to turn GTIDs on for a server that replicates, because its
// replicas must change with it.
func ApplyMySQLSettings(ctx context.Context, inst mysql.Instance, rootPassword string, changes []MySQLChange) error {
	root, done, err := rootLogin(ctx, inst, rootPassword)
	if err != nil {
		return err
	}
	defer done()
	exec := func(sql string) error {
		if _, err := root.Query(ctx, "", sql); err != nil {
			return fmt.Errorf("%s: %w", sql, err)
		}
		return nil
	}

	for _, c := range changes {
		if c.Name != "gtid_mode" && c.Name != "enforce_gtid_consistency" {
			continue
		}
		for _, q := range []string{"SHOW REPLICA STATUS", "SHOW REPLICAS"} {
			rows, err := root.Query(ctx, "", q)
			if err != nil {
				return fmt.Errorf("check replication: %w", err)
			}
			if len(rows) > 0 {
				return errors.New("this MySQL server replicates (" + q + " lists a connection), so liops won't turn GTIDs on by itself: " +
					"the whole replication setup has to change together. Turn GTIDs on across it by hand, then run setup again")
			}
		}
		break
	}

	// A value saved with SET PERSIST would override the settings file at the next restart.
	for _, name := range []string{"gtid_mode", "enforce_gtid_consistency", "binlog_expire_logs_seconds"} {
		if err := exec("RESET PERSIST IF EXISTS " + name); err != nil {
			return err
		}
	}
	for _, c := range changes {
		switch c.Name {
		case "enforce_gtid_consistency":
			if c.Now == "OFF" {
				if err := exec("SET GLOBAL enforce_gtid_consistency = WARN"); err != nil {
					return err
				}
			}
			if err := exec("SET GLOBAL enforce_gtid_consistency = ON"); err != nil {
				return err
			}
		case "gtid_mode":
			from := slices.Index(gtidModes, c.Now)
			if from < 0 {
				return fmt.Errorf("gtid_mode: unexpected value %q", c.Now)
			}
			for _, mode := range gtidModes[from+1:] {
				if mode == "ON" {
					if err := waitAnonymousTransactions(ctx, root); err != nil {
						return err
					}
				}
				if err := exec("SET GLOBAL gtid_mode = " + mode); err != nil {
					return err
				}
			}
		case "binlog_expire_logs_seconds":
			if err := exec("SET GLOBAL binlog_expire_logs_seconds = " + strconv.Itoa(minBinlogExpiry)); err != nil {
				return err
			}
		}
	}
	return nil
}

// anonymousWait is how long to wait for transactions started without a GTID to finish.
var anonymousWait = 2 * time.Minute

// waitAnonymousTransactions waits until no transaction without a GTID is running, as
// MySQL requires before gtid_mode can go from ON_PERMISSIVE to ON.
func waitAnonymousTransactions(ctx context.Context, root mysql.Instance) error {
	deadline := time.Now().Add(anonymousWait)
	for {
		rows, err := root.Query(ctx, "", "SHOW GLOBAL STATUS LIKE 'Ongoing_anonymous_transaction_count'")
		if err != nil {
			return err
		}
		if len(rows) == 1 && len(rows[0]) == 2 && rows[0][1] == "0" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("transactions started before GTIDs were turned on are still running after %s; "+
				"gtid_mode is ON_PERMISSIVE for now. Run setup --enable-gtid again once they have finished", anonymousWait)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// writeMySQLConf writes the settings point-in-time backups need into a file MySQL reads
// at startup. SET GLOBAL only lasts until a restart, and SET PERSIST keeps values in the
// data directory, which a restored backup replaces.
func writeMySQLConf(path string, cur mysqlSettings) error {
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		return fmt.Errorf("mysql.conf_file: %s doesn't exist; set mysql.conf_file to a file in a directory MySQL reads its settings from (on Ubuntu, /etc/mysql/mysql.conf.d/)", filepath.Dir(path))
	}
	content := "# Written by liops setup: settings that point-in-time backups need. They are also set in\n" +
		"# the running server; this file keeps them after a restart or a restore.\n" +
		"[mysqld]\n" +
		"gtid_mode = ON\n" +
		"enforce_gtid_consistency = ON\n" +
		"binlog_expire_logs_seconds = " + strconv.Itoa(max(cur.expiry, minBinlogExpiry)) + "\n"
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		return nil
	}
	return writeAtomic(path, content, 0o644)
}

// nonInnoDBTables counts user tables in storage engines other than InnoDB.
func nonInnoDBTables(ctx context.Context, inst mysql.Instance) (int, error) {
	n, err := inst.Value(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_TYPE = 'BASE TABLE' AND ENGINE <> 'InnoDB' "+
		"AND TABLE_SCHEMA NOT IN ('mysql', 'sys', 'information_schema', 'performance_schema')")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(n)
}
