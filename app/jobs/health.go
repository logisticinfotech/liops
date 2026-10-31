// SPDX-License-Identifier: Apache-2.0

package jobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
)

// MonitorSocket is where jobs.New's runners find the monitoring service.
var MonitorSocket = monitor.DefaultSocket

// serverHealth is the monitoring's part of the daily summary: per database server, the
// queries that scanned whole tables in the last 24 hours. Query shapes only, never
// values. Nil when monitoring is off.
func (r *Runner) serverHealth(ctx context.Context) []string {
	if !r.Cfg.Monitor.Enabled || r.Monitor == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dbs, err := r.Monitor.DBs(ctx)
	if err != nil {
		return []string{"The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"}
	}
	var lines []string
	for _, d := range dbs {
		if d.Error != "" {
			lines = append(lines, d.Title+": the monitor can't log in. Use Set up database access in Settings → Monitoring.")
			continue
		}
		if !d.Statements {
			continue
		}
		top, err := r.Monitor.Queries(ctx, "24h", d.Label, "", 500)
		if err != nil {
			lines = append(lines, d.Title+": couldn't read the query statistics: "+err.Error())
			continue
		}
		var scans []monitor.QueryTotal
		var total float64
		for _, q := range top {
			if monitor.QueryEfficiency(d.Engine, q.QueryCounters).Scans {
				scans = append(scans, q)
				total += q.TimeMS
			}
		}
		if len(scans) == 0 {
			lines = append(lines, d.Title+": no query scanned whole tables in the last 24 hours.")
			continue
		}
		what := "query scans"
		if len(scans) > 1 {
			what = "queries scan"
		}
		lines = append(lines, fmt.Sprintf("%s: %d %s whole tables: %s of database time in the last 24 hours.", d.Title, len(scans), what, notify.MS(total)))
		unit := "rows"
		if d.Engine == monitor.EnginePostgres {
			unit = "blocks"
		}
		for _, q := range scans[:min(3, len(scans))] {
			e := monitor.QueryEfficiency(d.Engine, q.QueryCounters)
			read := notify.Count(e.PerCall) + " " + unit + " read per call"
			if e.Ratio > 0 {
				read = notify.Count(e.Ratio) + " " + unit + " read per row returned"
			}
			text := strings.Join(strings.Fields(q.Text), " ")
			if len(text) > 120 {
				text = text[:117] + "…"
			}
			lines = append(lines, fmt.Sprintf("  - %s in %s, %s calls, %s: %s", notify.MS(q.TimeMS), q.DB, notify.Count(q.Calls), read, text))
		}
	}
	if len(lines) > 0 {
		lines = append(lines, "Details, with what to change: Monitoring → Database in the liops UI.")
	}
	return lines
}
