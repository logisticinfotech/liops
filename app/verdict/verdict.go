// SPDX-License-Identifier: Apache-2.0

// Package verdict answers "tune it, fix the app, or a bigger VM?" (docs/PLAN-monitor.md
// §5): plain rules over the last day's numbers, each finding with the numbers behind it
// and what to do. It never claims more than the evidence shows.
package verdict

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Evidence is what the rules look at, gathered from the monitor and root's own checks.
type Evidence struct {
	CPUs float64
	// Averages over the window, in percent, and the share of minutes the CPU was over 80%.
	CPUBusy, CPUHighShare, Steal, IOWait float64
	DiskUtil                             float64 // the busiest disk's average utilisation, percent
	DiskDevice                           string
	MemTotal                             float64
	MemAvailMin                          float64 // the least memory available in the window
	SwapIn                               float64 // pages swapped in a second, on average
	OOMKills                             float64
	Filesystems                          []Filesystem
	Pools                                []Pool
	OPcache                              []OPcache
	Databases                            []Database
	Requests                             Requests
	NPlusOne                             []Query // query shapes called many times per request
	Logs                                 []Log
	Apps                                 []App
	Queues                               []Queue
	Deploy                               *Deploy
}

// Filesystem is one filesystem's fullness.
type Filesystem struct {
	Mount      string
	Pct        float64
	FullInDays int // 0: not filling within 90 days
}

// Pool is one PHP-FPM pool.
type Pool struct {
	Label       string
	BusyPct     float64 // share of readings with every worker busy
	Reached     float64 // "max children reached"
	MaxChildren int
	Affordable  int // workers the free memory allows
}

// OPcache is one PHP version's OPcache problems, in words.
type OPcache struct {
	Version  string
	Problems []string
}

// Database is one database server over the window.
type Database struct {
	Title       string
	TotalMS     float64 // all statements' time
	ScanMS      float64 // the time of statements that scan whole tables
	Scans       []Query
	SlowMax     float64 // the most statements running over 1 s at once
	LockWaitMax float64
	Deadlocks   float64
	CacheHit    float64 // percent; HasCacheHit false when unknown
	HasCacheHit bool
}

// Query is a query shape and what's known about it.
type Query struct {
	DB, Text   string
	MS         float64
	PerRequest float64 // calls per request (N+1)
	Route      string  // the route that runs it, when its requests move with the calls
	Read       string  // "1.65M rows per row returned", "25 MB per call"
}

// Requests are the web requests over the window.
type Requests struct {
	Count, Slow        float64 // slow: over 1 s
	P95, TimeMS, AppMS float64
	SlowRoutes         []string
}

// Log is one log file's growth.
type Log struct {
	Path    string
	PerHour float64 // bytes
}

// App is one Laravel app's failing checks.
type App struct {
	Dir      string
	Problems []string
}

// Queue is a Redis queue's backlog.
type Queue struct {
	Key     string
	Waiting float64
}

// Deploy compares the requests before and after the latest deploy.
type Deploy struct {
	At            time.Time
	What          string
	Before, After Requests
	Routes        []string // routes that got slower: "GET /x: 120 ms → 900 ms"
	NewQueries    []string
}

// Level is how much a finding matters.
type Level int

const (
	Info Level = iota
	Warning
	Problem
)

// Finding is one conclusion: what it means, the numbers behind it, what to do.
type Finding struct {
	Level   Level
	Area    string // Machine, Disk, Database, PHP, App, Requests, Logs
	Says    string
	Because string
	Do      string
	Link    string // the page with the details
	weight  float64
}

