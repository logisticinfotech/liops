// SPDX-License-Identifier: Apache-2.0

// Package storage is the object storage both backup layers go to. pgBackRest talks
// to the bucket itself; liops uses this package for the per-database dumps, the
// storage probe, usage totals and download links.
package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/config"
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
	// Read streams an object's content. The caller closes it.
	Read(ctx context.Context, key string) (io.ReadCloser, error)
	List(ctx context.Context, prefix string) ([]Object, error)
	// Any reports whether there is at least one object under prefix. It stops at the
	// first, so it's cheap even for a folder with thousands of WAL files.
	Any(ctx context.Context, prefix string) (bool, error)
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
	ProbePrefix      = "liops-probe/"
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
// (cfg.BackupName), e.g. "lmf-prod-1/". Everything the server stores is under it,
// so servers sharing a bucket, or a replacement server set up under a new name,
// never touch each other's backups.
func Folder(cfg *config.Config) string { return cfg.BackupName() + "/" }

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

func (p Prefixed) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	return p.Store.Read(ctx, p.Prefix+key)
}

// List returns keys relative to Prefix.
func (p Prefixed) List(ctx context.Context, prefix string) ([]Object, error) {
	objs, err := p.Store.List(ctx, p.Prefix+prefix)
	for i := range objs {
		objs[i].Key = strings.TrimPrefix(objs[i].Key, p.Prefix)
	}
	return objs, err
}

func (p Prefixed) Any(ctx context.Context, prefix string) (bool, error) {
	return p.Store.Any(ctx, p.Prefix+prefix)
}

func (p Prefixed) Delete(ctx context.Context, key string) error {
	return p.Store.Delete(ctx, p.Prefix+key)
}

func (p Prefixed) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return p.Store.DownloadURL(ctx, p.Prefix+key, ttl)
}

func (p Prefixed) Probe(ctx context.Context) error { return ProbeStore(ctx, p) }

// ProbeStore writes, reads back and deletes a small test object on s. Prefixed stores
// keep the object inside the server's folder; a leftover from a failed read is still
// removed.
func ProbeStore(ctx context.Context, s Store) (err error) {
	key := ProbePrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	want := []byte("liops storage probe\n")
	if _, err := s.Put(ctx, key, bytes.NewReader(want)); err != nil {
		return fmt.Errorf("write test object: %w", err)
	}
	defer func() {
		if derr := s.Delete(ctx, key); derr != nil && err == nil {
			err = fmt.Errorf("delete test object: %w", derr)
		}
	}()
	r, err := s.Read(ctx, key)
	if err != nil {
		return fmt.Errorf("read test object: %w", err)
	}
	got, err := io.ReadAll(r)
	r.Close()
	if err != nil {
		return fmt.Errorf("read test object: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("read test object: content doesn't match what was written")
	}
	return nil
}
