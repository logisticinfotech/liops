// SPDX-License-Identifier: Apache-2.0

// Package xtrabackup is layer 1 for MySQL: full and differential base backups taken
// with Percona XtraBackup and streamed straight to storage. (Binlogs, shipped between
// backups, make the moments in between restorable.)
//
// Each backup is a folder xtrabackup/base/<UTC time>-<type>/ holding backup.xbstream
// (XtraBackup's stream, zstd-compressed) and backup.json (its manifest). The manifest is
// written last, so a folder without one is a failed attempt and is never used.
package xtrabackup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/dumps"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/config"
)

// Prefix is where base backups are stored.
const Prefix = "xtrabackup/base/"

// Backup types.
const (
	Full = "full"
	Diff = "diff"
)

// Object names inside a backup's folder.
const (
	StreamFile   = "backup.xbstream"
	ManifestFile = "backup.json"
)

// labelTime is the timestamp format in backup labels. It sorts chronologically.
const labelTime = "2006-01-02T150405Z"

// Manifest describes one complete base backup.
type Manifest struct {
	Label string    `json:"label"` // its folder name, e.g. 2026-10-25T013000Z-full
	Type  string    `json:"type"`  // Full or Diff
	Start time.Time `json:"start"`
	Stop  time.Time `json:"stop"`
	// Base is the label of the full backup a diff is taken against.
	Base    string `json:"base,omitempty"`
	FromLSN uint64 `json:"from_lsn"`
	ToLSN   uint64 `json:"to_lsn"`
	// The binlog position the backup is consistent with: a restore replays binlogs from here.
	BinlogFile    string `json:"binlog_file"`
	BinlogPos     uint64 `json:"binlog_pos"`
	GTIDs         string `json:"gtids,omitempty"`
	ServerVersion string `json:"server_version"`
	ToolVersion   string `json:"xtrabackup_version"`
	Size          int64  `json:"size"` // bytes stored
}

// Duration is how long the backup took.
func (m Manifest) Duration() time.Duration { return m.Stop.Sub(m.Start) }

// Key returns the object key of a file in the backup's folder.
func (m Manifest) Key(file string) string { return Prefix + m.Label + "/" + file }

// Engine takes base backups of one MySQL server into a Store.
type Engine struct {
	Instance mysql.Instance
	Store    storage.Store
	// WorkDir holds XtraBackup's small metadata files while a backup runs.
	WorkDir string
	Now     func() time.Time
}

func New(cfg *config.Config, run command.Runner, store storage.Store, workDir string) *Engine {
	return &Engine{Instance: mysql.Instance{Cfg: cfg.MySQL, Run: run}, Store: store, WorkDir: workDir, Now: time.Now}
}

// Backups returns every complete backup, oldest first.
func (e *Engine) Backups(ctx context.Context) ([]Manifest, error) {
	objs, err := e.Store.List(ctx, Prefix)
	if err != nil {
		return nil, err
	}
	var list []Manifest
	for _, o := range objs {
		if path.Base(o.Key) != ManifestFile {
			continue
		}
		m, err := e.readManifest(ctx, o.Key)
		if err != nil {
			return nil, err
		}
		list = append(list, m)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Start.Before(list[j].Start) })
	return list, nil
}

func (e *Engine) readManifest(ctx context.Context, key string) (Manifest, error) {
	var m Manifest
	r, err := e.Store.Read(ctx, key)
	if err != nil {
		return m, err
	}
	defer r.Close()
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&m); err != nil {
		return m, fmt.Errorf("read %s: %w", key, err)
	}
	return m, nil
}

// Last returns the newest backup of a type ("" for any), or nil.
func Last(list []Manifest, typ string) *Manifest {
	for i := len(list) - 1; i >= 0; i-- {
		if typ == "" || list[i].Type == typ {
			return &list[i]
		}
	}
	return nil
}

