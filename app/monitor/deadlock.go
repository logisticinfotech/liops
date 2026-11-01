// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Deadlock is MySQL's latest deadlock report. Its statements hold real values: it's read
// live for a page, never stored or logged.
type Deadlock struct {
	At         time.Time `json:"at"`
	Tables     []string  `json:"tables"`      // the tables whose rows were locked
	Statements []string  `json:"statements"`  // each transaction's statement
	RolledBack string    `json:"rolled_back"` // which transaction MySQL rolled back: "(1)" or "(2)"
	Text       string    `json:"text"`        // the whole section
}

var (
	deadlockTimeRE  = regexp.MustCompile(`(?m)^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) 0x`)
	deadlockTableRE = regexp.MustCompile("of table `([^`]+)`\\.`([^`]+)`")
	deadlockStmtRE  = regexp.MustCompile(`(?m)^MySQL thread id .*\n((?:[^\n*][^\n]*\n?)+)`)
	deadlockRollRE  = regexp.MustCompile(`\*\*\* WE ROLL BACK TRANSACTION \((\d+)\)`)
)

// parseDeadlock cuts the LATEST DETECTED DEADLOCK section out of SHOW ENGINE INNODB STATUS.
func parseDeadlock(status string) (Deadlock, bool) {
	start := strings.Index(status, "LATEST DETECTED DEADLOCK")
	if start < 0 {
		return Deadlock{}, false
	}
	section := status[start:]
	// The next section starts with a line of dashes, its title, and another line of dashes.
	if end := regexp.MustCompile(`\n-{4,}\n[A-Z][A-Z /]+\n-{4,}\n`).FindStringIndex(section[len("LATEST DETECTED DEADLOCK"):]); end != nil {
		section = section[:len("LATEST DETECTED DEADLOCK")+end[0]]
	}
	d := Deadlock{Text: strings.TrimSpace(section)}
	if m := deadlockTimeRE.FindStringSubmatch(section); m != nil {
		d.At, _ = time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local)
	}
	seen := map[string]bool{}
	for _, m := range deadlockTableRE.FindAllStringSubmatch(section, -1) {
		if t := m[1] + "." + m[2]; !seen[t] {
			seen[t] = true
			d.Tables = append(d.Tables, t)
		}
	}
	sort.Strings(d.Tables)
	for _, m := range deadlockStmtRE.FindAllStringSubmatch(section, -1) {
		if s := strings.TrimSpace(m[1]); s != "" {
			d.Statements = append(d.Statements, s)
		}
	}
	if m := deadlockRollRE.FindStringSubmatch(section); m != nil {
		d.RolledBack = "(" + m[1] + ")"
	}
	return d, true
}

func readDeadlock(ctx context.Context, d DBServer) (Deadlock, bool, error) {
	if d.Engine != EngineMySQL {
		return Deadlock{}, false, nil
	}
	out, err := d.Exec(ctx, "mysql", append(mysqlArgs(d), "--execute=SHOW ENGINE INNODB STATUS")...)
	if err != nil {
		return Deadlock{}, false, err
	}
	// --raw keeps the report's new lines; the columns are Type, Name, Status.
	text := string(out)
	if i := strings.Index(text, "\t\t"); i >= 0 {
		text = text[i+2:]
	}
	dl, ok := parseDeadlock(text)
	return dl, ok, nil
}

func (m *Monitor) deadlockHandler(mux *http.ServeMux, writeJSON func(http.ResponseWriter, any)) {
	mux.HandleFunc("GET /v1/db/deadlock", func(w http.ResponseWriter, r *http.Request) {
		for _, d := range m.servers() {
			if d.Label() != r.URL.Query().Get("server") {
				continue
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			dl, ok, err := readDeadlock(ctx, d)
			switch {
			case err != nil:
				http.Error(w, err.Error(), http.StatusBadGateway)
			case !ok:
				http.Error(w, "no deadlock since MySQL started", http.StatusNotFound)
			default:
				writeJSON(w, dl)
			}
			return
		}
		http.Error(w, "no such database server", http.StatusNotFound)
	})
}

// Deadlock reads MySQL's latest deadlock report, live.
func (c *Client) Deadlock(ctx context.Context, server string) (Deadlock, error) {
	var out Deadlock
	return out, c.get(ctx, "/v1/db/deadlock?"+url.Values{"server": {server}}.Encode(), &out)
}
