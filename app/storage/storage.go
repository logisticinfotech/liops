// SPDX-License-Identifier: Apache-2.0

// Package storage is the object storage both backup layers go to. pgBackRest talks
// to the bucket itself; pgtijori uses this package for the per-database dumps, the
// storage probe, usage totals and download links.
package storage

import (
	"context"
	"io"
	"strings"
	"time"
)

// Object is one stored object.
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Store is an object store. Put streams: it never needs the whole object in memory
// or on disk.
type Store interface {
	Put(ctx context.Context, key string, r io.Reader) (size int64, err error)
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
	DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Probe writes, reads back and deletes a small test object.
	Probe(ctx context.Context) error
}

// Bucket layout (object key prefixes). pgBackRest owns repo1-path /pgbackrest and
// creates backup/<stanza>/ and archive/<stanza>/ under it. Layer-2 dumps and
// globals live inside that same stanza backup folder:
//
//	pgbackrest/backup/<stanza>/…          ← pgBackRest
//	pgbackrest/backup/<stanza>/dumps/…    ← per-database dumps
//	pgbackrest/backup/<stanza>/globals/…  ← roles / tablespaces
//
// LegacyDumpsPrefix / LegacyGlobalsPrefix are the pre-stanza layout; still listed
// for download/retention so existing objects are not abandoned.
const (
	PgBackRestPrefix    = "pgbackrest/"
	LegacyDumpsPrefix   = "dumps/"
	LegacyGlobalsPrefix = "globals/"
	probePrefix         = "pgtijori-probe/"
)

// StanzaBackupPrefix is pgbackrest/backup/<stanza>/.
func StanzaBackupPrefix(stanza string) string {
	return PgBackRestPrefix + "backup/" + stanza + "/"
}

// DumpsPrefix is pgbackrest/backup/<stanza>/dumps/.
func DumpsPrefix(stanza string) string {
	return StanzaBackupPrefix(stanza) + "dumps/"
}

// GlobalsPrefix is pgbackrest/backup/<stanza>/globals/.
func GlobalsPrefix(stanza string) string {
	return StanzaBackupPrefix(stanza) + "globals/"
}

// IsGlobalsKey reports whether key is a globals object for this stanza or the legacy layout.
func IsGlobalsKey(key, stanza string) bool {
	return strings.HasPrefix(key, GlobalsPrefix(stanza)) || strings.HasPrefix(key, LegacyGlobalsPrefix)
}

// IsLayer2Key reports whether key is a dump or globals object (current or legacy layout).
func IsLayer2Key(key, stanza string) bool {
	if IsGlobalsKey(key, stanza) {
		return true
	}
	// Dump keys: validated by callers with pgdump.ParseKey; here accept prefixes only.
	return strings.HasPrefix(key, DumpsPrefix(stanza)) || strings.HasPrefix(key, LegacyDumpsPrefix)
}

// ListDumps lists dump objects under the stanza path and the legacy dumps/ prefix.
func ListDumps(ctx context.Context, store Store, stanza string) ([]Object, error) {
	return listTwo(ctx, store, DumpsPrefix(stanza), LegacyDumpsPrefix)
}

// ListGlobals lists globals objects under the stanza path and the legacy globals/ prefix.
func ListGlobals(ctx context.Context, store Store, stanza string) ([]Object, error) {
	return listTwo(ctx, store, GlobalsPrefix(stanza), LegacyGlobalsPrefix)
}

func listTwo(ctx context.Context, store Store, a, b string) ([]Object, error) {
	first, err1 := store.List(ctx, a)
	second, err2 := store.List(ctx, b)
	if err1 != nil {
		return nil, err1
	}
	if err2 != nil {
		return first, err2
	}
	return append(first, second...), nil
}

// TotalSize adds up the size of objects.
func TotalSize(objs []Object) int64 {
	var n int64
	for _, o := range objs {
		n += o.Size
	}
	return n
}
