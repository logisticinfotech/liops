// SPDX-License-Identifier: Apache-2.0

// Package tuning recommends settings for PHP-FPM, OPcache, MySQL, PostgreSQL and Redis
// from what the server shows (docs/PLAN-monitor.md §5.1). It only advises: a person
// approves each change, and app/setup applies it. Memory is planned together, against
// the server's RAM, so liops never recommends more than there is.
package tuning

import (
	"fmt"
	"math"
	"sort"
	"strconv"
)

// Kinds of setting, which decide how a change is written, checked and applied.
const (
	KindMySQL    = "mysql"    // zz-liops-tuning.cnf, SET GLOBAL online
	KindPostgres = "postgres" // conf.d/30-liops-tuning.conf, reload (or restart)
	KindPHP      = "php"      // fpm/conf.d/zz-liops-tuning.ini, php-fpm reload
	KindPool     = "pool"     // the pool file itself, php-fpm reload
	KindRedis    = "redis"    // CONFIG SET, CONFIG REWRITE
)

// Allowed are the only settings liops will ever write, per kind. Durability (fsync,
// innodb_flush_log_at_trx_commit, sync_binlog, synchronous_commit), replication,
// authentication and network exposure are never among them.
var Allowed = map[string]map[string]bool{
	KindMySQL:    {"innodb_buffer_pool_size": true, "innodb_redo_log_capacity": true, "max_connections": true, "tmp_table_size": true, "max_heap_table_size": true, "table_open_cache": true},
	KindPostgres: {"shared_buffers": true, "effective_cache_size": true, "work_mem": true, "maintenance_work_mem": true, "random_page_cost": true, "effective_io_concurrency": true, "max_wal_size": true},
	KindPHP:      {"opcache.memory_consumption": true, "opcache.interned_strings_buffer": true, "opcache.max_accelerated_files": true, "realpath_cache_size": true},
	KindPool:     {"pm.max_children": true, "pm.max_requests": true, "pm.start_servers": true, "pm.min_spare_servers": true, "pm.max_spare_servers": true},
	KindRedis:    {"maxmemory": true},
}

// Recommendation is one proposed change.
type Recommendation struct {
	ID        string // stable, e.g. "mysql/innodb_buffer_pool_size"
	Kind      string
	Target    string // the PHP version ("8.3") or pool ("8.3/www"); empty otherwise
	Component string // for people: "MySQL", "PHP 8.3 OPcache", "PHP-FPM pool 8.3/www"
	Setting   string
	Current   string
	Proposed  string
	Why       string // the numbers
	Effect    string
	How       string // how it would be applied
	Restart   bool   // it only takes effect after a restart, which a person starts
	// Extra are settings changed with this one (max_heap_table_size with tmp_table_size,
	// the spare servers with pm.max_children).
	Extra map[string]string
}

// Facts are what the recommendations are made from.
type Facts struct {
	MemTotal, MemAvailable float64
	SSD                    bool
	MySQL                  *MySQL
	Postgres               *Postgres
	Pools                  []Pool
	OPcache                []OPcache
	Redis                  *Redis
}

// MySQL is MySQL's current settings and load.
type MySQL struct {
	BufferPool, DataBytes   float64
	MaxConnections, MaxUsed float64 // Max_used_connections since the start
	TmpTableSize            float64
	TmpDiskShare            float64 // percent of temporary tables that went to disk
	CacheHit                float64
}

// Postgres is PostgreSQL's current settings and load.
type Postgres struct {
	SharedBuffers, EffectiveCache, WorkMem, DataBytes float64
	RandomPageCost                                    float64
	TempBytesPerHour                                  float64 // sorts and hashes spilling to disk
	CacheHit                                          float64
}

// Pool is one PHP-FPM pool.
type Pool struct {
	Version, Name                     string
	PM                                string
	MaxChildren, MaxRequests, Workers int
	WorkerMemory                      float64 // average resident memory
	BusyPct, Reached                  float64
}

// OPcache is one PHP version's OPcache.
type OPcache struct {
	Version               string
	MemoryMB, MemoryPct   float64
	KeysPct, MaxFiles     float64
	StringsMB, StringsPct float64
	Restarts              float64
}

// Redis is Redis's memory and whether sessions live in it.
type Redis struct {
	MaxMemory, Used, Evicted float64
	Policy                   string
	Sessions                 bool
}

