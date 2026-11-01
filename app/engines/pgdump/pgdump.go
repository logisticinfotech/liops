// SPDX-License-Identifier: Apache-2.0

// Package pgdump is layer 2: a nightly pg_dump of every database, streamed straight
// to storage with no file on the server's disk.
package pgdump

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/dumps"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// Ext is the file extension of a pg_dump custom-format dump.
const Ext = ".dump"

// Engine dumps the databases of one cluster into a Store.
type Engine struct {
	Cfg     config.Dumps
	Cluster postgres.Cluster
	Store   storage.Store
}

func New(cfg *config.Config, run command.Runner, store storage.Store) *Engine {
	return &Engine{Cfg: cfg.Dumps, Cluster: postgres.Cluster{Cfg: cfg.Postgres, Run: run}, Store: store}
}

// Key is the object key of a dump of db taken at t: dumps/<db>/<UTC time>.dump.
func Key(db string, t time.Time) string { return dumps.Key(db, t, Ext) }

// Discover returns the databases to dump: every database that accepts connections
// and isn't a template, minus the configured exclusions.
func (e *Engine) Discover(ctx context.Context) ([]string, error) {
	// Hex-encoded so that no database name can break the line/column parsing.
	rows, err := e.Cluster.Query(ctx, "postgres",
		"SELECT encode(convert_to(datname, 'UTF8'), 'hex') FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY 1")
	if err != nil {
		return nil, err
	}
	var dbs []string
	for _, row := range rows {
		name, err := hex.DecodeString(row[0])
		if err != nil {
			return nil, fmt.Errorf("discover databases: %w", err)
		}
		if !slices.Contains(e.Cfg.Exclude, string(name)) {
			dbs = append(dbs, string(name))
		}
	}
	return dbs, nil
}

// Labels runs the configured label query and returns database name → label.
// It returns nil when no label query is configured.
func (e *Engine) Labels(ctx context.Context) (map[string]string, error) {
	if !e.Cfg.Labels.Configured() {
		return nil, nil
	}
	sql, err := e.Cfg.Labels.SelectSQL(false)
	if err != nil {
		return nil, fmt.Errorf("label query: %w", err)
	}
	rows, err := e.Cluster.Query(ctx, e.Cfg.Labels.Database, sql)
	if err != nil {
		return nil, fmt.Errorf("label query: %w", err)
	}
	labels := make(map[string]string, len(rows))
	for _, row := range rows {
		if len(row) < 2 {
			return nil, fmt.Errorf("label query: must return two columns (database name, label), got %d", len(row))
		}
		labels[row[0]] = row[1]
	}
	return labels, nil
}

// compression returns pg_dump's -Z value: zstd from PostgreSQL 16, gzip before.
func (e *Engine) compression() string {
	if e.Cluster.Cfg.Version >= 16 {
		return "zstd:3"
	}
	return "6"
}

// Dump streams a custom-format dump of db to storage. Nothing is written to local
// disk. If pg_dump or the upload fails, no object is left behind.
func (e *Engine) Dump(ctx context.Context, db string, at time.Time) (key string, size int64, err error) {
	key = Key(db, at)
	cmd := e.Cluster.Command("pg_dump", "-Fc", "-Z", e.compression(), "-d", postgres.Conn(db))
	size, err = e.stream(ctx, cmd, key)
	return key, size, err
}

// DumpGlobals streams `pg_dumpall --globals-only` (roles, tablespaces) to storage.
// Restoring a single database dump on a fresh server needs its roles first.
func (e *Engine) DumpGlobals(ctx context.Context, at time.Time) (key string, size int64, err error) {
	key = dumps.GlobalsKey(at)
	size, err = e.stream(ctx, e.Cluster.Command("pg_dumpall", "--globals-only"), key)
	return key, size, err
}

func (e *Engine) stream(ctx context.Context, cmd command.Cmd, key string) (int64, error) {
	return dumps.Stream(ctx, e.Cluster.Run, cmd, e.Store, key)
}
