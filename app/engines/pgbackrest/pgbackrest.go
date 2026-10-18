// SPDX-License-Identifier: Apache-2.0

// Package pgbackrest is layer 1: point-in-time backups. pgtijori writes pgBackRest's
// config and runs it; pgBackRest does the actual backing up and WAL archiving.
package pgbackrest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/postgres"
	"github.com/logisticinfotech/pgtijori/app/storage"
	"github.com/logisticinfotech/pgtijori/config"
)

// ConfPath is pgBackRest's config file. On Debian/Ubuntu it is pgBackRest's default,
// so plain `pgbackrest` commands typed over SSH (e.g. during a restore) use the same
// settings. With Homebrew it is under the Homebrew prefix and pgtijori passes it with
// --config.
func ConfPath(cfg *config.Config) string {
	if cfg.Postgres.Layout == config.LayoutHomebrew {
		return filepath.Join(postgres.HomebrewPrefix, "etc", "pgbackrest", "pgbackrest.conf")
	}
	return "/etc/pgbackrest/pgbackrest.conf"
}

// homebrewLogPath is pgBackRest's log directory with Homebrew. Its default,
// /var/log/pgbackrest, isn't writable without root on a Mac.
func homebrewLogPath() string {
	return filepath.Join(postgres.HomebrewPrefix, "var", "log", "pgbackrest")
}

// program returns the pgbackrest program and the arguments every call needs. With
// Homebrew it is the full path, because PostgreSQL started by `brew services` runs
// archive_command without Homebrew on its PATH.
func program(cfg *config.Config) (string, []string) {
	if cfg.Postgres.Layout == config.LayoutHomebrew {
		return filepath.Join(postgres.HomebrewPrefix, "bin", "pgbackrest"), []string{"--config=" + ConfPath(cfg)}
	}
	return "pgbackrest", nil
}

const header = "# Written by pgtijori. Don't edit: changes are overwritten.\n# Change /etc/pgtijori/config.yaml and run `pgtijori setup` instead.\n"

// Engine runs pgBackRest for one stanza.
type Engine struct {
	Cfg     *config.Config
	Cluster postgres.Cluster
}

func New(cfg *config.Config, run command.Runner) *Engine {
	return &Engine{Cfg: cfg, Cluster: postgres.Cluster{Cfg: cfg.Postgres, Run: run}}
}

// RenderConfig returns the content of pgbackrest.conf. It holds the storage secret.
func RenderConfig(cfg *config.Config) (string, error) {
	s3 := cfg.Storage.S3
	uriStyle := "host"
	if s3.PathStyle {
		uriStyle = "path"
	}
	// pgBackRest takes the port separately from the endpoint host.
	endpoint, port := s3.Endpoint, ""
	if h, p, err := net.SplitHostPort(s3.Endpoint); err == nil {
		endpoint, port = h, p
	}
	global := [][2]string{
		{"repo1-type", "s3"},
		{"repo1-path", "/" + storage.Folder(cfg) + strings.TrimSuffix(storage.PgBackRestPrefix, "/")},
		{"repo1-s3-endpoint", endpoint},
		{"repo1-s3-bucket", s3.Bucket},
		{"repo1-s3-region", s3.Region},
		{"repo1-s3-key", s3.AccessKeyID},
		{"repo1-s3-key-secret", s3.SecretAccessKey},
		{"repo1-s3-uri-style", uriStyle},
		// Keep enough backups to restore to any moment in the last N days.
		{"repo1-retention-full-type", "time"},
		{"repo1-retention-full", strconv.Itoa(cfg.PgBackRest.RetentionDays)},
		{"compress-type", "zst"},
		// Pack small files (most of a cluster: 8-24 KB each) into larger objects. Without
		// it every file is its own upload, and the round trips to object storage, not
		// the data, set the backup time.
		{"repo1-bundle", "y"},
		{"start-fast", "y"},
		{"process-max", "4"},
		{"log-level-console", "info"},
		{"log-level-file", "detail"},
	}
	if port != "" {
		global = append(global, [2]string{"repo1-storage-port", port})
	}
	if !s3.VerifyTLS {
		global = append(global, [2]string{"repo1-storage-verify-tls", "n"})
	}
	stanza := [][2]string{
		{"pg1-path", cfg.Postgres.DataDir},
		{"pg1-port", strconv.Itoa(cfg.Postgres.Port)},
	}
	if cfg.Postgres.Layout == config.LayoutHomebrew {
		global = append(global, [2]string{"log-path", homebrewLogPath()})
		stanza = append(stanza, [2]string{"pg1-socket-path", "/tmp"}) // Homebrew's unix_socket_directories
	}

	var b strings.Builder
	b.WriteString(header)
	if err := writeSection(&b, "global", global); err != nil {
		return "", err
	}
	if err := writeSection(&b, cfg.PgBackRest.Stanza, stanza); err != nil {
		return "", err
	}
	return b.String(), nil
}

