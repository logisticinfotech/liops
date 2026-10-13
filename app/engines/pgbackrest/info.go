// SPDX-License-Identifier: Apache-2.0

package pgbackrest

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Info is what `pgbackrest info` reports for one stanza.
type Info struct {
	Stanza  string
	Status  string // "ok", or pgBackRest's message, e.g. "no valid backups"
	OK      bool
	Backups []Backup // oldest first
	// WAL range in the repository, as segment names.
	WALMin, WALMax string
}

// Backup is one full or differential backup.
type Backup struct {
	Label    string
	Type     string // full, diff, incr
	Start    time.Time
	Stop     time.Time
	Size     int64 // database size
	RepoSize int64 // stored (compressed) size of this backup's own files
	Error    bool  // pgBackRest found checksum errors while copying
}

// Duration is how long the backup took.
func (b Backup) Duration() time.Duration { return b.Stop.Sub(b.Start) }

// Last returns the newest backup of type typ ("" for any type), or nil.
func (i *Info) Last(typ string) *Backup {
	for n := len(i.Backups) - 1; n >= 0; n-- {
		if typ == "" || i.Backups[n].Type == typ {
			return &i.Backups[n]
		}
	}
	return nil
}

// RestoreFrom is the earliest moment the cluster can be restored to: the end of the
// oldest backup still in the repository. Zero if there are no backups.
func (i *Info) RestoreFrom() time.Time {
	if len(i.Backups) == 0 {
		return time.Time{}
	}
	return i.Backups[0].Stop
}

type rawStanza struct {
	Name   string `json:"name"`
	Status struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
	Archive []struct {
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"archive"`
	Backup []struct {
		Label     string `json:"label"`
		Type      string `json:"type"`
		Error     bool   `json:"error"`
		Timestamp struct {
			Start int64 `json:"start"`
			Stop  int64 `json:"stop"`
		} `json:"timestamp"`
		Info struct {
			Size       int64 `json:"size"`
			Repository struct {
				Delta int64 `json:"delta"`
			} `json:"repository"`
		} `json:"info"`
	} `json:"backup"`
}

// ParseInfo parses `pgbackrest --output=json info` output and returns the named stanza.
func ParseInfo(data []byte, stanza string) (*Info, error) {
	var raw []rawStanza
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("pgbackrest info: %w", err)
	}
	for _, s := range raw {
		if s.Name != stanza {
			continue
		}
		info := &Info{Stanza: s.Name, Status: s.Status.Message, OK: s.Status.Code == 0}
		for _, b := range s.Backup {
			info.Backups = append(info.Backups, Backup{
				Label:    b.Label,
				Type:     b.Type,
				Start:    time.Unix(b.Timestamp.Start, 0),
				Stop:     time.Unix(b.Timestamp.Stop, 0),
				Size:     b.Info.Size,
				RepoSize: b.Info.Repository.Delta,
				Error:    b.Error,
			})
		}
		sort.SliceStable(info.Backups, func(i, j int) bool { return info.Backups[i].Start.Before(info.Backups[j].Start) })
		for _, a := range s.Archive {
			if info.WALMin == "" || a.Min < info.WALMin {
				info.WALMin = a.Min
			}
			if a.Max > info.WALMax {
				info.WALMax = a.Max
			}
		}
		return info, nil
	}
	return nil, fmt.Errorf("pgbackrest info: stanza %q not found", stanza)
}
