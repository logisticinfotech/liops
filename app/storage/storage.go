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

	"github.com/logisticinfotech/pgtijori/config"
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

// Prefixes for the two layers inside the server's folder (see Folder).
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

// Folder is the server's folder in the bucket, named after its backup name
// (pgbackrest.stanza), e.g. "lmf-prod-1/". Everything the server stores is under it,
// so servers sharing a bucket, or a replacement server set up under a new name,
// never touch each other's backups.
func Folder(cfg *config.Config) string { return cfg.PgBackRest.Stanza + "/" }

// Open connects to the bucket and returns the server's folder in it. Keys passed to
// the returned Store are relative to the folder ("dumps/app/….dump").
func Open(cfg *config.Config) (Store, error) {
	s3, err := NewS3(cfg.Storage.S3)
	if err != nil {
		return nil, err
	}
	return Prefixed{Store: s3, Prefix: Folder(cfg)}, nil
}

// Prefixed is a Store limited to the keys under Prefix.
type Prefixed struct {
	Store  Store
	Prefix string
}

func (p Prefixed) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	return p.Store.Put(ctx, p.Prefix+key, r)
}

// List returns keys relative to Prefix.
func (p Prefixed) List(ctx context.Context, prefix string) ([]Object, error) {
	objs, err := p.Store.List(ctx, p.Prefix+prefix)
	for i := range objs {
		objs[i].Key = strings.TrimPrefix(objs[i].Key, p.Prefix)
	}
	return objs, err
}

func (p Prefixed) Delete(ctx context.Context, key string) error {
	return p.Store.Delete(ctx, p.Prefix+key)
}

func (p Prefixed) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return p.Store.DownloadURL(ctx, p.Prefix+key, ttl)
}

func (p Prefixed) Probe(ctx context.Context) error { return p.Store.Probe(ctx) }
