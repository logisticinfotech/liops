// SPDX-License-Identifier: Apache-2.0

// Package pgdump is layer 2: a nightly pg_dump of every database, streamed straight
// to storage with no file on the server's disk.
package pgdump

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// keyTime is the timestamp format in object keys. It sorts chronologically.
const keyTime = "2006-01-02T150405Z"

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
// The name is path-escaped, so a "/" in a database name can't change the layout.
func Key(db string, t time.Time) string {
	return Prefix(db) + t.UTC().Format(keyTime) + ".dump"
}

// Prefix is where the dumps of db are stored: dumps/<db>/.
func Prefix(db string) string {
	return storage.DumpsPrefix + url.PathEscape(db) + "/"
}

// ParseKey returns the database and time encoded in a dump key.
func ParseKey(key string) (db string, t time.Time, ok bool) {
	rest, found := strings.CutPrefix(key, storage.DumpsPrefix)
	if !found {
		return "", time.Time{}, false
	}
	escaped, file, found := strings.Cut(rest, "/")
	if !found || strings.Contains(file, "/") {
		return "", time.Time{}, false
	}
	db, err := url.PathUnescape(escaped)
	if err != nil {
		return "", time.Time{}, false
	}
	t, err = time.Parse(keyTime, strings.TrimSuffix(file, ".dump"))
	if err != nil {
		return "", time.Time{}, false
	}
	return db, t, true
}

// GlobalsKey is the object key of the roles and tablespaces dump taken at t.
func GlobalsKey(t time.Time) string {
	return storage.GlobalsPrefix + t.UTC().Format(keyTime) + ".sql"
}

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
	if e.Cfg.Labels.SQL == "" {
		return nil, nil
	}
	rows, err := e.Cluster.Query(ctx, e.Cfg.Labels.Database, e.Cfg.Labels.SQL)
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
	key = GlobalsKey(at)
	size, err = e.stream(ctx, e.Cluster.Command("pg_dumpall", "--globals-only"), key)
	return key, size, err
}

func (e *Engine) stream(ctx context.Context, cmd command.Cmd, key string) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	dumped := make(chan error, 1)
	go func() {
		err := e.Cluster.Run.Stream(ctx, cmd, pw)
		// A failed dump fails the upload's read, so the partial upload is aborted.
		pw.CloseWithError(err)
		dumped <- err
	}()

	size, putErr := e.Store.Put(ctx, key, pr)
	if putErr != nil {
		cancel()                  // stop pg_dump
		pr.CloseWithError(putErr) // unblock it if it is waiting to write
	}
	if dumpErr := <-dumped; dumpErr != nil {
		if putErr == nil {
			// Shouldn't happen, but never keep an object from a failed dump.
			_ = e.Store.Delete(context.WithoutCancel(ctx), key)
		}
		return 0, dumpErr
	}
	if putErr != nil {
		return 0, putErr
	}
	return size, nil
}
