// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
)

// queryRanges are the Database page's ranges: statement statistics are kept 7 days.
var queryRanges = []string{"1h", "24h", "7d"}

type dbMonData struct {
	Problem string
	Servers []monitor.DBStatus
	Server  monitor.DBStatus // the one shown
	Range   string
	Ranges  []string
	DB      string // only this database ("" for all)
	DBNames []string

	Scans []queryRow // the headline: queries that scan whole tables
	Top   []queryRow
	New   []queryRow // first seen in this range: what a deploy brought
	Live  dbLive

	Charts        []chartBox
	Activity      *monitor.Activity
	ActivityError string

	Tables      *monitor.DBTables
	TablesError string
	Biggest     []tableRow
	Growing     []tableRow
	SeqScanned  []tableRow
	Bloated     []tableRow
	NoPK        []tableRow
	Unused      []monitor.IndexInfo
	Duplicates  []monitor.IndexInfo
	XIDOld      []xidRow
	Memory      []memRow
	DataBytes   float64
}

// queryRow is a query shape with what the UI shows about it.
type queryRow struct {
	monitor.QueryTotal
	Engine    string
	Eff       monitor.Efficiency
	Flags     []monitor.Flag
	PerCallMS float64
	Short     string
	Link      string
}

type dbLive struct {
	Connections, MaxConnections, Running, Slow, LockWaits, LongTx, IdleTx, PerSecond, CacheHit float64
	HasCacheHit                                                                                bool
}

type tableRow struct {
	monitor.TableInfo
	Growth    float64 // bytes a day
	HasGrowth bool
	DeadShare float64 // percent of rows that are dead (PostgreSQL)
	PerScan   float64 // rows read per whole-table scan (PostgreSQL)
}

type xidRow struct {
	DB  string
	Age float64
}

type memRow struct {
	Name   string
	Bytes  float64
	Advice string
}

func (s *Server) newQueryRow(engine string, q monitor.QueryTotal, rng string) queryRow {
	r := queryRow{QueryTotal: q, Engine: engine, Eff: monitor.QueryEfficiency(engine, q.QueryCounters), Flags: monitor.QueryFlags(q.Text, q.QueryCounters)}
	if q.Calls > 0 {
		r.PerCallMS = q.TimeMS / q.Calls
	}
	r.Short = strings.Join(strings.Fields(q.Text), " ")
	if len(r.Short) > 160 {
		r.Short = r.Short[:157] + "…"
	}
	if r.Short == "" {
		r.Short = "(query text not read yet)"
	}
	r.Link = "/monitoring/database/query?" + url.Values{"server": {q.Server}, "db": {q.DB}, "id": {q.ID}, "range": {rng}}.Encode()
	return r
}

// pickServer returns the monitor's servers and the one asked for (the first by default).
func (s *Server) pickServer(ctx context.Context, d *dbMonData, label string) bool {
	if !s.Svc.Config().Monitor.Enabled {
		d.Problem = "Monitoring is off. Turn it on in Settings → Monitoring."
		return false
	}
	servers, err := s.Monitor.DBs(ctx)
	if err != nil {
		d.Problem = "The monitoring service isn't answering. Check it with: sudo systemctl status liops-monitor"
		if !errors.Is(err, monitor.ErrNotRunning) {
			d.Problem = "The monitoring service couldn't answer: " + err.Error()
		}
		return false
	}
	d.Servers = servers
	if len(servers) == 0 {
		d.Problem = "No PostgreSQL or MySQL server was found on this machine."
		return false
	}
	d.Server = servers[0]
	for _, sv := range servers {
		if sv.Label == label {
			d.Server = sv
		}
	}
	return true
}

