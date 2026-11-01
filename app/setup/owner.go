// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"strings"

	"github.com/logisticinfotech/liops/app/storage"
)

// OwnersPrefix holds the marker of the server that uses a backup name's folder:
// owners/<engine>-<identity>, e.g. owners/mysql-<server_uuid> or
// owners/postgres-<system identifier>. Setup writes it when it claims the folder.
const OwnersPrefix = "owners/"

// claimFolder makes sure the backup name's folder in the bucket is this server's to
// use, then records this server as its owner. The rule, the same for every engine:
//
//   - empty (a new name): claimed;
//   - holding this server's marker: fine (setup run again);
//   - anything else is refused: another server's marker, or backups and dumps that no
//     marker accounts for.
//
// legacy, when set, gets one chance to recognise a folder without a marker as this
// server's own, from before markers existed (PostgreSQL: a pgBackRest stanza with this
// cluster's system identifier). It returns who the folder belongs to instead, or "" when
// it is this server's.
func claimFolder(ctx context.Context, store storage.Store, name, engine, identity string, legacy func(context.Context) (string, error)) error {
	mine := OwnersPrefix + engine + "-" + identity
	owners, err := store.List(ctx, OwnersPrefix)
	if err != nil {
		return fmt.Errorf("couldn't check whether backup name %q is already used in the bucket: %w", name, err)
	}
	for _, o := range owners {
		if o.Key != mine {
			return &NameTakenError{Name: name, Engine: engine, Owner: describeOwner(strings.TrimPrefix(o.Key, OwnersPrefix))}
		}
	}
	if len(owners) > 0 {
		return nil // ours
	}

	used, err := folderInUse(ctx, store)
	if err != nil {
		return fmt.Errorf("couldn't check whether backup name %q is already used in the bucket: %w", name, err)
	}
	if used {
		owner := "backups or dumps that no server has claimed (from an older liops, or copied in by hand)"
		if legacy != nil {
			if owner, err = legacy(ctx); err != nil {
				return err
			}
		}
		if owner != "" {
			return &NameTakenError{Name: name, Engine: engine, Owner: owner}
		}
	}

	marker := fmt.Sprintf("{\"engine\":%q,\"id\":%q}\n", engine, identity)
	if _, err := store.Put(ctx, mine, strings.NewReader(marker)); err != nil {
		return fmt.Errorf("record this server as the owner of backup name %q: %w", name, err)
	}
	return nil
}

// folderInUse reports whether the server's folder already holds objects, ignoring a
// leftover storage probe (liops-probe/) that would otherwise look like unclaimed backups.
func folderInUse(ctx context.Context, store storage.Store) (bool, error) {
	objs, err := store.List(ctx, "")
	if err != nil {
		return false, err
	}
	for _, o := range objs {
		if !strings.HasPrefix(o.Key, storage.ProbePrefix) {
			return true, nil
		}
	}
	return false, nil
}

// describeOwner turns a marker name (mysql-<uuid>) into words for an error message.
func describeOwner(marker string) string {
	engine, id, _ := strings.Cut(marker, "-")
	switch engine {
	case "mysql":
		return "another MySQL server (server_uuid " + id + ")"
	case "postgres":
		return "another PostgreSQL server (system identifier " + id + ")"
	}
	return "another server (" + marker + ")"
}

// NameTakenError: the backup name's folder belongs to another server, or holds backups
// no server has claimed. Nothing has been changed when it is returned.
type NameTakenError struct {
	Name   string
	Engine string // this server's engine: which setting and runbook to point to
	Owner  string // who the folder belongs to, in words
}

func (e *NameTakenError) Error() string {
	field, runbook := "pgbackrest.stanza", "docs/restore.md section 3"
	if e.Engine == "mysql" {
		field, runbook = "mysql.backup_name", "docs/restore-mysql.md section 3"
	}
	return fmt.Sprintf("backup name %q is already used in the bucket by %s. Nothing was changed. "+
		"Choose a new backup name (%s); each server needs its own. To restore those backups onto this server, see %s",
		e.Name, e.Owner, field, runbook)
}
