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
	"It's used only while setup runs, and never stored")

// MySQLOptions are the choices a person makes when setting up a MySQL server.
type MySQLOptions struct {
	// RootPassword is only needed when MySQL's root can't log in through the socket.
	RootPassword string
	// ApplySettings approves the server setting changes that point-in-time backups need
	// (setup --enable-gtid). Without it, setup only reports them.
	ApplySettings bool
}

// MySQLResult is what setup found.
type MySQLResult struct {
	Version string
	// Pending are setting changes waiting for approval. Setup isn't complete while there are any.
	Pending []MySQLChange
	// NonInnoDBTables counts user tables in other storage engines (usually MyISAM), which
	// enforce_gtid_consistency can affect.
	NonInnoDBTables int
}

// SetupMySQL makes sure liops can log in to MySQL with its own account (creating it,
// and its defaults file, if needed), that the server is new enough, that the backup
// name isn't another server's, and that the server is ready for point-in-time backups.
// Setting changes are only made with opts.ApplySettings; otherwise they are returned
// in Pending.
func SetupMySQL(ctx context.Context, cfg *config.Config, run command.Runner, store storage.Store, opts MySQLOptions) (MySQLResult, error) {
	var res MySQLResult
	inst := mysql.Instance{Cfg: cfg.MySQL, Run: run}
	version, err := EnsureMySQLAccount(ctx, inst, opts.RootPassword)
	res.Version = version
	if err != nil {
		return res, err
	}
	if err := CheckMySQLOwner(ctx, cfg, inst, store); err != nil {
		return res, err
	}

	pending, current, err := CheckPointInTime(ctx, inst, version)
	if err != nil {
		return res, err
	}
	if len(pending) > 0 {
		if !opts.ApplySettings {
			res.Pending = pending
			res.NonInnoDBTables, _ = nonInnoDBTables(ctx, inst)
			return res, nil
		}
		if err := ApplyMySQLSettings(ctx, inst, opts.RootPassword, pending); err != nil {
			return res, err
		}
		if pending, current, err = CheckPointInTime(ctx, inst, version); err != nil {
			return res, err
		}
		if len(pending) > 0 {
			return res, fmt.Errorf("MySQL settings still need changing after setup changed them: %v", pending)
		}
	}
	// The settings are live; keep them over a restart or a restore.
	return res, writeMySQLConf(inst.Cfg.ConfFile, current)
}

// EnsureMySQLAccount makes sure liops can log in to MySQL with its own account,
// creating it (and its defaults file) as MySQL's root if needed, and that the server
// is new enough. It returns the server version.
func EnsureMySQLAccount(ctx context.Context, inst mysql.Instance, rootPassword string) (string, error) {
	if _, err := inst.Version(ctx); err != nil {
		if err := createMySQLUser(ctx, inst, rootPassword); err != nil {
			return "", err
		}
	}
	version, err := inst.Version(ctx)
	if err != nil {
		return "", fmt.Errorf("liops can't log in to MySQL with %s: %w", inst.Cfg.DefaultsFile, err)
	}
	return version, mysql.CheckVersion(version)
}

// rootLogin returns the instance logged in as MySQL's root: through the socket, or with
// password. done removes the temporary login file.
func rootLogin(ctx context.Context, inst mysql.Instance, password string) (root mysql.Instance, done func(), err error) {
	tmp, err := os.MkdirTemp("", "liops-mysql-")
	if err != nil {
		return inst, nil, err
	}
	done = func() { os.RemoveAll(tmp) }
	login := "[client]\nuser=root\n"
	if password != "" {
		login += "password=" + optionValue(password) + "\n"
	}
	root = inst
	root.Cfg.DefaultsFile = filepath.Join(tmp, "root.cnf")
	if err := os.WriteFile(root.Cfg.DefaultsFile, []byte(login), 0o600); err != nil {
		done()
		return inst, nil, err
	}
	if _, err := root.Version(ctx); err != nil {
		done()
		if strings.Contains(err.Error()+command.OutputOf(err), "Can't connect") {
			return inst, nil, fmt.Errorf("can't connect to MySQL through %s: is it running, and is mysql.socket right? %w", inst.Cfg.Socket, err)
		}
		if password == "" {
			return inst, nil, ErrNeedRootPassword
		}
		return inst, nil, fmt.Errorf("couldn't log in to MySQL as root with that password: %w", err)
	}
	return root, done, nil
}

// createMySQLUser logs in as MySQL's root, creates (or resets) liops's account with
// a new random password, and writes the defaults file holding it.
func createMySQLUser(ctx context.Context, inst mysql.Instance, rootPassword string) error {
	root, done, err := rootLogin(ctx, inst, rootPassword)
	if err != nil {
		return err
	}
	defer done()

	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return err
	}
	password := hex.EncodeToString(secret)
	account := mysql.Account(MySQLUser, "localhost")
	// From a file, not --execute, so the password never shows in the process list.
	sql := fmt.Sprintf("CREATE USER IF NOT EXISTS %[1]s IDENTIFIED BY '%[2]s';\nALTER USER %[1]s IDENTIFIED BY '%[2]s';\nGRANT %[3]s ON *.* TO %[1]s;\n",
		account, password, mysqlPrivileges)
	script := filepath.Join(filepath.Dir(root.Cfg.DefaultsFile), "create-user.sql")
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
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeAtomic(path, content, 0o600)
}

// writeAtomic writes a file through a temporary file and a rename, so a reader never
// sees half of it.
func writeAtomic(path, content string, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".liops-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // no-op after a successful rename
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
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