// dbMonitor is the Database page: which queries take the database's time, and why.
func (s *Server) dbMonitor(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := dbMonData{Range: q.Get("range"), Ranges: queryRanges, DB: q.Get("db")}
	if _, ok := monitor.Ranges[d.Range]; !ok || d.Range == "90d" {
		d.Range = "24h"
	}
	render := func() { s.render(w, r, "dbmonitor", page{Title: "Database", Nav: "dbmonitor", Data: d}) }
	ctx := r.Context()
	if !s.pickServer(ctx, &d, q.Get("server")) {
		render()
		return
	}
	sv := d.Server
	if sv.Error != "" {
		d.Problem = "The monitor can't log in to " + sv.Title + " yet. Use Set up database access in Settings → Monitoring."
		render()
		return
	}

	// Queries: the headline, and the top by total time.
	all, err := s.Monitor.Queries(ctx, d.Range, sv.Label, "", 500)
	if err == nil {
		names := map[string]bool{}
		for _, qt := range all {
			names[qt.DB] = true
			if d.DB != "" && qt.DB != d.DB {
				continue
			}
			row := s.newQueryRow(sv.Engine, qt, d.Range)
			if row.Eff.Scans && len(d.Scans) < 15 {
				d.Scans = append(d.Scans, row)
			}
			if len(d.Top) < 25 {
				d.Top = append(d.Top, row)
			}
			if since := time.Now().Add(-monitor.Ranges[d.Range]); qt.FirstSeen.After(since) && len(d.New) < 10 {
				d.New = append(d.New, row)
			}
		}
		for n := range names {
			d.DBNames = append(d.DBNames, n)
		}
		sort.Strings(d.DBNames)
	}

	// Live numbers and their history.
	if latest, err := s.Monitor.Latest(ctx); err == nil {
		v := func(name string) float64 { return latest.Values["db."+name+":"+sv.Label] }
		d.Live = dbLive{Connections: v("connections"), MaxConnections: v("max_connections"), Running: v("running"), Slow: v("slow"),
			LockWaits: v("lock_waits"), LongTx: v("long_transactions"), IdleTx: v("idle_in_transaction"), PerSecond: v("per_second"), CacheHit: v("cache_hit")}
		_, d.Live.HasCacheHit = latest.Values["db.cache_hit:"+sv.Label]
	}
	markers := s.jobMarkers(time.Now().Add(-monitor.Ranges[d.Range]))
	perSecond := "Statements per second"
	if sv.Engine == monitor.EnginePostgres {
		perSecond = "Transactions per second"
	}
	for _, c := range []struct {
		title, unit string
		names       []string
		labels      []string
	}{
		{"Connections", "number", []string{"connections", "running"}, []string{"connected", "running"}},
		{"Waiting and slow", "number", []string{"slow", "lock_waits", "long_transactions"}, []string{"running over 1 s", "waiting for a lock", "transactions over 1 min"}},
		{perSecond, "number", []string{"per_second"}, []string{"per second"}},
		{"Reads served from memory", "percent", []string{"cache_hit"}, []string{"cache hit ratio"}},
	} {
		names := make([]string, len(c.names))
		for i, n := range c.names {
			names[i] = "db." + n + ":" + sv.Label
		}
		res, err := s.Monitor.Series(ctx, d.Range, names...)
		if err != nil || len(res.X) < 2 {
			continue
		}
		spec := chartSpec{Unit: c.unit, X: res.X, Markers: markers}
		for i, n := range names {
			spec.Series = append(spec.Series, chartSeries{Label: c.labels[i], Values: res.Series[n].Avg})
		}
		if data, err := json.Marshal(spec); err == nil {
			d.Charts = append(d.Charts, chartBox{Title: c.title, Spec: string(data)})
		}
	}

	// What's running now: read live.
	if a, err := s.Monitor.Activity(ctx, sv.Label); err == nil {
		d.Activity = &a
	} else {
		d.ActivityError = err.Error()
	}

	if t, err := s.Monitor.Tables(ctx, sv.Label); err == nil {
		d.Tables = &t
		s.tableFindings(&d, t, sv.Engine)
	} else {
		d.TablesError = "Tables are read once an hour; the first pass hasn't finished yet."
	}
	render()
}