func writeSection(b *strings.Builder, name string, kv [][2]string) error {
	fmt.Fprintf(b, "\n[%s]\n", name)
	for _, p := range kv {
		// A line break in a value would let it inject other settings.
		if strings.ContainsAny(p[1], "\r\n") {
			return fmt.Errorf("pgbackrest.conf: %s contains a line break", p[0])
		}
		fmt.Fprintf(b, "%s=%s\n", p[0], p[1])
	}
	return nil
}

// ArchiveCommand is the archive_command PostgreSQL runs for each WAL segment.
func ArchiveCommand(cfg *config.Config) string {
	name, args := program(cfg)
	return strings.Join(append(append([]string{name}, args...), "--stanza="+cfg.PgBackRest.Stanza, "archive-push", "%p"), " ")
}

// RenderPostgresConf returns the conf.d file that turns on WAL archiving.
// archive_mode only takes effect after a PostgreSQL restart.
func RenderPostgresConf(cfg *config.Config) string {
	return header + "\n" +
		"archive_mode = on\n" +
		"archive_command = '" + ArchiveCommand(cfg) + "'\n" +
		"archive_timeout = " + strconv.Itoa(int(time.Duration(cfg.PgBackRest.ArchiveTimeout)/time.Second)) + "\n"
}

// WriteBackRestConf writes pgbackrest.conf (root:postgres 0640, it holds the storage
// secret) in /etc/pgbackrest (root:postgres 0750) and reports whether it changed. The
// postgres user must be able to read it: PostgreSQL runs pgBackRest as postgres for
// every WAL segment. Files are owned by the user pgtijori runs as: root on a server,
// or you with Homebrew.
func (e *Engine) WriteBackRestConf() (changed bool, err error) {
	conf, err := RenderConfig(e.Cfg)
	if err != nil {
		return false, err
	}
	uid := os.Geteuid()
	gid, err := groupOf(e.Cfg.Postgres.OSUser)
	if err != nil {
		return false, err
	}
	confPath := ConfPath(e.Cfg)
	// The directory may already exist from the pgBackRest package with a mode that
	// keeps postgres out, so set it every time.
	if err := ensureDir(filepath.Dir(confPath), 0o750, uid, gid); err != nil {
		return false, err
	}
	if e.Cluster.Homebrew() {
		if err := ensureDir(homebrewLogPath(), 0o750, uid, gid); err != nil {
			return false, err
		}
	}
	return writeIfChanged(confPath, conf, 0o640, uid, gid)
}

// WritePostgresConf writes the PostgreSQL conf.d file (0644) that turns on WAL
// archiving, and with Homebrew makes postgresql.conf include conf.d/. It reports
// whether anything changed.
func (e *Engine) WritePostgresConf() (changed bool, err error) {
	changed, err = writeIfChanged(e.Cluster.ConfDPath(), RenderPostgresConf(e.Cfg), 0o644, os.Geteuid(), -1)
	if err != nil || !e.Cluster.Homebrew() {
		return changed, err
	}
	included, err := includeConfD(filepath.Join(e.Cfg.Postgres.DataDir, "postgresql.conf"))
	return changed || included, err
}

// includeLine makes PostgreSQL read conf.d/ next to postgresql.conf. Debian's
// postgresql.conf has it already; Homebrew's doesn't.
const includeLine = "include_dir = 'conf.d'"

