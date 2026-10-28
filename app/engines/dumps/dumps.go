// SPDX-License-Identifier: Apache-2.0

// Package dumps holds what layer 2 does the same way for every database engine: the
// object keys of dumps, streaming a dump program's output to storage, and the
// retention rules. The engines (pgdump) decide what to run.
package dumps

import (
	"context"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/storage"
)

// keyTime is the timestamp format in object keys. It sorts chronologically.
const keyTime = "2006-01-02T150405Z"

// Key is the object key of a dump of db taken at t: dumps/<db>/<UTC time><ext>, where
// ext is the engine's file extension (".dump" for pg_dump). The name is path-escaped,
// so a "/" in a database name can't change the layout.
func Key(db string, t time.Time, ext string) string {
	return Prefix(db) + t.UTC().Format(keyTime) + ext
}

// Prefix is where the dumps of db are stored: dumps/<db>/.
func Prefix(db string) string {
	return storage.DumpsPrefix + url.PathEscape(db) + "/"
}

// ParseKey returns the database and time encoded in a dump key, whatever its extension.
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
	stamp, _, _ := strings.Cut(file, ".")
	t, err = time.Parse(keyTime, stamp)
	if err != nil {
		return "", time.Time{}, false
	}
	return db, t, true
}

// GlobalsKey is the object key of the server-wide dump (roles, users, grants) taken at t.
func GlobalsKey(t time.Time) string {
	return storage.GlobalsPrefix + t.UTC().Format(keyTime) + ".sql"
}

// Stream runs cmd and uploads its stdout to key. Nothing is written to local disk.
// If the command or the upload fails, no object is left behind.
func Stream(ctx context.Context, run command.Runner, cmd command.Cmd, store storage.Store, key string) (int64, error) {
	return Upload(ctx, store, key, func(ctx context.Context, w io.Writer) error { return run.Stream(ctx, cmd, w) })
}

// Upload uploads what write writes to key, as it is written. If write or the upload
// fails, no object is left behind. write must stop when ctx is cancelled.
func Upload(ctx context.Context, store storage.Store, key string, write func(ctx context.Context, w io.Writer) error) (int64, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	dumped := make(chan error, 1)
	go func() {
		err := write(ctx, pw)
		// A failed dump fails the upload's read, so the partial upload is aborted.
		pw.CloseWithError(err)
		dumped <- err
	}()

	size, putErr := store.Put(ctx, key, pr)
	if putErr != nil {
		cancel()                  // stop the dump program
		pr.CloseWithError(putErr) // unblock it if it is waiting to write
	}
	if dumpErr := <-dumped; dumpErr != nil {
		if putErr == nil {
			// Shouldn't happen, but never keep an object from a failed dump.
			_ = store.Delete(context.WithoutCancel(ctx), key)
		}
		return 0, dumpErr
	}
	if putErr != nil {
		return 0, putErr
	}
	return size, nil
}
