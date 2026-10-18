// SPDX-License-Identifier: Apache-2.0

// Package setup holds the steps that connect pgtijori to a cluster: find the cluster,
// write the pgBackRest and PostgreSQL settings, turn on WAL archiving and create the
// stanza. `pgtijori setup` and the setup wizard both use it. Every step is safe to
// run again.
package setup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/engines/postgres"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/config"
)

// Cluster is a PostgreSQL cluster found on this machine.
type Cluster struct {
	Layout  string // config.LayoutDebian or config.LayoutHomebrew
	Version int
	Name    string
	Port    int
	Status  string // "online", "down", ...
	Owner   string
	DataDir string
	BinDir  string // empty: the layout's default
}

// Online reports whether the cluster is running.
func (c Cluster) Online() bool { return strings.HasPrefix(c.Status, "online") }

// StartCommand is what to type to start the cluster.
func (c Cluster) StartCommand() string {
	cfg := config.Postgres{Layout: c.Layout, Version: c.Version, Cluster: c.Name}
	return postgres.Cluster{Cfg: cfg}.StartCommand()
}

// goos is runtime.GOOS; tests change it.
var goos = runtime.GOOS

// DetectClusters lists the clusters on this machine: with pg_lsclusters on
// Debian/Ubuntu, or Homebrew's data directories on a Mac.
func DetectClusters(ctx context.Context, run command.Runner) ([]Cluster, error) {
	out, err := run.Run(ctx, command.Cmd{Name: "pg_lsclusters", Args: []string{"--no-header"}})
	if err == nil {
		return ParseClusters(string(out)), nil
	}
	if goos == "darwin" {
		return DetectHomebrew(postgres.HomebrewPrefix)
	}
	return nil, fmt.Errorf("pg_lsclusters (is PostgreSQL from the PGDG repository installed?): %w", err)
}

// DetectHomebrew lists Homebrew's clusters: one data directory per formula,
// <prefix>/var/postgresql@<version>. A cluster is online when the server process in
// its postmaster.pid is running; that file also has the port.
func DetectHomebrew(prefix string) ([]Cluster, error) {
	dirs, err := filepath.Glob(filepath.Join(prefix, "var", "postgresql@*"))
	if err != nil {
		return nil, err
	}
	var clusters []Cluster
	for _, dir := range dirs {
		data, err := os.ReadFile(filepath.Join(dir, "PG_VERSION"))
		if err != nil {
			continue // not a data directory (yet)
		}
		version, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil {
			continue
		}
		formula := filepath.Base(dir)
		c := Cluster{
			Layout: config.LayoutHomebrew, Version: version, Name: formula, Status: "down",
			Port: confPort(filepath.Join(dir, "postgresql.conf")), DataDir: dir,
			BinDir: filepath.Join(prefix, "opt", formula, "bin"),
		}
		if info, err := os.Stat(dir); err == nil {
			if st, ok := info.Sys().(*syscall.Stat_t); ok {
				if u, err := user.LookupId(strconv.Itoa(int(st.Uid))); err == nil {
					c.Owner = u.Username
				}
			}
		}
		if pid, port, ok := readPostmasterPid(filepath.Join(dir, "postmaster.pid")); ok && processRunning(pid) {
			c.Status, c.Port = "online", port
		}
		clusters = append(clusters, c)
	}
	return clusters, nil
}

var portRe = regexp.MustCompile(`(?m)^\s*port\s*=\s*(\d+)`)

// confPort is the port set in postgresql.conf, or PostgreSQL's default 5432.
func confPort(path string) int {
	data, _ := os.ReadFile(path)
	if m := portRe.FindSubmatch(data); m != nil {
		if p, err := strconv.Atoi(string(m[1])); err == nil {
			return p
		}
	}
	return 5432
}

// readPostmasterPid reads the server's process id (line 1) and port (line 4).
func readPostmasterPid(path string) (pid, port int, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) < 4 {
		return 0, 0, false
	}
	pid, err1 := strconv.Atoi(strings.TrimSpace(lines[0]))
	port, err2 := strconv.Atoi(strings.TrimSpace(lines[3]))
	return pid, port, err1 == nil && err2 == nil
}

func processRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// ParseClusters parses `pg_lsclusters --no-header` output:
//
//	18  main    5432 online postgres /var/lib/postgresql/18/main /var/log/postgresql/postgresql-18-main.log
func ParseClusters(out string) []Cluster {
	var clusters []Cluster
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		version, err1 := strconv.Atoi(f[0])
		port, err2 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil {
			continue
		}
		clusters = append(clusters, Cluster{Layout: config.LayoutDebian, Version: version, Name: f[1], Port: port, Status: f[3], Owner: f[4], DataDir: f[5]})
	}
	return clusters
}

// Apply copies a detected cluster into the config.
func Apply(cfg *config.Config, c Cluster) {
	cfg.Postgres.Layout = c.Layout
	cfg.Postgres.BinDir = c.BinDir
	cfg.Postgres.Version = c.Version
	cfg.Postgres.Cluster = c.Name
	cfg.Postgres.Port = c.Port
	cfg.Postgres.DataDir = c.DataDir
	cfg.Postgres.OSUser = c.Owner
}

// ServiceName is the service that runs the cluster, e.g. postgresql@18-main.
func ServiceName(cfg *config.Config) string { return postgres.Cluster{Cfg: cfg.Postgres}.ServiceName() }

// RestartCommand is what to type to restart the cluster.
func RestartCommand(cfg *config.Config) string {
	return postgres.Cluster{Cfg: cfg.Postgres}.RestartCommand()
}