// budget is the memory the recommendations may hand out: what's available, less a
// margin for the OS cache and spikes. Each recommendation that takes memory charges it.
type budget struct{ left float64 }

func (b *budget) take(n float64) bool {
	if n <= b.left {
		b.left -= n
		return true
	}
	return false
}

// Recommend makes the recommendations, the biggest wins first: the databases' memory,
// PHP's workers, OPcache, then Redis.
func Recommend(f Facts) []Recommendation {
	var out []Recommendation
	b := &budget{left: math.Max(0, f.MemAvailable-math.Max(0.15*f.MemTotal, 512<<20))}

	if m := f.MySQL; m != nil {
		if m.DataBytes > 0.9*m.BufferPool && m.CacheHit < 99 {
			want := roundUp(m.DataBytes*1.25, 128<<20)
			if want > m.BufferPool && !b.take(want-m.BufferPool) {
				want = roundDown(m.BufferPool+b.left, 128<<20)
				b.take(want - m.BufferPool)
			}
			if want >= m.BufferPool+128<<20 {
				out = append(out, Recommendation{ID: "mysql/innodb_buffer_pool_size", Kind: KindMySQL, Component: "MySQL", Setting: "innodb_buffer_pool_size",
					Current: mb(m.BufferPool), Proposed: mb(want),
					Why:    fmt.Sprintf("The data and indexes are %s, but only %s fit in the buffer pool; %.1f%% of reads come from memory.", gb(m.DataBytes), gb(m.BufferPool), m.CacheHit),
					Effect: "More of the data stays in memory: fewer disk reads, faster queries.",
					How:    "Written to zz-liops-tuning.cnf and resized online (SET GLOBAL): no restart."})
			}
		}
		if m.MaxConnections > 0 && m.MaxUsed >= 0.85*m.MaxConnections {
			want := math.Ceil(m.MaxConnections*1.5/10) * 10
			out = append(out, Recommendation{ID: "mysql/max_connections", Kind: KindMySQL, Component: "MySQL", Setting: "max_connections",
				Current: num(m.MaxConnections), Proposed: num(want),
				Why:    fmt.Sprintf("Up to %.0f of the %.0f allowed connections were in use at once; at the limit new ones fail with \"Too many connections\".", m.MaxUsed, m.MaxConnections),
				Effect: "Room for bursts of PHP workers and queue workers.",
				How:    "Written to zz-liops-tuning.cnf and set online (SET GLOBAL)."})
		}
		if m.TmpDiskShare >= 10 && m.TmpTableSize < 64<<20 {
			out = append(out, Recommendation{ID: "mysql/tmp_table_size", Kind: KindMySQL, Component: "MySQL", Setting: "tmp_table_size",
				Current: mb(m.TmpTableSize), Proposed: "64M", Extra: map[string]string{"max_heap_table_size": "64M"},
				Why:    fmt.Sprintf("%.0f%% of the temporary tables queries need (GROUP BY, ORDER BY, DISTINCT) went to disk.", m.TmpDiskShare),
				Effect: "Larger temporary tables stay in memory. It's per query, so it only uses the memory while one runs.",
				How:    "With max_heap_table_size (the smaller of the two wins), in zz-liops-tuning.cnf and online."})
		}
	}

	if p := f.Postgres; p != nil {
		if p.SharedBuffers < 0.25*f.MemTotal && p.DataBytes > p.SharedBuffers && p.CacheHit < 99 {
			want := roundDown(math.Min(0.25*f.MemTotal, p.DataBytes*1.2), 64<<20)
			if want > p.SharedBuffers && !b.take(want-p.SharedBuffers) {
				want = roundDown(p.SharedBuffers+b.left, 64<<20)
				b.take(want - p.SharedBuffers)
			}
			if want >= p.SharedBuffers+64<<20 {
				out = append(out, Recommendation{ID: "postgres/shared_buffers", Kind: KindPostgres, Component: "PostgreSQL", Setting: "shared_buffers",
					Current: mb(p.SharedBuffers), Proposed: mb(want), Restart: true,
					Why:    fmt.Sprintf("The data is %s and shared_buffers is %s; %.1f%% of reads come from PostgreSQL's own cache.", gb(p.DataBytes), gb(p.SharedBuffers), p.CacheHit),
					Effect: "More of the data in PostgreSQL's cache (about a quarter of RAM is the usual size; the OS cache holds more).",
					How:    "Written to conf.d/30-liops-tuning.conf; it takes effect after a restart, which you start when it suits."})
			}
		}
		if want := 0.6 * f.MemTotal; p.EffectiveCache < 0.5*want {
			out = append(out, Recommendation{ID: "postgres/effective_cache_size", Kind: KindPostgres, Component: "PostgreSQL", Setting: "effective_cache_size",
				Current: mb(p.EffectiveCache), Proposed: mb(roundDown(want, 64<<20)),
				Why:    fmt.Sprintf("It's set to %s on a server with %s of RAM, most of which the OS uses as a file cache.", gb(p.EffectiveCache), gb(f.MemTotal)),
				Effect: "A hint, not memory used: PostgreSQL chooses index scans more readily.",
				How:    "Written to conf.d/30-liops-tuning.conf and reloaded: no restart."})
		}
		if f.SSD && p.RandomPageCost >= 2 {
			out = append(out, Recommendation{ID: "postgres/random_page_cost", Kind: KindPostgres, Component: "PostgreSQL", Setting: "random_page_cost",
				Current: strconv.FormatFloat(p.RandomPageCost, 'f', -1, 64), Proposed: "1.1",
				Why:    "The disk is an SSD, where random reads cost about the same as sequential ones; the default (4) is for spinning disks.",
				Effect: "PostgreSQL uses indexes where they help, instead of reading whole tables.",
				How:    "Written to conf.d/30-liops-tuning.conf and reloaded."})
		}
		const workMemWant = 32 << 20
		if p.TempBytesPerHour >= 100<<20 && p.WorkMem < workMemWant && f.MemTotal >= 4<<30 &&
			b.take(workMemWant-p.WorkMem) {
			out = append(out, Recommendation{ID: "postgres/work_mem", Kind: KindPostgres, Component: "PostgreSQL", Setting: "work_mem",
				Current: mb(p.WorkMem), Proposed: "32MB",
				Why:    fmt.Sprintf("Sorts and hashes spilled %s an hour to temporary files.", gb(p.TempBytesPerHour)),
				Effect: "They stay in memory. It's per sort, per query: it's raised modestly.",
				How:    "Written to conf.d/30-liops-tuning.conf and reloaded."})
		}
	}

	for _, p := range f.Pools {
		label := p.Version + "/" + p.Name
		if (p.BusyPct >= 10 || p.Reached > 0) && p.WorkerMemory > 0 {
			extra := 0
			for n := p.MaxChildren + 1; n <= p.MaxChildren*2+4; n++ {
				if !b.take(p.WorkerMemory) {
					break
				}
				extra++
			}
			if want := p.MaxChildren + extra; extra > 0 {
				r := Recommendation{ID: "pool/" + label + "/pm.max_children", Kind: KindPool, Target: label, Component: "PHP-FPM pool " + label, Setting: "pm.max_children",
					Current: strconv.Itoa(p.MaxChildren), Proposed: strconv.Itoa(want),
					Why:    fmt.Sprintf("All %d workers were busy in %.0f%% of the readings; each takes about %s, and the memory free allows %d more.", p.MaxChildren, p.BusyPct, mb(p.WorkerMemory), extra),
					Effect: "Requests stop waiting for a free worker.",
					How:    "Changed in the pool's file (a copy kept), checked with php-fpm -t, then PHP-FPM reloaded."}
				if p.PM == "dynamic" {
					r.Extra = map[string]string{"pm.start_servers": strconv.Itoa(max(2, want/4)), "pm.min_spare_servers": strconv.Itoa(max(1, want/8)), "pm.max_spare_servers": strconv.Itoa(max(3, want/4))}
				}
				out = append(out, r)
			}
		}
		if p.MaxRequests == 0 && p.WorkerMemory >= 128<<20 {
			out = append(out, Recommendation{ID: "pool/" + label + "/pm.max_requests", Kind: KindPool, Target: label, Component: "PHP-FPM pool " + label, Setting: "pm.max_requests",
				Current: "0 (never)", Proposed: "500",
				Why:    fmt.Sprintf("Workers average %s and are never replaced: memory leaks in long-lived workers add up.", mb(p.WorkerMemory)),
				Effect: "Each worker is replaced after 500 requests, freeing what it leaked.",
				How:    "Changed in the pool's file, checked, PHP-FPM reloaded."})
		}
	}

	for _, o := range f.OPcache {
		comp := "PHP " + o.Version + " OPcache"
		if o.MemoryPct >= 90 && o.MemoryMB > 0 {
			want := math.Min(512, o.MemoryMB*2)
			if want > o.MemoryMB && b.take((want-o.MemoryMB)*(1<<20)) {
				out = append(out, Recommendation{ID: "php/" + o.Version + "/opcache.memory_consumption", Kind: KindPHP, Target: o.Version, Component: comp, Setting: "opcache.memory_consumption",
					Current: num(o.MemoryMB), Proposed: num(want),
					Why:    fmt.Sprintf("OPcache's %.0f MB is %.0f%% used; files that don't fit are compiled on every request.", o.MemoryMB, o.MemoryPct),
					Effect: "Every file stays compiled.", How: "Written to fpm/conf.d/zz-liops-tuning.ini, checked, PHP-FPM reloaded."})
			}
		}
		if o.KeysPct >= 90 && o.MaxFiles > 0 {
			want := 20000.0
			for _, n := range []float64{20000, 32531, 65407, 130987} {
				if n > o.MaxFiles*1.5 {
					want = n
					break
				}
			}
			out = append(out, Recommendation{ID: "php/" + o.Version + "/opcache.max_accelerated_files", Kind: KindPHP, Target: o.Version, Component: comp, Setting: "opcache.max_accelerated_files",
				Current: num(o.MaxFiles), Proposed: num(want),
				Why:    fmt.Sprintf("%.0f%% of OPcache's file slots are used (a Laravel app with its vendor/ has tens of thousands of files).", o.KeysPct),
				Effect: "Every file can be cached.", How: "Written to fpm/conf.d/zz-liops-tuning.ini, checked, PHP-FPM reloaded."})
		}
		if o.StringsPct >= 90 {
			cur := "unknown"
			if o.StringsMB > 0 {
				cur = num(o.StringsMB)
			}
			if o.StringsMB < 32 {
				out = append(out, Recommendation{ID: "php/" + o.Version + "/opcache.interned_strings_buffer", Kind: KindPHP, Target: o.Version, Component: comp, Setting: "opcache.interned_strings_buffer",
					Current: cur, Proposed: "32",
					Why:    fmt.Sprintf("The interned strings buffer is %.0f%% full: when it fills, OPcache stops caching new files.", o.StringsPct),
					Effect: "Room for the framework's strings.", How: "Written to fpm/conf.d/zz-liops-tuning.ini, checked, PHP-FPM reloaded."})
			}
		}
	}

	if r := f.Redis; r != nil && r.Sessions && r.Evicted > 0 && r.MaxMemory > 0 {
		want := roundUp(r.MaxMemory*1.5, 64<<20)
		if b.take(want - r.MaxMemory) {
			out = append(out, Recommendation{ID: "redis/maxmemory", Kind: KindRedis, Component: "Redis", Setting: "maxmemory",
				// Redis reads "m" as 10^6 and "mb" as 2^20.
				Current: redisMB(r.MaxMemory), Proposed: redisMB(want),
				Why:    fmt.Sprintf("Redis holds the sessions and evicted %.0f keys in the last day with maxmemory at %s: people get logged out.", r.Evicted, gb(r.MaxMemory)),
				Effect: "Room for the cache and the sessions. A separate Redis database (or instance) with noeviction for sessions is the lasting fix.",
				How:    "CONFIG SET, then CONFIG REWRITE so it survives a restart."})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return order(out[i].Kind) < order(out[j].Kind) })
	return out
}

func order(kind string) int {
	return map[string]int{KindMySQL: 0, KindPostgres: 1, KindPool: 2, KindPHP: 3, KindRedis: 4}[kind]
}

func roundUp(v, unit float64) float64   { return math.Ceil(v/unit) * unit }
func roundDown(v, unit float64) float64 { return math.Floor(v/unit) * unit }

// mb prints bytes the way the configs take them: 1536M, 4G.
func mb(v float64) string {
	if v >= 1<<30 && math.Mod(v, 1<<30) == 0 {
		return fmt.Sprintf("%.0fG", v/(1<<30))
	}
	return fmt.Sprintf("%.0fM", v/(1<<20))
}

func redisMB(v float64) string { return fmt.Sprintf("%.0fmb", v/(1<<20)) }

func gb(v float64) string {
	if v >= 1<<30 {
		return fmt.Sprintf("%.1f GB", v/(1<<30))
	}
	return fmt.Sprintf("%.0f MB", v/(1<<20))
}

func num(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