// Judge applies the rules, most important first.
func Judge(e Evidence) []Finding {
	var out []Finding
	add := func(f Finding) { out = append(out, f) }
	pct := func(v float64) string { return fmt.Sprintf("%.0f%%", v) }

	// The machine.
	if e.Steal >= 5 {
		add(Finding{Level: level(e.Steal >= 10), Area: "Machine", weight: 90 + e.Steal,
			Says:    "The host is overcommitted: other VMs take this one's CPU time",
			Because: "CPU steal averaged " + pct(e.Steal) + " (time this VM was ready to run but the host ran something else).",
			Do:      "Move the VM to another host or a plan with dedicated CPUs. Tuning won't help.", Link: "/monitoring/resources"})
	}
	if e.IOWait >= 15 || e.DiskUtil >= 60 {
		add(Finding{Level: Problem, Area: "Disk", weight: 80 + e.IOWait,
			Says:    "The disk is the bottleneck",
			Because: fmt.Sprintf("The CPU waited for the disk %s of the time; %s was busy %s on average.", pct(e.IOWait), or(e.DiskDevice, "the busiest disk"), pct(e.DiskUtil)),
			Do:      "A faster disk (NVMe, more IOPS), or more memory for the database's cache so it reads the disk less.", Link: "/monitoring/resources"})
	}
	lowMem := e.MemTotal > 0 && e.MemAvailMin < 0.05*e.MemTotal
	if e.OOMKills > 0 || e.SwapIn >= 10 || lowMem {
		var why []string
		if e.OOMKills > 0 {
			why = append(why, fmt.Sprintf("the kernel killed %.0f processes for memory", e.OOMKills))
		}
		if e.SwapIn >= 10 {
			why = append(why, fmt.Sprintf("it read %.0f pages a second back from swap", e.SwapIn))
		}
		if lowMem {
			why = append(why, fmt.Sprintf("memory available fell to %.0f%%", 100*e.MemAvailMin/e.MemTotal))
		}
		add(Finding{Level: Problem, Area: "Machine", weight: 95,
			Says:    "The server is short of memory",
			Because: capitalize(strings.Join(why, "; ")) + ".",
			Do:      "More RAM, or less demand: fewer PHP workers (pm.max_children), smaller database buffers.", Link: "/monitoring/resources"})
	}
	for _, f := range e.Filesystems {
		if f.Pct >= 90 || (f.FullInDays > 0 && f.FullInDays <= 30) {
			says := fmt.Sprintf("%s is %s full", f.Mount, pct(f.Pct))
			because := "At 100% writes fail: the database stops, uploads and logs fail."
			if f.FullInDays > 0 {
				says = fmt.Sprintf("%s will be full in about %d days", f.Mount, f.FullInDays)
				because = fmt.Sprintf("It's %s full and grew steadily over the last day.", pct(f.Pct))
			}
			add(Finding{Level: level(f.Pct >= 90 || f.FullInDays <= 7), Area: "Disk", weight: 85 - float64(f.FullInDays),
				Says: says, Because: because, Do: "See what grows on the Resources page (databases, WAL or binlogs, logs), and clean up or grow the disk.", Link: "/monitoring/resources"})
		}
	}

	// The database first: scans slow everything else down too.
	anyScans := false
	for _, d := range e.Databases {
		if len(d.Scans) == 0 || d.ScanMS < 60e3 || d.TotalMS <= 0 || d.ScanMS/d.TotalMS < 0.3 {
			continue
		}
		anyScans = true
		share := 100 * d.ScanMS / d.TotalMS
		var lines []string
		for _, q := range d.Scans[:min(3, len(d.Scans))] {
			lines = append(lines, fmt.Sprintf("%s in %s (%s): %s", ms(q.MS), q.DB, q.Read, short(q.Text, 90)))
		}
		because := fmt.Sprintf("%d %s whole tables took %s of %s's time (%s): %s.", len(d.Scans), plural(len(d.Scans), "query that scans", "queries that scan"), pct(share), d.Title, ms(d.ScanMS), strings.Join(lines, "; "))
		if d.SlowMax >= 1 && e.CPUBusy >= 50 {
			because += fmt.Sprintf(" Up to %.0f statements ran over a second at once while the CPU was %s busy: each scan holds a core, so other requests slow down too.", d.SlowMax, pct(e.CPUBusy))
		}
		add(Finding{Level: Problem, Area: "Database", weight: 100 + share,
			Says: "Fix the app first: some queries read whole tables", Because: because,
			Do: "Add the indexes they need or rewrite their filters (the Database page lists each one with its tables' indexes and what's wrong). A bigger VM would only hide it until the tables grow again.", Link: "/monitoring/database"})
	}
	for _, q := range e.NPlusOne {
		add(Finding{Level: Problem, Area: "Database", weight: 88,
			Says:    "N+1 queries in the code",
			Because: nPlusOneBecause(q),
			Do:      "It's run in a loop, once per item: load the relation with the parent query (eager loading, ->with(...)) instead.", Link: "/monitoring/database"})
	}
	for _, d := range e.Databases {
		if d.LockWaitMax >= 3 || d.Deadlocks > 0 {
			add(Finding{Level: Warning, Area: "Database", weight: 60,
				Says:    "Requests and jobs wait for each other's locks in " + d.Title,
				Because: lockBecause(d),
				Do:      "Keep transactions short (no outside calls inside them), update rows in the same order everywhere, and index the columns the locked updates filter on. For MySQL the Database page shows the latest deadlock's two statements.", Link: "/monitoring/database"})
		}
		if d.HasCacheHit && d.CacheHit < 95 && e.MemTotal > 0 && e.MemAvailMin > 0.3*e.MemTotal {
			add(Finding{Level: Warning, Area: "Database", weight: 55,
				Says:    "Give " + d.Title + " more memory",
				Because: fmt.Sprintf("Only %.1f%% of its reads came from memory while %.0f%% of the server's memory sat unused.", d.CacheHit, 100*e.MemAvailMin/e.MemTotal),
				Do:      "Raise innodb_buffer_pool_size (MySQL) or shared_buffers (PostgreSQL); the Database page shows the data size.", Link: "/monitoring/database"})
		}
	}

	// PHP.
	poolBusy := false
	for _, p := range e.Pools {
		if p.BusyPct < 10 && p.Reached == 0 {
			continue
		}
		poolBusy = true
		because := fmt.Sprintf("All %d workers of %s were busy in %s of the readings", p.MaxChildren, p.Label, pct(p.BusyPct))
		if p.Reached > 0 {
			because += fmt.Sprintf(", and it ran out of workers %.0f times", p.Reached)
		}
		because += fmt.Sprintf("; the CPU was %s busy.", pct(e.CPUBusy))
		if e.CPUBusy < 70 {
			do := "Raise pm.max_children"
			if p.Affordable > p.MaxChildren {
				do += fmt.Sprintf(" (the memory free allows about %d)", p.Affordable)
			}
			add(Finding{Level: Problem, Area: "PHP", weight: 87, Says: "Requests wait for a free PHP worker while the CPU has room", Because: because,
				Do: do + ". If workers are busy because they wait on slow queries or APIs, fix those too.", Link: "/monitoring/php"})
		} else {
			add(Finding{Level: Warning, Area: "PHP", weight: 70, Says: "PHP's workers and the CPU are both busy", Because: because,
				Do: "More workers won't help: find what takes the time (the queries above, the slow log) or add CPUs.", Link: "/monitoring/php"})
		}
	}
	for _, o := range e.OPcache {
		if len(o.Problems) > 0 {
			add(Finding{Level: Warning, Area: "PHP", weight: 65, Says: "PHP " + o.Version + "'s OPcache is too small",
				Because: strings.Join(o.Problems, " "), Do: "Raise opcache.memory_consumption and opcache.max_accelerated_files in the FPM php.ini.", Link: "/monitoring/php"})
		}
	}
	for _, a := range e.Apps {
		if len(a.Problems) > 0 {
			add(Finding{Level: Problem, Area: "App", weight: 75, Says: a.Dir + " has production settings that cost time",
				Because: strings.Join(a.Problems, " "), Link: "/monitoring/apps"})
		}
	}

	for _, q := range e.Queues {
		if q.Waiting >= 1000 {
			add(Finding{Level: Warning, Area: "App", weight: 68, Says: fmt.Sprintf("%.0f jobs are waiting in the queue %s", q.Waiting, q.Key),
				Because: "The queue workers aren't keeping up, or aren't running: mail, exports and notifications go out late.",
				Do:      "Check the workers (supervisorctl status, or systemctl for a queue:work unit), add workers, or find the slow job in the logs.", Link: "/monitoring/apps"})
		}
	}

	// Logs.
	for _, l := range e.Logs {
		if l.PerHour >= 20<<20 {
			add(Finding{Level: level(l.PerHour >= 100<<20), Area: "Logs", weight: 50 + l.PerHour/(10<<20),
				Says:    "A log grows by " + bytes(l.PerHour) + " an hour",
				Because: l.Path + " is written to all the time: that's disk space and I/O, and usually a debug level or a line in a loop.",
				Do:      "Lower the log level (LOG_LEVEL=warning) or remove the line; check the file in Logs.", Link: "/monitoring/logs"})
		}
	}

	// The deploy.
	if d := e.Deploy; d != nil && d.Before.Count >= 100 && d.After.Count >= 100 && d.After.P95 > 1.3*d.Before.P95 && d.After.P95-d.Before.P95 >= 100 {
		because := fmt.Sprintf("95%% of requests took up to %s before the deploy (%s) and %s after.", ms(d.Before.P95), d.What, ms(d.After.P95))
		if len(d.Routes) > 0 {
			because += " Slower routes: " + strings.Join(d.Routes, "; ") + "."
		}
		if len(d.NewQueries) > 0 {
			because += " New queries since: " + strings.Join(d.NewQueries, "; ") + "."
		}
		add(Finding{Level: Problem, Area: "Requests", weight: 92, Says: "Slower since the deploy at " + d.At.Format("Mon 15:04"), Because: because,
			Do: "Look at what that deploy changed in those routes and queries.", Link: "/monitoring/requests"})
	}

	// What's left when nothing above explains it.
	if e.CPUHighShare >= 25 && !anyScans && !poolBusy {
		add(Finding{Level: Problem, Area: "Machine", weight: 78, Says: "The workload has outgrown the VM",
			Because: fmt.Sprintf("The CPU was over 80%% busy %s of the time, with no single query or setting behind it.", pct(e.CPUHighShare)),
			Do:      fmt.Sprintf("More CPUs (it has %.0f).", e.CPUs), Link: "/monitoring/resources"})
	}
	r := e.Requests
	dbMS := 0.0
	for _, d := range e.Databases {
		dbMS += d.TotalMS
	}
	if r.Count >= 100 && r.Slow/r.Count >= 0.02 && e.CPUBusy < 40 && !poolBusy && !anyScans && (r.AppMS == 0 || dbMS < 0.2*r.AppMS) {
		because := fmt.Sprintf("%.1f%% of requests took over a second, yet the CPU was %s busy and the database spent %s on them.", 100*r.Slow/r.Count, pct(e.CPUBusy), ms(dbMS))
		if len(r.SlowRoutes) > 0 {
			because += " Slow routes: " + strings.Join(r.SlowRoutes, "; ") + "."
		}
		add(Finding{Level: Problem, Area: "Requests", weight: 72, Says: "Requests are slow while waiting on something else", Because: because,
			Do: "External APIs, mail, file locks or sleeps: PHP's slow log shows where each slow request was waiting.", Link: "/monitoring/php"})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Level != out[j].Level {
			return out[i].Level > out[j].Level
		}
		return out[i].weight > out[j].weight
	})
	return out
}

