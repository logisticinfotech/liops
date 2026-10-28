// SPDX-License-Identifier: Apache-2.0

package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

var dowNames = []string{"Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"}

// DayPreset is a day-of-week choice for the schedule UI (stored as the cron DOW field).
type DayPreset struct {
	Value string
	Label string
}

// DayPresets are the choices shown in Settings / setup. Values are cron day-of-week fields.
var DayPresets = []DayPreset{
	{"*", "Every day"},
	{"1-5", "Monday–Friday"},
	{"1-6", "Monday–Saturday"},
	{"0,6", "Saturday and Sunday"},
	{"0", "Sunday only"},
	{"1", "Monday only"},
	{"2", "Tuesday only"},
	{"3", "Wednesday only"},
	{"4", "Thursday only"},
	{"5", "Friday only"},
	{"6", "Saturday only"},
}

// DayPresetsFor returns DayPresets, plus the current days value if it is not already listed.
func DayPresetsFor(days string) []DayPreset {
	days = normalizeDowField(days)
	for _, p := range DayPresets {
		if p.Value == days {
			return DayPresets
		}
	}
	if days == "" {
		return DayPresets
	}
	label := describeDow(days)
	if label == "" {
		label = days
	}
	return append([]DayPreset{{Value: days, Label: label + " (current)"}}, DayPresets...)
}

// FriendlySchedule is a cron spec broken into day + clock time for the UI.
type FriendlySchedule struct {
	Time string // "01:30"
	Days string // cron day-of-week field, e.g. "0" or "1-6"
	OK   bool   // false → show the raw cron field instead
	Spec string // original cron
}

// ParseFriendly turns a 5-field cron into day+time when it is a simple weekly/daily clock time.
func ParseFriendly(spec string) FriendlySchedule {
	out := FriendlySchedule{Spec: spec}
	fields := strings.Fields(spec)
	if len(fields) != 5 || fields[2] != "*" || fields[3] != "*" {
		return out
	}
	mi, err1 := strconv.Atoi(fields[0])
	hr, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil || mi < 0 || mi > 59 || hr < 0 || hr > 23 {
		return out
	}
	days := normalizeDowField(fields[4])
	if days != "*" && describeDow(days) == "" {
		return out
	}
	out.Time = fmt.Sprintf("%02d:%02d", hr, mi)
	out.Days = days
	out.OK = true
	return out
}

// BuildCron builds a 5-field cron from a clock time ("01:30") and a day-of-week field ("0", "1-6", "*").
func BuildCron(clock, days string) (string, error) {
	clock = strings.TrimSpace(clock)
	days = normalizeDowField(strings.TrimSpace(days))
	if days == "" {
		return "", fmt.Errorf("days: choose which days this should run")
	}
	if days != "*" && describeDow(days) == "" {
		return "", fmt.Errorf("days: %q is not a valid day choice", days)
	}
	t, err := time.Parse("15:04", clock)
	if err != nil {
		t, err = time.Parse("15:04:05", clock)
	}
	if err != nil {
		return "", fmt.Errorf("time: %q is not a valid time (use HH:MM)", clock)
	}
	return fmt.Sprintf("%d %d * * %s", t.Minute(), t.Hour(), days), nil
}

// DescribeSchedule turns a 5-field cron spec into plain language, e.g.
// "30 1 * * 0" → "every Sunday at 01:30". Empty string if it can't be described.
func DescribeSchedule(spec string) string {
	f := ParseFriendly(spec)
	if !f.OK {
		return ""
	}
	if f.Days == "*" {
		return "every day at " + f.Time
	}
	days := describeDow(f.Days)
	if days == "" {
		return ""
	}
	return "every " + days + " at " + f.Time
}

// ArchiveTimeoutPresets are plain-language WAL interval choices (Value is a Go duration string).
var ArchiveTimeoutPresets = []DayPreset{
	{"15m", "Every 15 minutes"},
	{"30m", "Every 30 minutes"},
	{"1h", "Every 1 hour"},
	{"2h", "Every 2 hours"},
	{"6h", "Every 6 hours"},
	{"12h", "Every 12 hours"},
}

// BinlogIntervalPresets are plain-language choices for mysql.binlog_interval.
var BinlogIntervalPresets = []DayPreset{
	{"1m", "Every minute"},
	{"5m", "Every 5 minutes"},
	{"15m", "Every 15 minutes"},
	{"30m", "Every 30 minutes"},
	{"1h", "Every hour"},
}

// HasArchiveTimeoutPreset reports whether d matches a preset value.
func HasArchiveTimeoutPreset(d Duration) bool { return hasPreset(ArchiveTimeoutPresets, d) }

// HasBinlogIntervalPreset reports whether d matches a preset value.
func HasBinlogIntervalPreset(d Duration) bool { return hasPreset(BinlogIntervalPresets, d) }

func hasPreset(presets []DayPreset, d Duration) bool {
	s := d.String()
	for _, p := range presets {
		if p.Value == s {
			return true
		}
	}
	return false
}

func normalizeDowField(s string) string {
	if s == "7" {
		return "0"
	}
	return s
}

func describeDow(expr string) string {
	if strings.ContainsAny(expr, "*/") {
		return ""
	}
	if lo, hi, ok := strings.Cut(expr, "-"); ok {
		a, ok1 := parseDow(lo)
		b, ok2 := parseDow(hi)
		if !ok1 || !ok2 || a > b {
			return ""
		}
		if a == b {
			return dowNames[a]
		}
		return dowNames[a] + "–" + dowNames[b]
	}
	parts := strings.Split(expr, ",")
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		d, ok := parseDow(p)
		if !ok {
			return ""
		}
		names = append(names, dowNames[d])
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	case 2:
		return names[0] + " and " + names[1]
	default:
		return strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
	}
}

func parseDow(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	if n == 7 {
		n = 0
	}
	if n < 0 || n > 6 {
		return 0, false
	}
	return n, true
}
