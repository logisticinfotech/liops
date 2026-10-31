// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// The parsers below read the kernel's text files. Each takes the file's content, so tests
// can feed them samples; the collector (collector.go) reads the files.

// cpuTimes is the first line of /proc/stat, in clock ticks since boot.
type cpuTimes struct {
	user, nice, system, idle, iowait, irq, softirq, steal uint64
}

func (c cpuTimes) total() uint64 {
	return c.user + c.nice + c.system + c.idle + c.iowait + c.irq + c.softirq + c.steal
}

// parseStat reads /proc/stat: the CPU times and the number of CPUs.
func parseStat(s string) (cpuTimes, int, error) {
	var t cpuTimes
	cpus := 0
	found := false
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if f[0] == "cpu" {
			if len(f) < 9 {
				return t, 0, fmt.Errorf("/proc/stat: short cpu line %q", line)
			}
			v := make([]uint64, 8)
			for i := range v {
				v[i], _ = strconv.ParseUint(f[i+1], 10, 64)
			}
			t = cpuTimes{v[0], v[1], v[2], v[3], v[4], v[5], v[6], v[7]}
			found = true
		} else if strings.HasPrefix(f[0], "cpu") {
			cpus++
		}
	}
	if !found {
		return t, 0, fmt.Errorf("/proc/stat: no cpu line")
	}
	return t, max(cpus, 1), nil
}

// parseKeyValues reads "Key:   value kB" (meminfo) or "key value" (vmstat) lines. Values in
// kB are returned in bytes.
func parseKeyValues(s string) map[string]uint64 {
	kv := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(strings.Replace(line, ":", " ", 1))
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseUint(f[1], 10, 64)
		if err != nil {
			continue
		}
		if len(f) >= 3 && f[2] == "kB" {
			v *= 1024
		}
		kv[f[0]] = v
	}
	return kv
}

// parseLoadavg reads /proc/loadavg's 1- and 5-minute load averages.
func parseLoadavg(s string) (load1, load5 float64, err error) {
	f := strings.Fields(s)
	if len(f) < 2 {
		return 0, 0, fmt.Errorf("/proc/loadavg: unexpected %q", s)
	}
	if load1, err = strconv.ParseFloat(f[0], 64); err != nil {
		return 0, 0, err
	}
	load5, err = strconv.ParseFloat(f[1], 64)
	return load1, load5, err
}

// diskIO is one block device's counters from /proc/diskstats.
type diskIO struct {
	readSectors, writeSectors uint64
	busyMillis                uint64 // time spent doing I/O
}

// parseDiskstats reads /proc/diskstats for whole disks (sda, vda, nvme0n1, xvda), not their
// partitions or loop and ram devices.
func parseDiskstats(s string) map[string]diskIO {
	disks := map[string]diskIO{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 14 || !isWholeDisk(f[2]) {
			continue
		}
		num := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		disks[f[2]] = diskIO{readSectors: num(5), writeSectors: num(9), busyMillis: num(12)}
	}
	return disks
}

func isWholeDisk(name string) bool {
	switch {
	case strings.HasPrefix(name, "nvme"):
		return !strings.Contains(name[4:], "p") // nvme0n1, not nvme0n1p1
	case strings.HasPrefix(name, "sd"), strings.HasPrefix(name, "vd"), strings.HasPrefix(name, "xvd"):
		last := name[len(name)-1]
		return last < '0' || last > '9' // sda, not sda1
	}
	return false
}

// netIO is one interface's byte and error counters from /proc/net/dev.
type netIO struct{ rx, tx, errors uint64 }

// parseNetDev reads /proc/net/dev, without the loopback interface.
func parseNetDev(s string) map[string]netIO {
	ifs := map[string]netIO{}
	for _, line := range strings.Split(s, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		name = strings.TrimSpace(name)
		if !ok || name == "lo" || strings.HasPrefix(name, "Inter") || strings.Contains(name, "|") {
			continue
		}
		f := strings.Fields(rest)
		if len(f) < 16 {
			continue
		}
		num := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		ifs[name] = netIO{rx: num(0), tx: num(8), errors: num(2) + num(10)}
	}
	return ifs
}

// realFilesystems are the filesystem types worth watching for space.
var realFilesystems = map[string]bool{"ext4": true, "ext3": true, "xfs": true, "btrfs": true, "zfs": true, "f2fs": true, "vfat": false}

// parseMounts reads /proc/mounts and returns the mount points of real filesystems, each once.
func parseMounts(s string) []string {
	var points []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || !realFilesystems[f[2]] || seen[f[0]] {
			continue
		}
		seen[f[0]] = true // a device mounted twice (bind mounts) counts once
		points = append(points, unescapeMount(f[1]))
	}
	return points
}

// unescapeMount undoes /proc/mounts' octal escapes (\040 for a space).
func unescapeMount(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// procStat is one process's CPU time and resident memory.
type procStat struct {
	comm      string
	ticks     uint64 // user + system clock ticks
	rssPages  uint64
	startTick uint64 // to tell a reused PID apart
}

// parseProcStat reads /proc/<pid>/stat. The command name is in parentheses and may itself
// contain spaces and parentheses, so fields are counted from the last ')'.
func parseProcStat(s string) (procStat, error) {
	open, close := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || close < open {
		return procStat{}, fmt.Errorf("stat: unexpected %q", s)
	}
	f := strings.Fields(s[close+1:])
	if len(f) < 22 {
		return procStat{}, fmt.Errorf("stat: short %q", s)
	}
	num := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
	// After ')': state(0) … utime(11) stime(12) … starttime(19) vsize(20) rss(21).
	return procStat{comm: s[open+1 : close], ticks: num(11) + num(12), startTick: num(19), rssPages: num(21)}, nil
}

// commandGroup names a process for grouping in "top processes": the program and, for
// interpreters and launchers, the script or subcommand that tells processes apart (php
// artisan queue:work, node server.js, python manage.py).
func commandGroup(comm string, cmdline []string) string {
	if len(cmdline) == 0 || cmdline[0] == "" {
		// Kernel threads have no command line; "kworker/u8:0-flush-8:0" counts as "kworker".
		name, _, _ := strings.Cut(comm, "/")
		return name
	}
	base := func(p string) string {
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			return p[i+1:]
		}
		return p
	}
	prog := base(cmdline[0])
	// "php-fpm: pool www" and similar set their own title in one argument.
	if strings.Contains(prog, ": ") {
		return prog
	}
	args := cmdline[1:]
	switch {
	case strings.HasPrefix(prog, "php"), strings.HasPrefix(prog, "python"), prog == "node", prog == "ruby", prog == "perl", prog == "bash", prog == "sh":
		var parts []string
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				continue
			}
			parts = append(parts, base(a))
			if len(parts) == 2 {
				break
			}
		}
		return strings.TrimSpace(prog + " " + strings.Join(parts, " "))
	}
	return prog
}
