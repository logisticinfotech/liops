// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// MySQLUser is liops's own MySQL account. It logs in through the socket only.
const MySQLUser = "liops"

// mysqlPrivileges are what liops's account needs: mysqldump (SELECT, SHOW VIEW, TRIGGER,
// EVENT, SHOW_ROUTINE, LOCK TABLES), reading users and grants (SELECT on mysql.*), and,
// for the point-in-time backups to come, XtraBackup and binlog shipping (BACKUP_ADMIN,
// PROCESS, RELOAD, REPLICATION CLIENT), so that setup won't need root again for them.
const mysqlPrivileges = "SELECT, SHOW VIEW, TRIGGER, EVENT, SHOW_ROUTINE, LOCK TABLES, PROCESS, RELOAD, REPLICATION CLIENT, BACKUP_ADMIN"

// OwnersPrefix holds one marker object per server that has used the backup name,
// e.g. owners/mysql-<server_uuid>.
const OwnersPrefix = "owners/"

// ErrNeedRootPassword: MySQL's root can't log in through the socket without a password.
var ErrNeedRootPassword = errors.New("MySQL's root account needs a password here (it can't log in through the socket as the system root user). " +
	"Run setup again with root's MySQL password on stdin, e.g.: read -rs PW && printf '%s\\n' \"$PW\" | sudo liops setup --mysql-root-password-stdin. " +
	"It's used once to create liops's own MySQL account, and never stored")

// SetupMySQL makes sure liops can log in to MySQL with its own account (creating it,
// and its defaults file, if needed), that the server is new enough, and that the
// backup name isn't another server's. rootPassword is only needed when root can't log
// in through the socket. It returns the server version.
func SetupMySQL(ctx context.Context, cfg *config.Config, run command.Runner, store storage.Store, rootPassword string) (version string, err error) {
	inst := mysql.Instance{Cfg: cfg.MySQL, Run: run}
	if _, err := inst.Version(ctx); err != nil {
		if err := createMySQLUser(ctx, inst, rootPassword); err != nil {
			return "", err
		}
	}
	version, err = inst.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("liops can't log in to MySQL with %s: %w", cfg.MySQL.DefaultsFile, err)
	}
	if err := mysql.CheckVersion(version); err != nil {
		return "", err
	}
	if err := CheckMySQLOwner(ctx, cfg, inst, store); err != nil {
		return "", err
	}
	return version, nil
}

// createMySQLUser logs in as MySQL's root, creates (or resets) liops's account with
// a new random password, and writes the defaults file holding it.
func createMySQLUser(ctx context.Context, inst mysql.Instance, rootPassword string) error {
	tmp, err := os.MkdirTemp("", "liops-mysql-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	rootLogin := "[client]\nuser=root\n"
	if rootPassword != "" {
		rootLogin += "password=" + optionValue(rootPassword) + "\n"
	}
	root := inst
	root.Cfg.DefaultsFile = filepath.Join(tmp, "root.cnf")
	if err := os.WriteFile(root.Cfg.DefaultsFile, []byte(rootLogin), 0o600); err != nil {
		return err
	}
	if _, err := root.Version(ctx); err != nil {
		if strings.Contains(err.Error()+command.OutputOf(err), "Can't connect") {
			return fmt.Errorf("can't connect to MySQL through %s: is it running, and is mysql.socket right? %w", inst.Cfg.Socket, err)
		}
		if rootPassword == "" {
			return ErrNeedRootPassword
		}
		return fmt.Errorf("couldn't log in to MySQL as root with that password: %w", err)
	}

	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	password := hex.EncodeToString(secret)
	account := mysql.Account(MySQLUser, "localhost")
	// From a file, not --execute, so the password never shows in the process list.
	sql := fmt.Sprintf("CREATE USER IF NOT EXISTS %[1]s IDENTIFIED BY '%[2]s';\nALTER USER %[1]s IDENTIFIED BY '%[2]s';\nGRANT %[3]s ON *.* TO %[1]s;\n",
		account, password, mysqlPrivileges)
	script := filepath.Join(tmp, "create-user.sql")
	if err := os.WriteFile(script, []byte(sql), 0o600); err != nil {
		return err
	}
	if out, err := inst.Run.Run(ctx, root.Command("mysql", "--batch", "--execute=source "+script)); err != nil {
		return fmt.Errorf("create MySQL account %s: %w\n%s", account, err, out)
	}

	login := "# Written by liops setup: the login of liops's own MySQL account. Keep it private.\n" +
		"[client]\nuser=" + MySQLUser + "\npassword=" + password + "\n"
	return writePrivate(inst.Cfg.DefaultsFile, login)
}

// optionValue quotes a value for a MySQL option file.
func optionValue(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// writePrivate writes a root-only file atomically (mode 0600, directory 0700).
func writePrivate(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mysql-*.cnf")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after a successful rename
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// CheckMySQLOwner makes sure the backup name isn't already used in the bucket by a
// different server, then records this server as its owner. A MySQL server is known by
// its server_uuid; a PostgreSQL server by its pgBackRest backups.
func CheckMySQLOwner(ctx context.Context, cfg *config.Config, inst mysql.Instance, store storage.Store) error {
	uuid, err := inst.Value(ctx, "SELECT @@server_uuid")
	if err != nil {
		return fmt.Errorf("couldn't ask MySQL for its server_uuid: %w", err)
	}
	name := cfg.BackupName()
	mine := OwnersPrefix + "mysql-" + uuid

	pg, err := store.List(ctx, storage.PgBackRestPrefix+"backup/"+name+"/backup.info")
	if err != nil {
		return fmt.Errorf("couldn't check whether backup name %q is already used in the bucket: %w", name, err)
	}
	if len(pg) > 0 {
		return &MySQLNameTakenError{Name: name, Theirs: "a PostgreSQL server"}
	}
	owners, err := store.List(ctx, OwnersPrefix)
	if err != nil {
		return fmt.Errorf("couldn't check whether backup name %q is already used in the bucket: %w", name, err)
	}
	for _, o := range owners {
		if o.Key != mine {
			return &MySQLNameTakenError{Name: name, Theirs: "another server (" + strings.TrimPrefix(o.Key, OwnersPrefix) + ")"}
		}
	}
	if len(owners) == 0 {
		marker := fmt.Sprintf("{\"engine\":\"mysql\",\"server_uuid\":%q}\n", uuid)
		if _, err := store.Put(ctx, mine, strings.NewReader(marker)); err != nil {
			return fmt.Errorf("record this server as the owner of backup name %q: %w", name, err)
		}
	}
	return nil
}

// MySQLNameTakenError: the backup name's folder holds another server's backups.
type MySQLNameTakenError struct{ Name, Theirs string }

func (e *MySQLNameTakenError) Error() string {
	return fmt.Sprintf("backup name %q is already used in the bucket by %s. Nothing was changed. "+
		"Choose a new backup name (mysql.backup_name); each server needs its own", e.Name, e.Theirs)
}
