// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	// CheckInterval is how often the yes/no checks run: failed units, reboot required.
	CheckInterval = 5 * time.Minute
	// DiskUseInterval is how often the folders in DiskUseDirs are measured.
	DiskUseInterval = time.Hour
	// PushKeep is how long a pushed value counts as current. The root service pushes
	// hourly, so a value that's two hours old means it stopped.
	PushKeep = 2 * time.Hour
)

// DiskUseDirs are the folders the monitor measures itself. Database folders aren't
// readable by its user: the root service pushes their sizes instead (Push).
var DiskUseDirs = []string{"/var/log"}

// PushPrefixes are the only series the root service may push.
var PushPrefixes = []string{"db.size:", "disk.use:"}

// Checks are the slower yes/no checks.
type Checks struct {
	Time           time.Time `json:"time"` // zero before the first run
	RebootRequired bool      `json:"reboot_required"`
	RebootPackages []string  `json:"reboot_packages,omitempty"`
	FailedUnits    []string  `json:"failed_units,omitempty"`
	FailedError    string    `json:"failed_error,omitempty"` // systemctl couldn't be asked
}

// held is a value that joins every sample until it expires.
type held struct {
	value   float64
	expires time.Time
}

// hold keeps values in every sample for keep.
func (m *Monitor) hold(values map[string]float64, keep time.Duration) {
	until := m.Now().Add(keep)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.held == nil {
		m.held = map[string]held{}
	}
	for n, v := range values {
		m.held[n] = held{v, until}
	}
}

// heldValues adds the current held values to v. The caller holds m.mu.
func (m *Monitor) heldValues(v map[string]float64, now time.Time) {
	for n, h := range m.held {
		if now.After(h.expires) {
			delete(m.held, n)
			continue
		}
		v[n] = h.value
	}
}

// slowLoop runs the checks and measures DiskUseDirs, apart from sampling so that a slow
// systemctl or a big /var/log never delays a sample.
func (m *Monitor) slowLoop(ctx context.Context) {
	m.runChecks(ctx)
	m.measureDirs()
	checks := time.NewTicker(CheckInterval)
	defer checks.Stop()
	dirs := time.NewTicker(DiskUseInterval)
	defer dirs.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-checks.C:
			m.runChecks(ctx)
		case <-dirs.C:
			m.measureDirs()
		}
	}
}

func (m *Monitor) runChecks(ctx context.Context) {
	c := Checks{Time: m.Now()}
	c.RebootRequired, c.RebootPackages = m.Collector.RebootRequired()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	out, err := m.Systemctl(ctx, "list-units", "--state=failed", "--no-legend", "--plain", "--no-pager")
	if err != nil {
		c.FailedError = strings.TrimSpace(err.Error() + " " + string(out))
		m.logf("systemctl: %s", c.FailedError)
	} else {
		c.FailedUnits = parseFailedUnits(string(out))
	}
	values := map[string]float64{"reboot.required": 0, "units.failed": float64(len(c.FailedUnits))}
	if c.RebootRequired {
		values["reboot.required"] = 1
	}
	if c.FailedError != "" {
		delete(values, "units.failed")
	}
	m.hold(values, 2*CheckInterval)
	m.mu.Lock()
	m.checks = c
	m.mu.Unlock()
}

func (m *Monitor) measureDirs() {
	values := map[string]float64{}
	for _, dir := range DiskUseDirs {
		if size, err := dirSize(filepath.Join(m.Collector.Root, dir)); err == nil {
			values["disk.use:"+dir] = float64(size)
		} else {
			m.logf("disk use of %s: %v", dir, err)
		}
	}
	m.hold(values, 2*DiskUseInterval)
}

// Push records values from the root service (database sizes), keeping them for PushKeep.
func (m *Monitor) Push(values map[string]float64) error {
	if len(values) > 1000 {
		return errors.New("too many values (at most 1000)")
	}
	for n := range values {
		ok := false
		for _, p := range PushPrefixes {
			ok = ok || (strings.HasPrefix(n, p) && len(n) > len(p) && len(n) <= 200)
		}
		if !ok {
			return errors.New("only db.size:… and disk.use:… can be pushed, not " + n)
		}
	}
	m.hold(values, PushKeep)
	return nil
}

// RebootRequired reports Ubuntu's "a restart is needed" flag and the packages behind it.
func (c *Collector) RebootRequired() (bool, []string) {
	if _, err := os.Stat(filepath.Join(c.Root, "run/reboot-required")); err != nil {
		return false, nil
	}
	pkgs, _ := c.read("run/reboot-required.pkgs")
	seen := map[string]bool{}
	var out []string
	for _, p := range strings.Fields(pkgs) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return true, out
}

// parseFailedUnits reads `systemctl list-units --state=failed --no-legend --plain`.
func parseFailedUnits(out string) []string {
	var units []string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(strings.TrimPrefix(strings.TrimSpace(line), "●"))
		if len(f) > 0 {
			units = append(units, f[0])
		}
	}
	return units
}

// dirSize is the space the files under dir take on its filesystem (like du -s), skipping
// what the monitor's user can't read and other filesystems mounted inside.
func dirSize(dir string) (uint64, error) {
	var root syscall.Stat_t
	if err := syscall.Lstat(dir, &root); err != nil {
		return 0, err
	}
	var total uint64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() && path != dir {
				return fs.SkipDir
			}
			return nil
		}
		var st syscall.Stat_t
		if syscall.Lstat(path, &st) != nil {
			return nil
		}
		if uint64(st.Dev) != uint64(root.Dev) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		total += uint64(st.Blocks) * 512
		return nil
	})
	return total, err
}

// systemctl runs the real systemctl.
func systemctl(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "systemctl", args...).CombinedOutput()
}
