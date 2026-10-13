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
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/postgres"
	"github.com/logisticinfotech/pgtijori/app/storage"
	"github.com/logisticinfotech/pgtijori/config"
)

// ConfPath is pgBackRest's default config file, so plain `pgbackrest` commands
// typed over SSH (e.g. during a restore) use the same settings.
const ConfPath = "/etc/pgbackrest/pgbackrest.conf"

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
		{"repo1-path", "/" + strings.TrimSuffix(storage.PgBackRestPrefix, "/")},
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
		{"start-fast", "y"},
		{"process-max", "2"},
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
	return "pgbackrest --stanza=" + cfg.PgBackRest.Stanza + " archive-push %p"
}

// RenderPostgresConf returns the conf.d file that turns on WAL archiving.
// archive_mode only takes effect after a PostgreSQL restart.
func RenderPostgresConf(cfg *config.Config) string {
	return header + "\n" +
		"archive_mode = on\n" +
		"archive_command = '" + ArchiveCommand(cfg) + "'\n" +
		"archive_timeout = " + strconv.Itoa(int(time.Duration(cfg.PgBackRest.ArchiveTimeout)/time.Second)) + "\n"
}

// WriteConfigs writes pgbackrest.conf (root:postgres 0640, it holds the storage
// secret) and the PostgreSQL conf.d file (0644). It reports whether either changed.
func (e *Engine) WriteConfigs() (changed bool, err error) {
	conf, err := RenderConfig(e.Cfg)
	if err != nil {
		return false, err
	}
	gid, err := groupOf(e.Cfg.Postgres.OSUser)
	if err != nil {
		return false, err
	}
	c1, err := writeIfChanged(ConfPath, conf, 0o640, gid)
	if err != nil {
		return false, err
	}
	c2, err := writeIfChanged(e.Cluster.ConfDPath(), RenderPostgresConf(e.Cfg), 0o644, -1)
	if err != nil {
		return false, err
	}
	return c1 || c2, nil
}

func groupOf(username string) (int, error) {
	u, err := user.Lookup(username)
	if err != nil {
		return 0, fmt.Errorf("look up OS user %s: %w", username, err)
	}
	return strconv.Atoi(u.Gid)
}

// writeIfChanged atomically replaces path with content, owned by root and gid
// (-1 keeps the default group).
func writeIfChanged(path, content string, mode os.FileMode, gid int) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && string(old) == content {
		info, err := os.Stat(path)
		if err == nil && info.Mode().Perm() == mode {
			return false, nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
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
	if gid >= 0 {
		if err := tmp.Chown(0, gid); err != nil {
			tmp.Close()
			return false, err
		}
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

func (e *Engine) cmd(args ...string) command.Cmd {
	return command.Cmd{
		Name: "pgbackrest",
		Args: append([]string{"--stanza=" + e.Cfg.PgBackRest.Stanza}, args...),
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
