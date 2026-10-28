// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/liops/config"
)

func TestPrefixedKeepsToItsFolder(t *testing.T) {
	ctx := context.Background()
	mem := NewMemory()
	mem.Add("other-server/dumps/app/old.dump", []byte("theirs"), time.Now())

	cfg := config.Default()
	cfg.PgBackRest.Stanza = "app-prod-1"
	if Folder(cfg) != "app-prod-1/" {
		t.Fatalf("Folder = %q", Folder(cfg))
	}
	p := Prefixed{Store: mem, Prefix: Folder(cfg)}
	if _, err := p.Put(ctx, "dumps/app/new.dump", strings.NewReader("ours")); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.Get("app-prod-1/dumps/app/new.dump"); !ok {
		t.Error("Put should write under the folder")
	}
	objs, err := p.List(ctx, "dumps/")
	if err != nil || len(objs) != 1 || objs[0].Key != "dumps/app/new.dump" {
		t.Errorf("List should only see this folder, with relative keys: %+v, %v", objs, err)
	}
	if err := p.Delete(ctx, "dumps/app/new.dump"); err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.Get("other-server/dumps/app/old.dump"); !ok {
		t.Error("another server's folder must be untouched")
	}
}
