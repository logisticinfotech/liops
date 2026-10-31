// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// clockTicks is the kernel's USER_HZ: the unit of CPU times in /proc. It is 100 on every
// Linux liops supports.
const clockTicks = 100

// Sample is one collection: series name → value. Names are "metric" or "metric:label",
// e.g. "cpu.iowait", "disk.pct:/var", "svc.mem:nginx.service".
type Sample struct {
	Time   time.Time
	Values map[string]float64
}

// Process is one group of processes running the same command.
type Process struct {
	Command string
	Count   int
	CPU     float64 // percent of one CPU
	Memory  uint64  // resident bytes
}

// Collector reads the machine's numbers from /proc and /sys. Rates (CPU, I/O, network)
// need two readings, so the first Collect returns only levels.
type Collector struct {
	// Root is "/" on a server; tests point it at a folder of sample files.
	Root     string
	pageSize uint64
	prev     *reading
}

// reading is the counters from one Collect, kept to compute the next one's rates.
type reading struct {
	at     time.Time
	cpu    cpuTimes
	vm     map[string]uint64
	disks  map[string]diskIO
	nets   map[string]netIO
	svcCPU map[string]uint64 // usage_usec per service
	procs  map[int]procStat
}

func NewCollector(root string) *Collector {
	return &Collector{Root: root, pageSize: uint64(os.Getpagesize())}
}

func (c *Collector) read(rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(c.Root, rel))
	return string(data), err
}

// Collect takes one sample, and the top process groups by CPU.
func (c *Collector) Collect(now time.Time) (Sample, []Process, error) {
	s := Sample{Time: now, Values: map[string]float64{}}
	r := &reading{at: now}
	v := s.Values

	stat, err := c.read("proc/stat")
	if err != nil {
		return s, nil, fmt.Errorf("monitoring needs Linux's /proc: %w", err)
	}
	var cpus int
	if r.cpu, cpus, err = parseStat(stat); err != nil {
		return s, nil, err
	}
	v["cpus"] = float64(cpus)
	if load, err := c.read("proc/loadavg"); err == nil {
		if l1, l5, err := parseLoadavg(load); err == nil {
			v["load.1"], v["load.5"] = l1, l5
		}
	}

	if mem, err := c.read("proc/meminfo"); err == nil {
		m := parseKeyValues(mem)
		v["mem.total"] = float64(m["MemTotal"])
		v["mem.available"] = float64(m["MemAvailable"])
		v["mem.used"] = float64(m["MemTotal"] - min(m["MemAvailable"], m["MemTotal"]))
		v["mem.cache"] = float64(m["Cached"] + m["Buffers"])
		v["swap.used"] = float64(m["SwapTotal"] - min(m["SwapFree"], m["SwapTotal"]))
	}
	if vm, err := c.read("proc/vmstat"); err == nil {
		r.vm = parseKeyValues(vm)
	}
	if ds, err := c.read("proc/diskstats"); err == nil {
		r.disks = parseDiskstats(ds)
	}
	if nd, err := c.read("proc/net/dev"); err == nil {
		r.nets = parseNetDev(nd)
	}
	if mounts, err := c.read("proc/mounts"); err == nil {
		for _, point := range parseMounts(mounts) {
			var st syscall.Statfs_t
			if err := syscall.Statfs(filepath.Join(c.Root, point), &st); err != nil {
				continue
			}
			size := uint64(st.Blocks) * uint64(st.Bsize)
			if size == 0 {
				continue
			}
			used := size - uint64(st.Bavail)*uint64(st.Bsize)
			v["disk.used:"+point] = float64(used)
			v["disk.size:"+point] = float64(size)
			v["disk.pct:"+point] = float64(used) * 100 / float64(size)
		}
	}
	svcMem := c.services(r)
	for name, mem := range svcMem {
		v["svc.mem:"+name] = float64(mem)
	}
	r.procs = c.processes()

	var top []Process
	if p := c.prev; p != nil {
		dt := now.Sub(p.at).Seconds()
		if dt > 0 {
			c.rates(v, p, r, dt)
			top = c.topProcesses(p, r, dt)
		}
	}
	c.prev = r
	return s, top, nil
}

