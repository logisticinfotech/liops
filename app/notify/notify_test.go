// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"
	"time"
)

var ist = time.FixedZone("IST", 5*3600+1800)

func TestDailySummaryHealthy(t *testing.T) {
	now := time.Date(2026, 10, 5, 7, 30, 0, 0, ist)
	subject, body := DailySummary(Summary{
		Server:             "db-1",
		Now:                now,
		LastBackupType:     "diff",
		LastBackupAt:       time.Date(2026, 10, 5, 1, 30, 0, 0, ist),
		LastBackupDuration: 36 * time.Second,
		PointInTime:        true,
		RestoreFrom:        time.Date(2026, 9, 5, 1, 31, 0, 0, ist),
		LastWAL:            time.Date(2026, 10, 5, 7, 0, 0, 0, ist),
		DumpsEnabled:       true, DumpsOK: 11, DumpsTotal: 11, DumpsSize: 412 << 20,
		StorageUsed: 1288490188, DiskUsedPct: 18,
		NextFull: time.Date(2026, 10, 11, 1, 30, 0, 0, ist),
	})
	if subject != "✅ db-1 — backups healthy — Mon 5 Oct" {
		t.Errorf("subject = %q", subject)
	}
	for _, want := range []string{
		"All good. No action needed.",
		"last diff today 01:30 (36 s) · can restore back to 5 Sep",
		"Newest change saved:   today 07:00",
		"11 of 11 databases dumped tonight (total 412 MB)",
		"1.2 GB used",
		"18% used",
		"Next full backup: Sun 11 Oct 01:30",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
}

func TestDailySummaryWithProblems(t *testing.T) {
	subject, body := DailySummary(Summary{Server: "db-1", Now: time.Now(), PointInTime: true, Problems: []string{"no backup in 2 days"}, DiskUsedPct: -1})
	if !strings.HasPrefix(subject, "⚠️ db-1 — backups need attention") {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(body, "  - no backup in 2 days") || !strings.Contains(body, "none yet") || strings.Contains(body, "Server disk") {
		t.Errorf("body:\n%s", body)
	}
}

func TestDailySummaryWithoutPointInTime(t *testing.T) {
	_, body := DailySummary(Summary{Server: "db-1", Now: time.Now(), DumpsEnabled: true, DiskUsedPct: -1,
		NextFull: time.Now().Add(time.Hour)})
	for _, unwanted := range []string{"Point-in-time", "Newest change saved", "Next full backup"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("a server without point-in-time backups mustn't mention %q:\n%s", unwanted, body)
		}
	}
}

func TestFailureEmail(t *testing.T) {
	subject, body := FailureEmail(Failure{
		Server:   "db-2",
		Headline: "tonight's database dumps FAILED for 1 database",
		What:     `dump of tenant_xyz ("xyz")`,
		Safe:     "point-in-time backups are fine",
		ToDo:     "check the server, then run: liops run dumps",
		Output:   "pg_dump: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: Connection refused\n",
	})
	if subject != "❌ db-2 — tonight's database dumps FAILED for 1 database" {
		t.Errorf("subject = %q", subject)
	}
	for _, want := range []string{
		`What failed:    dump of tenant_xyz ("xyz")`,
		"Likely reason:  the database refused the connection (is PostgreSQL running?)",
		"--- technical details ---\npg_dump: error:",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body is missing %q:\n%s", want, body)
		}
	}
}

func TestLikelyReason(t *testing.T) {
	cases := map[string]string{
		"ERROR: [047]: unable to write '/var/lib/x': No space left on device":                               "disk is full",
		"WAL segment 000000010000000000000003 was not archived before the 60000ms timeout":                  "WAL upload is stuck",
		"HTTP 403 Forbidden <Code>SignatureDoesNotMatch</Code>":                                             "storage key was rejected",
		"dial tcp: lookup example.r2.cloudflarestorage.com: no such host":                                   "couldn't reach storage",
		"ERROR: [055]: unable to load info file '/pgbackrest/archive/main/archive.info' ... missing stanza": "stanza",
		"ERROR: [028]: backup and archive info files exist but do not match the database":                   "different PostgreSQL server",
		"ERROR: [044]: WAL file system-id 7683116411252273504 do not match stanza system-id 7000":           "different PostgreSQL server",
		"something new and strange":                           "unknown",
		"archived 000000010000000000000403 then failed oddly": "unknown", // a WAL name is not an HTTP 403
	}
	for output, want := range cases {
		if got := LikelyReason(output); !strings.Contains(got, want) {
			t.Errorf("LikelyReason(%q) = %q, want it to contain %q", output, got, want)
		}
	}
}

func TestFormatting(t *testing.T) {
	for n, want := range map[int64]string{512: "512 B", 1536: "1.5 KB", 412 << 20: "412 MB", 1288490188: "1.2 GB"} {
		if got := Bytes(n); got != want {
			t.Errorf("Bytes(%d) = %q, want %q", n, got, want)
		}
	}
	for d, want := range map[time.Duration]string{36 * time.Second: "36 s", 4 * time.Minute: "4 min", 72 * time.Minute: "1 h 12 min"} {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
	now := time.Date(2026, 10, 5, 7, 30, 0, 0, ist)
	if got := When(time.Date(2026, 10, 4, 23, 10, 0, 0, ist), now); got != "yesterday 23:10" {
		t.Errorf("When = %q", got)
	}
	if got := Name("tenant_abc", "abc"); got != `tenant_abc ("abc")` {
		t.Errorf("Name = %q", got)
	}
}

func TestSetupFinished(t *testing.T) {
	subject, body := SetupFinished("db-1", true, 3, 3, time.Date(2026, 10, 18, 1, 30, 0, 0, ist))
	if subject != "🎉 db-1 — liops is set up" || !strings.Contains(body, "3 of 3 databases") || !strings.Contains(body, "Sun 18 Oct 01:30") {
		t.Errorf("%s\n%s", subject, body)
	}
}
