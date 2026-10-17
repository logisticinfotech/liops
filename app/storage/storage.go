// SPDX-License-Identifier: Apache-2.0

// Package storage is the object storage both backup layers go to. pgBackRest talks
// to the bucket itself; pgtijori uses this package for the per-database dumps, the
// storage probe, usage totals and download links.
package storage

import (
	"context"
	"io"
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
	List(ctx context.Context, prefix string) ([]Object, error)
	Delete(ctx context.Context, key string) error
	DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	// Probe writes, reads back and deletes a small test object.
	Probe(ctx context.Context) error
}

// Prefixes for the two layers inside the bucket.
const (
	PgBackRestPrefix = "pgbackrest/"
	DumpsPrefix      = "dumps/"
	GlobalsPrefix    = "globals/"
	probePrefix      = "pgtijori-probe/"
)

// TotalSize adds up the size of objects.
func TotalSize(objs []Object) int64 {
	var n int64
	for _, o := range objs {
		n += o.Size
	}
	return n
}