// tableFindings sorts the hourly table snapshot into what the page shows.
func (s *Server) tableFindings(d *dbMonData, t monitor.DBTables, engine string) {
	var rows []tableRow
	for _, ti := range t.Tables {
		if d.DB != "" && ti.DB != d.DB {
			continue
		}
		row := tableRow{TableInfo: ti}
		row.Growth, row.HasGrowth = t.Growth[ti.Key()]
		if total := ti.Rows + ti.DeadRows; total > 0 {
			row.DeadShare = 100 * ti.DeadRows / total
		}
		if ti.SeqScans > 0 {
			row.PerScan = ti.SeqRowsRead / ti.SeqScans
		}
		d.DataBytes += ti.Bytes
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Bytes > rows[j].Bytes })
	d.Biggest = rows[:min(15, len(rows))]
	for _, r := range rows {
		if !r.PrimaryKey && len(d.NoPK) < 20 {
			d.NoPK = append(d.NoPK, r)
		}
		if r.DeadRows >= 10000 && r.DeadShare >= 20 {
			d.Bloated = append(d.Bloated, r)
		}
		if engine == monitor.EnginePostgres && r.Rows >= 10000 && r.PerScan >= 10000 && r.SeqScans >= 10 {
			d.SeqScanned = append(d.SeqScanned, r)
		}
	}
	growing := append([]tableRow(nil), rows...)
	sort.Slice(growing, func(i, j int) bool { return growing[i].Growth > growing[j].Growth })
	for _, r := range growing {
		if r.HasGrowth && r.Growth >= 1<<20 && len(d.Growing) < 10 {
			d.Growing = append(d.Growing, r)
		}
	}
	sort.Slice(d.SeqScanned, func(i, j int) bool { return d.SeqScanned[i].SeqRowsRead > d.SeqScanned[j].SeqRowsRead })
	d.SeqScanned = d.SeqScanned[:min(10, len(d.SeqScanned))]
	for _, ix := range t.Indexes {
		if d.DB != "" && ix.DB != d.DB {
			continue
		}
		if ix.Unused && !ix.Unique && !ix.Primary && len(d.Unused) < 30 {
			d.Unused = append(d.Unused, ix)
		}
		if ix.Duplicate != "" {
			d.Duplicates = append(d.Duplicates, ix)
		}
	}
	for db, age := range t.XIDAge {
		if age >= 200e6 {
			d.XIDOld = append(d.XIDOld, xidRow{db, age})
		}
	}
	sort.Slice(d.XIDOld, func(i, j int) bool { return d.XIDOld[i].Age > d.XIDOld[j].Age })

	// Memory against data size.
	data := 0.0
	for _, ti := range t.Tables {
		data += ti.Bytes
	}
	hit := ""
	if d.Live.HasCacheHit {
		hit = fmt.Sprintf(" %.1f%% of reads are served from memory now.", d.Live.CacheHit)
	}
	if pool, ok := t.Memory["innodb_buffer_pool_size"]; ok {
		advice := "All the data fits in memory."
		if data > pool {
			advice = fmt.Sprintf("About %.0f%% of the data fits in memory; the rest is read from disk when it's needed.", 100*pool/data)
		}
		if pool <= 128<<20 {
			advice += " 128 MB is Ubuntu's default, meant for small machines."
		}
		d.Memory = append(d.Memory, memRow{"innodb_buffer_pool_size", pool, advice + hit})
	}
	for _, name := range []string{"shared_buffers", "effective_cache_size", "work_mem", "maintenance_work_mem"} {
		if v, ok := t.Memory[name]; ok {
			advice := ""
			if name == "shared_buffers" {
				advice = "PostgreSQL's own cache; the operating system caches more."
				if v <= 128<<20 {
					advice += " 128 MB is the default, meant for small machines."
				}
				advice += hit
			}
			d.Memory = append(d.Memory, memRow{name, v, advice})
		}
	}
}