// Backup takes a full or differential backup and streams it to storage. A diff is
// taken against the newest full backup; with no full backup yet, it is taken as a full
// one (the returned manifest says which).
func (e *Engine) Backup(ctx context.Context, typ string) (Manifest, error) {
	start := e.Now().UTC()
	var base *Manifest
	if typ == Diff {
		list, err := e.Backups(ctx)
		if err != nil {
			return Manifest{}, fmt.Errorf("find the full backup to take the diff against: %w", err)
		}
		if base = Last(list, Full); base == nil {
			typ = Full
		}
	}
	m := Manifest{Label: start.Format(labelTime) + "-" + typ, Type: typ, Start: start}

	if err := os.MkdirAll(e.WorkDir, 0o700); err != nil {
		return m, err
	}
	dir, err := os.MkdirTemp(e.WorkDir, "backup-")
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(dir)

	args := []string{"--backup", "--stream=xbstream", "--compress=zstd", "--parallel=4",
		"--target-dir=" + dir, "--extra-lsndir=" + dir}
	if base != nil {
		m.Base = base.Label
		args = append(args, "--incremental-lsn="+strconv.FormatUint(base.ToLSN, 10))
	}
	size, err := dumps.Stream(ctx, e.Instance.Run, e.Instance.Command("xtrabackup", args...), e.Store, m.Key(StreamFile))
	if err != nil {
		return m, err
	}
	m.Size = size
	m.Stop = e.Now().UTC()
	if err := readMetadata(dir, &m); err != nil {
		// The stream is useless without its positions: don't leave it looking like a backup.
		_ = e.Store.Delete(context.WithoutCancel(ctx), m.Key(StreamFile))
		return m, err
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	if _, err := e.Store.Put(ctx, m.Key(ManifestFile), strings.NewReader(string(data)+"\n")); err != nil {
		return m, fmt.Errorf("store the manifest of %s: %w", m.Label, err)
	}
	return m, nil
}

// binlogPosRE reads xtrabackup_info's binlog_pos line. The GTID set may span lines.
var binlogPosRE = regexp.MustCompile(`(?s)binlog_pos = filename '([^']+)', position '(\d+)'(?:, GTID of the last change '([^']*)')?`)

// readMetadata reads the LSNs and binlog position XtraBackup wrote with --extra-lsndir.
func readMetadata(dir string, m *Manifest) error {
	checkpoints, err := os.ReadFile(dir + "/xtrabackup_checkpoints")
	if err != nil {
		return fmt.Errorf("XtraBackup didn't write its checkpoints: %w", err)
	}
	kv := keyValues(string(checkpoints))
	if m.FromLSN, err = strconv.ParseUint(kv["from_lsn"], 10, 64); err != nil {
		return fmt.Errorf("xtrabackup_checkpoints: from_lsn %q", kv["from_lsn"])
	}
	if m.ToLSN, err = strconv.ParseUint(kv["to_lsn"], 10, 64); err != nil {
		return fmt.Errorf("xtrabackup_checkpoints: to_lsn %q", kv["to_lsn"])
	}

	info, err := os.ReadFile(dir + "/xtrabackup_info")
	if err != nil {
		return fmt.Errorf("XtraBackup didn't write its info file: %w", err)
	}
	kv = keyValues(string(info))
	m.ServerVersion, m.ToolVersion = kv["server_version"], kv["tool_version"]
	pos := binlogPosRE.FindStringSubmatch(string(info))
	if pos == nil {
		return fmt.Errorf("xtrabackup_info has no binlog position: is binary logging on?")
	}
	m.BinlogFile = pos[1]
	m.BinlogPos, _ = strconv.ParseUint(pos[2], 10, 64)
	m.GTIDs = strings.Join(strings.Fields(pos[3]), "")
	return nil
}

// keyValues reads "key = value" lines.
func keyValues(s string) map[string]string {
	kv := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, " = "); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv
}