func level(problem bool) Level {
	if problem {
		return Problem
	}
	return Warning
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func short(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

func ms(v float64) string {
	switch {
	case v < 1000:
		return fmt.Sprintf("%.0f ms", v)
	case v < 60e3:
		return fmt.Sprintf("%.1f s", v/1e3)
	case v < 3600e3:
		return fmt.Sprintf("%.0f min", v/60e3)
	}
	return fmt.Sprintf("%.1f h", v/3600e3)
}

func bytes(v float64) string {
	switch {
	case v >= 1<<30:
		return fmt.Sprintf("%.1f GB", v/(1<<30))
	case v >= 1<<20:
		return fmt.Sprintf("%.0f MB", v/(1<<20))
	}
	return fmt.Sprintf("%.0f KB", v/(1<<10))
}

func nPlusOneBecause(q Query) string {
	if q.Route != "" {
		return fmt.Sprintf("This query runs about %.0f times per request to %s (its calls rise and fall with that route's requests), in %s: %s", q.PerRequest, q.Route, q.DB, short(q.Text, 120))
	}
	return fmt.Sprintf("This query ran %.0f times per web request on average, in %s: %s", q.PerRequest, q.DB, short(q.Text, 120))
}

func lockBecause(d Database) string {
	var parts []string
	if d.LockWaitMax >= 1 {
		parts = append(parts, fmt.Sprintf("up to %.0f statements waited for a lock at once", d.LockWaitMax))
	}
	if d.Deadlocks > 0 {
		parts = append(parts, fmt.Sprintf("%.0f %s: two transactions each held a row the other needed, and one was rolled back", d.Deadlocks, plural(int(d.Deadlocks), "deadlock", "deadlocks")))
	}
	return capitalize(strings.Join(parts, "; ")) + "."
}
