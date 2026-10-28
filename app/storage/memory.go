// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memObject
	// Now sets the Modified time of new objects; time.Now when nil.
	Now func() time.Time
	// FailPut, when set, makes Put fail for keys with this prefix.
	FailPut string
}

type memObject struct {
	data     []byte
	modified time.Time
}

func NewMemory() *Memory { return &Memory{objects: map[string]memObject{}} }

func (m *Memory) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailPut != "" && strings.HasPrefix(key, m.FailPut) {
		return 0, errors.New("upload refused (test)")
	}
	now := time.Now()
	if m.Now != nil {
		now = m.Now()
	}
	m.objects[key] = memObject{data: data, modified: now}
	return int64(len(data)), nil
}

// Add stores an object with a given modification time.
func (m *Memory) Add(key string, data []byte, modified time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, modified: modified}
}

// Get returns an object's content.
func (m *Memory) Get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return o.data, ok
}

func (m *Memory) Read(ctx context.Context, key string) (io.ReadCloser, error) {
	data, ok := m.Get(key)
	if !ok {
		return nil, fmt.Errorf("read %s: The specified key does not exist.", key)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *Memory) List(ctx context.Context, prefix string) ([]Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var objs []Object
	for k, o := range m.objects {
		if strings.HasPrefix(k, prefix) {
			objs = append(objs, Object{Key: k, Size: int64(len(o.data)), Modified: o.modified})
		}
	}
	sort.Slice(objs, func(i, j int) bool { return objs[i].Key < objs[j].Key })
	return objs, nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *Memory) DownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return "https://storage.example.com/" + key, nil
}

func (m *Memory) Probe(ctx context.Context) error { return nil }