// rates adds everything measured between two readings.
func (c *Collector) rates(v map[string]float64, p, r *reading, dt float64) {
	if total := float64(r.cpu.total() - p.cpu.total()); total > 0 {
		pct := func(now, before uint64) float64 { return float64(now-before) * 100 / total }
		v["cpu.user"] = pct(r.cpu.user+r.cpu.nice, p.cpu.user+p.cpu.nice)
		v["cpu.system"] = pct(r.cpu.system+r.cpu.irq+r.cpu.softirq, p.cpu.system+p.cpu.irq+p.cpu.softirq)
		v["cpu.iowait"] = pct(r.cpu.iowait, p.cpu.iowait)
		v["cpu.steal"] = pct(r.cpu.steal, p.cpu.steal)
		v["cpu.busy"] = 100 - pct(r.cpu.idle+r.cpu.iowait, p.cpu.idle+p.cpu.iowait)
	}
	if r.vm != nil && p.vm != nil {
		v["swap.in"] = float64(r.vm["pswpin"]-min(p.vm["pswpin"], r.vm["pswpin"])) * float64(c.pageSize) / dt
		v["swap.out"] = float64(r.vm["pswpout"]-min(p.vm["pswpout"], r.vm["pswpout"])) * float64(c.pageSize) / dt
		v["oom.kills"] = float64(r.vm["oom_kill"] - min(p.vm["oom_kill"], r.vm["oom_kill"]))
	}
	for name, d := range r.disks {
		if b, ok := p.disks[name]; ok {
			v["io.read:"+name] = float64(d.readSectors-min(b.readSectors, d.readSectors)) * 512 / dt
			v["io.write:"+name] = float64(d.writeSectors-min(b.writeSectors, d.writeSectors)) * 512 / dt
			v["io.util:"+name] = min(100, float64(d.busyMillis-min(b.busyMillis, d.busyMillis))/(dt*10))
		}
	}
	for name, n := range r.nets {
		if b, ok := p.nets[name]; ok {
			v["net.rx:"+name] = float64(n.rx-min(b.rx, n.rx)) / dt
			v["net.tx:"+name] = float64(n.tx-min(b.tx, n.tx)) / dt
			v["net.errors:"+name] = float64(n.errors - min(b.errors, n.errors))
		}
	}
	for name, usec := range r.svcCPU {
		if b, ok := p.svcCPU[name]; ok {
			v["svc.cpu:"+name] = float64(usec-min(b, usec)) / (dt * 1e6) * 100
		}
	}
}

// services reads each systemd service's cgroup (v2): CPU time, kept for the next rate, and
// memory, returned.
func (c *Collector) services(r *reading) map[string]uint64 {
	r.svcCPU = map[string]uint64{}
	mem := map[string]uint64{}
	dir := filepath.Join(c.Root, "sys/fs/cgroup/system.slice")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return mem
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasSuffix(name, ".service") {
			continue
		}
		cur, err := os.ReadFile(filepath.Join(dir, name, "memory.current"))
		if err != nil {
			continue
		}
		m, _ := strconv.ParseUint(strings.TrimSpace(string(cur)), 10, 64)
		if m == 0 {
			continue // not running
		}
		mem[name] = m
		if st, err := os.ReadFile(filepath.Join(dir, name, "cpu.stat")); err == nil {
			r.svcCPU[name] = parseKeyValues(string(st))["usage_usec"]
		}
	}
	return mem
}

// processes reads every process's stat and command line.
func (c *Collector) processes() map[int]procStat {
	procs := map[int]procStat{}
	entries, err := os.ReadDir(filepath.Join(c.Root, "proc"))
	if err != nil {
		return procs
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := c.read("proc/" + e.Name() + "/stat")
		if err != nil {
			continue // gone already
		}
		ps, err := parseProcStat(data)
		if err != nil {
			continue
		}
		cmd, _ := c.read("proc/" + e.Name() + "/cmdline")
		ps.comm = commandGroup(ps.comm, strings.Split(strings.TrimRight(cmd, "\x00"), "\x00"))
		procs[pid] = ps
	}
	return procs
}

// topProcesses groups processes by command and returns the 15 busiest by CPU, then memory.
func (c *Collector) topProcesses(p, r *reading, dt float64) []Process {
	groups := map[string]*Process{}
	for pid, ps := range r.procs {
		g := groups[ps.comm]
		if g == nil {
			g = &Process{Command: ps.comm}
			groups[ps.comm] = g
		}
		g.Count++
		g.Memory += ps.rssPages * c.pageSize
		if b, ok := p.procs[pid]; ok && b.startTick == ps.startTick && ps.ticks >= b.ticks {
			g.CPU += float64(ps.ticks-b.ticks) / clockTicks / dt * 100
		}
	}
	list := make([]Process, 0, len(groups))
	for _, g := range groups {
		if g.Memory > 0 || g.CPU > 0 {
			list = append(list, *g)
		}
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].CPU != list[j].CPU {
			return list[i].CPU > list[j].CPU
		}
		return list[i].Memory > list[j].Memory
	})
	return list[:min(len(list), 15)]
}
