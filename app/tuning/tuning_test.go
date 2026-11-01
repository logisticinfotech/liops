// SPDX-License-Identifier: Apache-2.0

package tuning

import (
	"testing"
)

func byID(rs []Recommendation) map[string]Recommendation {
	out := map[string]Recommendation{}
	for _, r := range rs {
		out[r.ID] = r
	}
	return out
}

// Ubuntu's defaults on an 8 GB server running everything: MySQL's 128 MB buffer pool for
// 3 GB of data, a 5-worker pool that's always busy, a full OPcache.
func defaults() Facts {
	return Facts{MemTotal: 8 << 30, MemAvailable: 6 << 30, SSD: true,
		MySQL:   &MySQL{BufferPool: 128 << 20, DataBytes: 3 << 30, MaxConnections: 151, MaxUsed: 140, TmpTableSize: 16 << 20, TmpDiskShare: 30, CacheHit: 92},
		Pools:   []Pool{{Version: "8.3", Name: "www", PM: "dynamic", MaxChildren: 5, Workers: 5, WorkerMemory: 64 << 20, BusyPct: 60}},
		OPcache: []OPcache{{Version: "8.3", MemoryMB: 128, MemoryPct: 99, KeysPct: 95, MaxFiles: 10000, StringsMB: 8, StringsPct: 97}},
		Redis:   &Redis{MaxMemory: 256 << 20, Used: 256 << 20, Evicted: 900, Policy: "allkeys-lru", Sessions: true},
	}
}

func TestRecommend(t *testing.T) {
	rs := byID(Recommend(defaults()))
	for id, want := range map[string]string{
		"mysql/innodb_buffer_pool_size":           "3840M",
		"mysql/max_connections":                   "230",
		"mysql/tmp_table_size":                    "64M",
		"pool/8.3/www/pm.max_children":            "14",
		"php/8.3/opcache.memory_consumption":      "256",
		"php/8.3/opcache.max_accelerated_files":   "20000",
		"php/8.3/opcache.interned_strings_buffer": "32",
	} {
		if r, ok := rs[id]; !ok || r.Proposed != want {
			t.Errorf("%s: %+v, want %s", id, r, want)
		}
	}
	if r := rs["pool/8.3/www/pm.max_children"]; r.Extra["pm.max_spare_servers"] == "" {
		t.Error("a dynamic pool's spare servers should move with pm.max_children")
	}
	// The memory: 6 GB available, 1.2 GB kept back. 3.6 GB more buffer pool leaves room for
	// 9 more 64 MB workers (not the 2x10+4 asked), 128 MB more OPcache and 128 MB more Redis.
	if r := rs["redis/maxmemory"]; r.Proposed != "384mb" {
		t.Errorf("redis: %+v", r)
	}
	for _, r := range rs {
		if !Allowed[r.Kind][r.Setting] {
			t.Errorf("%s isn't an allowed setting", r.Setting)
		}
		for k := range r.Extra {
			if !Allowed[r.Kind][k] {
				t.Errorf("%s isn't an allowed setting", k)
			}
		}
		if r.Why == "" || r.How == "" || r.Current == "" {
			t.Errorf("incomplete: %+v", r)
		}
	}
}

func TestMemoryIsNeverOverpromised(t *testing.T) {
	f := defaults()
	f.MemAvailable = 1 << 30 // little left: 1 GB, less the 1.2 GB margin
	f.Postgres = &Postgres{SharedBuffers: 128 << 20, EffectiveCache: 4 << 30, WorkMem: 4 << 20,
		DataBytes: 20 << 30, TempBytesPerHour: 2 << 30, CacheHit: 90}
	rs := Recommend(f)
	for _, r := range rs {
		if r.Setting == "innodb_buffer_pool_size" || r.Setting == "pm.max_children" || r.Setting == "opcache.memory_consumption" || r.Setting == "maxmemory" || r.Setting == "work_mem" || r.Setting == "shared_buffers" {
			t.Errorf("no memory to hand out, yet: %+v", r)
		}
	}
}

func TestPostgres(t *testing.T) {
	f := Facts{MemTotal: 16 << 30, MemAvailable: 12 << 30, SSD: true,
		Postgres: &Postgres{SharedBuffers: 128 << 20, EffectiveCache: 4 << 30, WorkMem: 4 << 20, DataBytes: 20 << 30, RandomPageCost: 4, TempBytesPerHour: 2 << 30, CacheHit: 90}}
	rs := byID(Recommend(f))
	if r := rs["postgres/shared_buffers"]; r.Proposed != "4G" || !r.Restart {
		t.Errorf("shared_buffers: %+v", r)
	}
	if r := rs["postgres/effective_cache_size"]; r.Proposed != "9792M" {
		t.Errorf("effective_cache_size: %+v", r)
	}
	if rs["postgres/random_page_cost"].Proposed != "1.1" || rs["postgres/work_mem"].Proposed != "32MB" {
		t.Errorf("rs = %+v", rs)
	}
	f.SSD = false
	if _, ok := byID(Recommend(f))["postgres/random_page_cost"]; ok {
		t.Error("random_page_cost stays for spinning disks")
	}
}

func TestHealthyServerGetsNothing(t *testing.T) {
	f := Facts{MemTotal: 8 << 30, MemAvailable: 4 << 30,
		MySQL:   &MySQL{BufferPool: 4 << 30, DataBytes: 2 << 30, MaxConnections: 151, MaxUsed: 30, TmpTableSize: 64 << 20, TmpDiskShare: 2, CacheHit: 99.9},
		Pools:   []Pool{{Version: "8.3", Name: "www", MaxChildren: 30, MaxRequests: 500, WorkerMemory: 60 << 20, BusyPct: 2}},
		OPcache: []OPcache{{Version: "8.3", MemoryMB: 256, MemoryPct: 40, KeysPct: 30, MaxFiles: 20000, StringsPct: 40}},
	}
	if rs := Recommend(f); len(rs) != 0 {
		t.Errorf("recommendations for a well-tuned server: %+v", rs)
	}
}
