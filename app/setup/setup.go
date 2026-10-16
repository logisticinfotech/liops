// SPDX-License-Identifier: Apache-2.0

// Package setup holds the steps that connect pgtijori to a cluster: find the cluster,
// write the pgBackRest and PostgreSQL settings, turn on WAL archiving and create the
// stanza. `pgtijori setup` and the setup wizard both use it. Every step is safe to
// run again.
package setup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/engines/pgbackrest"
	"github.com/logisticinfotech/pgtijori/app/notify"
	"github.com/logisticinfotech/pgtijori/config"
)

// Cluster is a PostgreSQL cluster found on this machine (Debian/Ubuntu layout).
type Cluster struct {
	Version int
	Name    string
	Port    int
	Status  string // "online", "down", ...
	Owner   string
	DataDir string
}

// Online reports whether the cluster is running.
func (c Cluster) Online() bool { return strings.HasPrefix(c.Status, "online") }

// DetectClusters lists the clusters with pg_lsclusters.
func DetectClusters(ctx context.Context, run command.Runner) ([]Cluster, error) {
	out, err := run.Run(ctx, command.Cmd{Name: "pg_lsclusters", Args: []string{"--no-header"}})
	if err != nil {
		return nil, fmt.Errorf("pg_lsclusters (is PostgreSQL from the PGDG repository installed?): %w", err)
	}
	return ParseClusters(string(out)), nil
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
		clusters = append(clusters, Cluster{Version: version, Name: f[1], Port: port, Status: f[3], Owner: f[4], DataDir: f[5]})
	}
	return clusters
}

// Apply copies a detected cluster into the config.
func Apply(cfg *config.Config, c Cluster) {
	cfg.Postgres.Version = c.Version
	cfg.Postgres.Cluster = c.Name
	cfg.Postgres.Port = c.Port
	cfg.Postgres.DataDir = c.DataDir
	cfg.Postgres.OSUser = c.Owner
}

// ServiceName is the systemd unit of the cluster, e.g. postgresql@18-main.
func ServiceName(cfg *config.Config) string {
	return fmt.Sprintf("postgresql@%d-%s", cfg.Postgres.Version, cfg.Postgres.Cluster)
}

// RestartCommand is what to type to restart the cluster.
func RestartCommand(cfg *config.Config) string { return "sudo systemctl restart " + ServiceName(cfg) }

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
	changed, err := e.WriteConfigs()
	if err != nil {
		return NeedsRestart, fmt.Errorf("write settings: %w", err)
	}
	if err := e.CheckReadable(ctx); err != nil {
		return NeedsRestart, err
	}
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

// RestartPostgres restarts the cluster (a few seconds of downtime) and waits until
// it accepts connections again.
func RestartPostgres(ctx context.Context, e *pgbackrest.Engine) error {
	out, err := e.Cluster.Run.Run(ctx, command.Cmd{Name: "systemctl", Args: []string{"restart", ServiceName(e.Cfg)}})
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
