// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"fmt"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
)

// Summary is the content of the daily summary email.
type Summary struct {
	Server string
	Now    time.Time // in the schedule timezone

	// Problems, in plain language. None means healthy.
	Problems []string

	// PointInTime is false for a server without point-in-time backups yet (MySQL,
	// until they are built): the summary then leaves them out.
	PointInTime        bool
	LastBackupType     string // "full" or "diff"; empty if there is none
	LastBackupAt       time.Time
	LastBackupDuration time.Duration
	RestoreFrom        time.Time
	LastWAL            time.Time // newest WAL segment archived

	DumpsEnabled bool
	DumpsOK      int
	DumpsTotal   int
	DumpsSize    int64

	StorageUsed int64
	DiskUsedPct int // -1 if unknown

	NextFull time.Time
}

// DailySummary returns the subject and body of the daily summary.
func DailySummary(s Summary) (subject, body string) {
	day := s.Now.Format("Mon 2 Jan")
	var b strings.Builder
	if len(s.Problems) == 0 {
		subject = fmt.Sprintf("✅ %s — backups healthy — %s", s.Server, day)
		b.WriteString("All good. No action needed.\n\n")
	} else {
		subject = fmt.Sprintf("⚠️ %s — backups need attention — %s", s.Server, day)
		b.WriteString("Needs attention:\n")
		for _, p := range s.Problems {
			b.WriteString("  - " + p + "\n")
		}
		b.WriteString("\n")
	}

	row := func(label, value string) { fmt.Fprintf(&b, "%-22s %s\n", label+":", value) }
	if s.PointInTime {
		if s.LastBackupType == "" {
			row("Point-in-time backup", "none yet")
		} else {
			v := fmt.Sprintf("last %s %s (%s)", s.LastBackupType, When(s.LastBackupAt, s.Now), Duration(s.LastBackupDuration))
			if !s.RestoreFrom.IsZero() {
				v += " · can restore back to " + s.RestoreFrom.In(s.Now.Location()).Format("2 Jan")
			}
			row("Point-in-time backup", v)
		}
		if s.LastWAL.IsZero() {
			row("Newest change saved", "unknown")
		} else {
			row("Newest change saved", When(s.LastWAL, s.Now))
		}
	}
	if s.DumpsEnabled {
		row("Database dumps", fmt.Sprintf("%d of %d databases dumped tonight (total %s)", s.DumpsOK, s.DumpsTotal, Bytes(s.DumpsSize)))
	} else {
		row("Database dumps", "turned off")
	}
	row("Storage", Bytes(s.StorageUsed)+" used")
	if s.DiskUsedPct >= 0 {
		row("Server disk", fmt.Sprintf("%d%% used", s.DiskUsedPct))
	}
	if s.PointInTime && !s.NextFull.IsZero() {
		b.WriteString("\nNext full backup: " + s.NextFull.Format("Mon 2 Jan 15:04") + "\n")
	}
	return subject, b.String()
}

// Failure is the content of a failure email.
type Failure struct {
	Server   string
	Headline string // e.g. "tonight's differential backup FAILED"
	What     string // what failed
	Reason   string // plain-language likely reason; LikelyReason(Output) when empty
	Safe     string // what is still fine
	ToDo     string // what to do next
	Output   string // technical details
}

// FailureEmail returns the subject and body of a failure alert.
func FailureEmail(f Failure) (subject, body string) {
	subject = fmt.Sprintf("❌ %s — %s", f.Server, f.Headline)
	reason := f.Reason
	if reason == "" {
		reason = LikelyReason(f.Output)
	}
	var b strings.Builder
	row := func(label, value string) {
		if value != "" {
			fmt.Fprintf(&b, "%-15s %s\n", label+":", value)
		}
	}
	row("What failed", f.What)
	row("Likely reason", reason)
	row("Still safe", f.Safe)
	row("What to do", f.ToDo)
	if out := command.LastLines(f.Output, 30); out != "" {
		b.WriteString("\n--- technical details ---\n" + out + "\n")
	}
	return subject, b.String()
}

// NewDatabase is sent when a database is dumped for the first time.
func NewDatabase(server, db, label string, size int64) (subject, body string) {
	name := Name(db, label)
	subject = fmt.Sprintf("🆕 %s — new database found: %s", server, name)
	body = fmt.Sprintf("liops found a new database, %s, and dumped it for the first time (%s).\n\n"+
		"It's now included in the nightly dumps. It was already covered by the point-in-time\n"+
		"backups from the moment it was created. No action needed.\n", name, Bytes(size))
	return subject, body
}

// DeletedDatabase is sent when a database that had dumps is no longer found.
func DeletedDatabase(server, db, label string, keptUntil time.Time) (subject, body string) {
	name := Name(db, label)
	subject = fmt.Sprintf("🗑️ %s — database no longer found: %s", server, name)
	body = fmt.Sprintf("The database %s was not found tonight: it was dropped, or added to dumps.exclude.\n\n"+
		"Its newest dump is kept until %s, then deleted. Older dumps were deleted now.\n"+
		"The point-in-time backups still contain it until they expire.\n\n"+
		"If this wasn't expected, download the kept dump before %s.\n",
		name, keptUntil.Format("Mon 2 Jan"), keptUntil.Format("2 Jan"))
	return subject, body
}

// Name shows a database with its label, e.g. `tenant_abc ("abc")`.
func Name(db, label string) string {
	if label == "" || label == db {
		return db
	}
	return fmt.Sprintf("%s (%q)", db, label)
}

// When formats t relative to now: "today 01:30", "yesterday 23:10", "Mon 5 Oct 01:30".
func When(t, now time.Time) string {
	t = t.In(now.Location())
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	y3, m3, d3 := now.AddDate(0, 0, -1).Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "today " + t.Format("15:04")
	case y1 == y3 && m1 == m3 && d1 == d3:
		return "yesterday " + t.Format("15:04")
	default:
		return t.Format("Mon 2 Jan 15:04")
	}
}

// Duration formats d briefly: "36 s", "4 min", "1 h 12 min".
func Duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Round(time.Second)/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%d h %d min", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

// Bytes formats n in binary units: "412 MB", "1.2 GB".
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	suffix := []string{"KB", "MB", "GB", "TB", "PB"}[exp]
	if v < 10 {
		return fmt.Sprintf("%.1f %s", v, suffix)
	}
	return fmt.Sprintf("%.0f %s", v, suffix)
}

// SetupFinished is sent when the setup wizard has taken the first backup.
func SetupFinished(server string, backupOK bool, dumped, total int, nextFull time.Time) (subject, body string) {
	subject = fmt.Sprintf("🎉 %s — liops is set up", server)
	var b strings.Builder
	b.WriteString("liops finished setting up backups on " + server + ".\n\n")
	if backupOK {
		b.WriteString("First full backup:  done\n")
	} else {
		b.WriteString("First full backup:  FAILED (see the Jobs page; it's retried at the next scheduled backup)\n")
	}
	fmt.Fprintf(&b, "First dumps:        %d of %d databases\n", dumped, total)
	if !nextFull.IsZero() {
		b.WriteString("Next full backup:   " + nextFull.Format("Mon 2 Jan 15:04") + "\n")
	}
	b.WriteString("\nFrom now on you get a daily summary, and an email straight away if anything fails.\n")
	return subject, b.String()
}