// Archiving is the state of WAL archiving.
type Archiving int

const (
	// NeedsRestart: archive_mode is off; PostgreSQL must be restarted once.
	NeedsRestart Archiving = iota
	// Ready: archive_mode is on and archive_command runs pgBackRest.
	Ready
)

// WriteSettings writes pgbackrest.conf and the conf.d file, and makes PostgreSQL
// reload them when they changed and archiving is already on. It reports whether
// PostgreSQL still needs a restart.
func WriteSettings(ctx context.Context, e *pgbackrest.Engine) (Archiving, error) {
	c1, err := e.WriteBackRestConf()
	if err != nil {
		return NeedsRestart, fmt.Errorf("write settings: %w", err)
	}
	if err := e.CheckReadable(ctx); err != nil {
		return NeedsRestart, err
	}
	// Before WAL archiving points at the bucket: make sure the backup name's folder
	// is new or this cluster's own.
	if err := CheckOwner(ctx, e); err != nil {
		return NeedsRestart, err
	}
	c2, err := e.WritePostgresConf()
	if err != nil {
		return NeedsRestart, fmt.Errorf("write settings: %w", err)
	}
	changed := c1 || c2
	mode, err := e.Cluster.Setting(ctx, "archive_mode")
	if err != nil {
		return NeedsRestart, fmt.Errorf("couldn't ask PostgreSQL for its settings (is it running?): %w", err)
	}
	if mode != "on" {
		return NeedsRestart, nil
	}
	want := pgbackrest.ArchiveCommand(e.Cfg)
	cmd, err := e.Cluster.Setting(ctx, "archive_command")
	if err != nil {
		return NeedsRestart, err
	}
	if changed || cmd != want {
		// archive_command and archive_timeout only need a reload.
		if _, err := e.Cluster.Query(ctx, "postgres", "SELECT pg_reload_conf()"); err != nil {
			return NeedsRestart, err
		}
		for range 10 {
			time.Sleep(300 * time.Millisecond)
			if cmd, err = e.Cluster.Setting(ctx, "archive_command"); err == nil && cmd == want {
				break
			}
		}
	}
	if cmd != want {
		return NeedsRestart, fmt.Errorf("archive_command is %q, expected %q: it's probably also set in postgresql.conf or postgresql.auto.conf, which wins over %s; remove it there",
			cmd, want, e.Cluster.ConfDPath())
	}
	return Ready, nil
}

// CheckOwner makes sure the backup name (pgbackrest.stanza) isn't already used in the
// bucket by a different PostgreSQL cluster. A new name, or this cluster's own backups
// (setup run again, or a server restored from them), are fine. pgBackRest would refuse
// to mix two clusters anyway, but only after archiving was on, with a cryptic error.
func CheckOwner(ctx context.Context, e *pgbackrest.Engine) error {
	name := e.Cfg.PgBackRest.Stanza
	info, err := e.Info(ctx)
	if err != nil {
		return fmt.Errorf("couldn't check whether backup name %q is already used in the bucket: %s: %w",
			name, notify.LikelyReason(command.OutputOf(err)+err.Error()), err)
	}
	if info.SystemID == "" {
		return nil // a new name
	}
	rows, err := e.Cluster.Query(ctx, "postgres", "SELECT system_identifier FROM pg_control_system()")
	if err != nil {
		return fmt.Errorf("couldn't ask PostgreSQL for its system identifier (is it running?): %w", err)
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return fmt.Errorf("pg_control_system: unexpected output %q", rows)
	}
	if mine := rows[0][0]; mine != info.SystemID {
		return &NameTakenError{Name: name, Theirs: info.SystemID, Mine: mine}
	}
	return nil
}

// NameTakenError: the backup name's folder holds another cluster's backups.
type NameTakenError struct{ Name, Theirs, Mine string }

func (e *NameTakenError) Error() string {
	return fmt.Sprintf("backup name %q is already used in the bucket by a different PostgreSQL server "+
		"(its system identifier is %s, this server's is %s). Nothing was changed. Choose a new backup name; "+
		"each server needs its own. To restore that server's backups onto this one, see docs/restore.md section 3",
		e.Name, e.Theirs, e.Mine)
}

// RestartPostgres restarts the cluster (a few seconds of downtime) and waits until
// it accepts connections again.
func RestartPostgres(ctx context.Context, e *pgbackrest.Engine) error {
	out, err := e.Cluster.Run.Run(ctx, e.Cluster.RestartCmd())
	if err != nil {
		return fmt.Errorf("restart PostgreSQL: %w\n%s", err, out)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := e.Cluster.Setting(ctx, "archive_mode"); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("PostgreSQL didn't accept connections within 2 minutes of the restart")
}

// StepError is a failed pgBackRest step, with its output and a plain-language reason.
type StepError struct {
	Step   string
	Output string
}

func (e *StepError) Error() string {
	return fmt.Sprintf("pgbackrest %s failed: %s", e.Step, notify.LikelyReason(e.Output))
}

// CreateStanza runs stanza-create, then check, which archives a WAL segment end to end.
func CreateStanza(ctx context.Context, e *pgbackrest.Engine) error {
	for _, step := range []struct {
		name string
		fn   func(context.Context) ([]byte, error)
	}{
		{"stanza-create", e.StanzaCreate},
		{"check", e.Check},
	} {
		if out, err := step.fn(ctx); err != nil {
			if len(out) == 0 {
				out = []byte(err.Error())
			}
			return &StepError{Step: step.name, Output: string(out)}
		}
	}
	return nil
}