var includeRe = regexp.MustCompile(`(?m)^\s*include_dir\s*=\s*'conf\.d'`)

// includeConfD appends includeLine to postgresql.conf unless it is there. The rest
// of the file is left as it is.
func includeConfD(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	if includeRe.Match(data) {
		return false, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return false, err
	}
	sep := ""
	if len(data) > 0 && data[len(data)-1] != '\n' {
		sep = "\n"
	}
	_, err = fmt.Fprintf(f, "%s\n# Added by pgtijori: loads conf.d/10-pgbackrest.conf (WAL archiving to pgBackRest).\n%s\n", sep, includeLine)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err == nil, err
}

func groupOf(username string) (int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, fmt.Errorf("look up OS user %s: %w", username, err)
	}
	return strconv.Atoi(u.Gid)
}

// ensureDir creates dir if needed and sets its owner and mode.
func ensureDir(dir string, mode os.FileMode, uid, gid int) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return err
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("set the owner of %s: %w", dir, err)
	}
	if err := os.Chmod(dir, mode); err != nil {
		return fmt.Errorf("set the mode of %s: %w", dir, err)
	}
	return nil
}

// upToDate reports whether path already has this content, mode and owner
// (gid -1: any group).
func upToDate(path, content string, mode os.FileMode, uid, gid int) (bool, error) {
	old, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, nil
	}
	return string(old) == content && info.Mode().Perm() == mode &&
		int(st.Uid) == uid && (gid < 0 || int(st.Gid) == gid), nil
}

// writeIfChanged atomically replaces path with content, owned by uid and gid
// (-1 keeps the default group), unless it is already exactly that.
func writeIfChanged(path, content string, mode os.FileMode, uid, gid int) (bool, error) {
	if ok, err := upToDate(path, content, mode, uid, gid); err != nil || ok {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".pgtijori-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Chown(uid, gid); err != nil {
		tmp.Close()
		return false, err
	}
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	return true, os.Rename(tmp.Name(), path)
}

// CheckReadable makes sure the postgres user can read pgbackrest.conf, so a
// permission problem shows up here with a clear message rather than as a failed
// archive_command later.
func (e *Engine) CheckReadable(ctx context.Context) error {
	path := ConfPath(e.Cfg)
	cmd := command.Cmd{Name: "test", Args: []string{"-r", path}, User: e.Cfg.Postgres.OSUser}
	if _, err := e.Cluster.Run.Run(ctx, cmd); err != nil {
		return fmt.Errorf("the %s user can't read %s: every directory above it must be enterable by %s (check: ls -ld %s %s)",
			e.Cfg.Postgres.OSUser, path, e.Cfg.Postgres.OSUser, filepath.Dir(path), path)
	}
	return nil
}

func (e *Engine) cmd(args ...string) command.Cmd {
	name, base := program(e.Cfg)
	return command.Cmd{
		Name: name,
		Args: append(append(base, "--stanza="+e.Cfg.PgBackRest.Stanza), args...),
		User: e.Cfg.Postgres.OSUser,
	}
}

// StanzaCreate creates the stanza in the repository. Safe to run again.
func (e *Engine) StanzaCreate(ctx context.Context) ([]byte, error) {
	return e.Cluster.Run.Run(ctx, e.cmd("stanza-create"))
}

// Check makes sure WAL archiving and the repository work.
func (e *Engine) Check(ctx context.Context) ([]byte, error) {
	return e.Cluster.Run.Run(ctx, e.cmd("check"))
}

// Backup takes a backup of type "full" or "diff". Expired backups are removed afterwards.
func (e *Engine) Backup(ctx context.Context, typ string) ([]byte, error) {
	if typ != "full" && typ != "diff" {
		return nil, fmt.Errorf("unknown backup type %q", typ)
	}
	return e.Cluster.Run.Run(ctx, e.cmd("--type="+typ, "backup"))
}

// Info returns the stanza's backups.
func (e *Engine) Info(ctx context.Context) (*Info, error) {
	out, err := e.Cluster.Run.Run(ctx, e.cmd("--output=json", "info"))
	if err != nil {
		return nil, err
	}
	return ParseInfo(out, e.Cfg.PgBackRest.Stanza)
}
