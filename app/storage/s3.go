// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/url"
	"path"
	"strconv"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/logisticinfotech/liops/config"
)

// PartSize is the multipart upload part size for streamed uploads. It bounds memory
// use per upload, and with S3's 10,000-part limit it caps one object at ~156 GiB.
const PartSize = 16 << 20

// S3 is an S3-compatible bucket: Cloudflare R2, AWS S3, Backblaze B2, Wasabi, MinIO.
type S3 struct {
	client *minio.Client
	bucket string
}

// NewS3 connects to the bucket described by cfg. It doesn't make any request.
func NewS3(cfg config.S3) (*S3, error) {
	opts := &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure: true,
		Region: cfg.Region,
	}
	if cfg.PathStyle {
		opts.BucketLookup = minio.BucketLookupPath
	} else {
		opts.BucketLookup = minio.BucketLookupDNS
	}
	if !cfg.VerifyTLS {
		tr, err := minio.DefaultTransport(true)
		if err != nil {
			return nil, err
		}
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} // test setups only; see config.S3.VerifyTLS
		opts.Transport = tr
	}
	client, err := minio.New(cfg.Endpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	return &S3{client: client, bucket: cfg.Bucket}, nil
}

func (s *S3) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	info, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
		PartSize:    PartSize,
	})
	if err != nil {
		return 0, fmt.Errorf("upload %s: %w", key, err)
	}
	return info.Size, nil
}

func (s *S3) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	// GetObject is lazy: Stat makes a missing object an error here, not on the first Read.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		return nil, fmt.Errorf("read %s: %w", key, err)
	}
	return obj, nil
}

func (s *S3) List(ctx context.Context, prefix string) ([]Object, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var objs []Object
	var listErr error
	// The channel must be drained to the end, even after an error, or the lister leaks.
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if listErr != nil {
			continue
		}
		if o.Err != nil {
			listErr = fmt.Errorf("list %s: %w", prefix, o.Err)
			cancel()
			continue
		}
		objs = append(objs, Object{Key: o.Key, Size: o.Size, Modified: o.LastModified})
	}
	return objs, listErr
}

func (s *S3) Any(ctx context.Context, prefix string) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	found := false
	var listErr error
	// Drain the channel after cancelling, or the lister leaks.
	for o := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true, MaxKeys: 1}) {
		if found || listErr != nil {
			continue
		}
		if o.Err != nil {
			listErr = fmt.Errorf("list %s: %w", prefix, o.Err)
		} else {
			found = true
		}
		cancel()
	}
	if found {
		return true, nil
	}
	return false, listErr
}

func (s *S3) Delete(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete %s: %w", key, err)
	}
	return nil
}

func (s *S3) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	params := url.Values{}
	params.Set("response-content-disposition", "attachment; filename="+strconv.Quote(path.Base(key)))
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, ttl, params)
	if err != nil {
		return "", fmt.Errorf("download link for %s: %w", key, err)
	}
	return u.String(), nil
}

func (s *S3) Probe(ctx context.Context) error {
	key := probePrefix + strconv.FormatInt(time.Now().UnixNano(), 36)
	want := []byte("liops storage probe\n")
	if _, err := s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(want), int64(len(want)), minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("write test object: %w", err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("read test object: %w", err)
	}
	got, err := io.ReadAll(obj)
	obj.Close()
	if err != nil {
		return fmt.Errorf("read test object: %w", err)
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("read test object: content doesn't match what was written")
	}
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete test object: %w", err)
	}
	return nil
}
