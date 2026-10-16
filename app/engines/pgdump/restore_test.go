// SPDX-License-Identifier: Apache-2.0

package pgdump

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/logisticinfotech/pgtijori/app/command"
	"github.com/logisticinfotech/pgtijori/app/storage"
	"github.com/logisticinfotech/pgtijori/config"
)

func TestRestoreHappyPathRenamesOld(t *testing.T) {
	store := storage.NewMemory()
	at := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC)
	key := Key("main", "vn_rental", at)
	store.Add(key, []byte("PGDMP-bytes"), at)

	var programs []string
	fake := &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		programs = append(programs, program)
		args := strings.Join(c.Args, " ")
		switch program {
		case "chown":
			if len(c.Args) != 2 || c.Args[0] != "postgres:" || !strings.HasSuffix(c.Args[1], ".dump") {
				t.Errorf("chown args: %v", c.Args)
			}
		case "psql":
			if strings.Contains(args, "pg_database WHERE datname") {
				if strings.Contains(args, "datname = 'vn_rental'") {
					return []byte("1\n"), nil // live db exists
				}
				return nil, nil // _old does not exist
			}
			if strings.Contains(args, "ALTER DATABASE") && strings.Contains(args, "vn_rental_old") {
				return nil, nil
			}
			if strings.Contains(args, "pg_terminate_backend") {
				return nil, nil
			}
		case "createdb":
			if !strings.Contains(args, "vn_rental") {
				t.Errorf("createdb args: %v", c.Args)
			}
		case "pg_restore":
			joined := strings.Join(c.Args, " ")
			if !strings.Contains(joined, "--exit-on-error") || !strings.Contains(joined, "dbname='vn_rental'") {
				t.Errorf("pg_restore args: %v", c.Args)
			}
			if filepath.Ext(c.Args[len(c.Args)-1]) != ".dump" {
				t.Errorf("pg_restore should end with the dump path: %v", c.Args)
			}
		case "dropdb":
			t.Errorf("successful restore must not dropdb: %v", c.Args)
		}
		return nil, nil
	}}
	e := New(config.Default(), fake, store)
	var steps []int
	keptAs, err := e.Restore(context.Background(), "vn_rental", key, func(step int, _ string) { steps = append(steps, step) })
	if err != nil {
		t.Fatal(err)
	}
	if keptAs != "vn_rental_old" {
		t.Errorf("keptAs = %q", keptAs)
	}
	if len(steps) != 4 || steps[0] != 1 || steps[3] != 4 {
		t.Errorf("steps = %v", steps)
	}
	joined := strings.Join(programs, ",")
	if !strings.Contains(joined, "chown") || !strings.Contains(joined, "psql") ||
		!strings.Contains(joined, "createdb") || !strings.Contains(joined, "pg_restore") {
		t.Errorf("programs = %v", programs)
	}
	if strings.Contains(joined, "dropdb") {
		t.Errorf("must not drop on success: %v", programs)
	}
}

func TestRestoreWhenDBMissingSkipsRename(t *testing.T) {
	store := storage.NewMemory()
	key := Key("main", "app", time.Now())
	store.Add(key, []byte("x"), time.Now())
	fake := &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		if program == "psql" && strings.Contains(strings.Join(c.Args, " "), "pg_database WHERE datname") {
			return nil, nil // does not exist
		}
		return nil, nil
	}}
	keptAs, err := New(config.Default(), fake, store).Restore(context.Background(), "app", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if keptAs != "" {
		t.Errorf("keptAs = %q, want empty", keptAs)
	}
}

func TestRestoreRefusesSystemDB(t *testing.T) {
	store := storage.NewMemory()
	key := Key("main", "postgres", time.Now())
	store.Add(key, []byte("x"), time.Now())
	e := New(config.Default(), &command.Fake{}, store)
	_, err := e.Restore(context.Background(), "postgres", key, nil)
	if err == nil || !strings.Contains(err.Error(), "system database") {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreKeyMustMatchDB(t *testing.T) {
	store := storage.NewMemory()
	key := Key("main", "other", time.Now())
	store.Add(key, []byte("x"), time.Now())
	e := New(config.Default(), &command.Fake{}, store)
	_, err := e.Restore(context.Background(), "vn_rental", key, nil)
	if err == nil || !strings.Contains(err.Error(), "not \"vn_rental\"") {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreMissingKey(t *testing.T) {
	e := New(config.Default(), &command.Fake{}, storage.NewMemory())
	key := Key("main", "vn_rental", time.Now())
	_, err := e.Restore(context.Background(), "vn_rental", key, nil)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreRenameFailureStopsBeforeCreate(t *testing.T) {
	store := storage.NewMemory()
	key := Key("main", "app", time.Now())
	store.Add(key, []byte("x"), time.Now())
	var programs []string
	fake := &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		programs = append(programs, program)
		args := strings.Join(c.Args, " ")
		if program == "psql" && strings.Contains(args, "datname = 'app'") {
			return []byte("1\n"), nil
		}
		if program == "psql" && strings.Contains(args, "ALTER DATABASE") {
			return nil, errors.New("cannot rename")
		}
		return nil, nil
	}}
	_, err := New(config.Default(), fake, store).Restore(context.Background(), "app", key, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot rename") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(strings.Join(programs, ","), "createdb") {
		t.Errorf("must not create after rename failed: %v", programs)
	}
}

func TestRestorePgRestoreFailureRollsBack(t *testing.T) {
	store := storage.NewMemory()
	key := Key("main", "app", time.Now())
	store.Add(key, []byte("x"), time.Now())
	var programs []string
	fake := &command.Fake{Handle: func(program string, c command.Cmd) ([]byte, error) {
		programs = append(programs, program)
		args := strings.Join(c.Args, " ")
		if program == "psql" && strings.Contains(args, "pg_database WHERE datname") {
			if strings.Contains(args, "datname = 'app'") {
				return []byte("1\n"), nil
			}
			return nil, nil
		}
		if program == "pg_restore" {
			return nil, errors.New("could not execute query")
		}
		return nil, nil
	}}
	keptAs, err := New(config.Default(), fake, store).Restore(context.Background(), "app", key, nil)
	if err == nil || !strings.Contains(err.Error(), "previous database restored as app") {
		t.Fatalf("got keptAs=%q err=%v", keptAs, err)
	}
	joined := strings.Join(programs, ",")
	if !strings.Contains(joined, "dropdb") {
		t.Errorf("should drop partial new db: %v", programs)
	}
	// Second ALTER should rename app_old back to app.
	alterCount := 0
	for _, p := range fake.Calls {
		if filepath.Base(p.Name) == "psql" && strings.Contains(strings.Join(p.Args, " "), "ALTER DATABASE") {
			alterCount++
		}
	}
	if alterCount < 2 {
		t.Errorf("expected rename out and rename back, got %d ALTER calls", alterCount)
	}
}