// dbMonitorQuery is one query shape: its text, what's wrong with it, its history, the
// tables it touches with their indexes, and, for MySQL, one real statement read live.
func (s *Server) dbMonitorQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rng := q.Get("range")
	if _, ok := monitor.Ranges[rng]; !ok || rng == "90d" {
		rng = "24h"
	}
	type tableIndexes struct {
		Table   monitor.TableInfo
		Indexes []monitor.IndexInfo
		Suggest *monitor.IndexSuggestion
	}
	var d struct {
		dbMonData
		Row         queryRow
		Charts      []chartBox
		Touched     []tableIndexes
		Sample      *monitor.QuerySample
		SampleFlags []monitor.Flag
		SampleError string
		BackLink    string
		RangeLinks  map[string]string
	}
	d.Range, d.Ranges = rng, queryRanges
	render := func() { s.render(w, r, "dbmonitor_query", page{Title: "Query", Nav: "dbmonitor", Data: d}) }
	ctx := r.Context()
	if !s.pickServer(ctx, &d.dbMonData, q.Get("server")) {
		render()
		return
	}
	sv := d.Server
	detail, err := s.Monitor.Query(ctx, rng, sv.Label, q.Get("db"), q.Get("id"))
	if err != nil {
		d.Problem = "This query isn't in the statistics (any more): " + err.Error()
		render()
		return
	}
	total := monitor.QueryTotal{QueryShape: detail.Shape}
	var x []int64
	var perCall, calls []*float64
	for _, p := range detail.Points {
		total.QueryCounters.Calls += p.Calls
		total.TimeMS += p.TimeMS
		total.RowsExamined += p.RowsExamined
		total.RowsSent += p.RowsSent
		total.NoIndex += p.NoIndex
		total.TempDisk += p.TempDisk
		x = append(x, p.At)
		c, pc := p.Calls, 0.0
		if p.Calls > 0 {
			pc = p.TimeMS / p.Calls
		}
		calls, perCall = append(calls, &c), append(perCall, &pc)
	}
	d.Row = s.newQueryRow(sv.Engine, total, rng)
	d.BackLink = "/monitoring/database?" + url.Values{"server": {sv.Label}, "range": {rng}}.Encode()
	d.RangeLinks = map[string]string{}
	for _, rr := range queryRanges {
		d.RangeLinks[rr] = "/monitoring/database/query?" + url.Values{"server": {sv.Label}, "db": {detail.Shape.DB}, "id": {detail.Shape.ID}, "range": {rr}}.Encode()
	}
	if len(x) >= 2 {
		for _, c := range []chartSpec{
			{Unit: "ms", X: x, Series: []chartSeries{{Label: "time per call", Values: perCall}}},
			{Unit: "number", X: x, Series: []chartSeries{{Label: "calls per 5 minutes", Values: calls}}},
		} {
			title := "Time per call"
			if c.Unit == "number" {
				title = "Calls"
			}
			if data, err := json.Marshal(c); err == nil {
				d.Charts = append(d.Charts, chartBox{Title: title, Spec: string(data)})
			}
		}
	}

	// The tables it names, with their indexes.
	if t, err := s.Monitor.Tables(ctx, sv.Label); err == nil {
		for _, name := range monitor.TablesIn(detail.Shape.Text) {
			for _, ti := range t.Tables {
				if ti.DB != detail.Shape.DB || !(name == ti.Name || name == ti.Schema+"."+ti.Name || name == ti.DB+"."+ti.Name) {
					continue
				}
				ti2 := tableIndexes{Table: ti}
				for _, ix := range t.Indexes {
					if ix.TableKey() == ti.Key() {
						ti2.Indexes = append(ti2.Indexes, ix)
					}
				}
				if len(monitor.TablesIn(detail.Shape.Text)) == 1 {
					ti2.Suggest = monitor.SuggestIndex(detail.Shape.Text, ti.Name, ti2.Indexes)
				}
				d.Touched = append(d.Touched, ti2)
			}
		}
	}

	// MySQL keeps one real statement per shape. It has real values: shown, never stored.
	if sv.Engine == monitor.EngineMySQL {
		if sample, err := s.Monitor.Sample(ctx, sv.Label, detail.Shape.DB, detail.Shape.ID); err == nil {
			d.Sample, d.SampleFlags = &sample, monitor.SampleFlags(sample.Text)
		} else {
			d.SampleError = err.Error()
		}
	}
	render()
}
