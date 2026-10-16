// SPDX-License-Identifier: Apache-2.0

package config

import (
	"testing"
	"time"
)

func TestDescribeSchedule(t *testing.T) {
	cases := map[string]string{
		"30 1 * * 0":   "every Sunday at 01:30",
		"30 1 * * 1-6": "every Monday–Saturday at 01:30",
		"30 7 * * *":   "every day at 07:30",
		"0 2 * * 1,5":  "every Monday and Friday at 02:00",
		"bad":          "",
		"30 1 * * */2": "",
	}
	for spec, want := range cases {
		if got := DescribeSchedule(spec); got != want {
			t.Errorf("DescribeSchedule(%q) = %q, want %q", spec, got, want)
		}
	}
}

func TestParseFriendlyAndBuildCron(t *testing.T) {
	f := ParseFriendly("30 1 * * 0")
	if !f.OK || f.Time != "01:30" || f.Days != "0" {
		t.Fatalf("ParseFriendly = %+v", f)
	}
	got, err := BuildCron(f.Time, f.Days)
	if err != nil || got != "30 1 * * 0" {
		t.Fatalf("BuildCron = %q, %v", got, err)
	}
	got, err = BuildCron("07:30", "*")
	if err != nil || got != "30 7 * * *" {
		t.Fatalf("BuildCron daily = %q, %v", got, err)
	}
	if ParseFriendly("15 3 1 * *").OK {
		t.Fatal("monthly cron should not be friendly")
	}
}

func TestDurationString(t *testing.T) {
	cases := map[time.Duration]string{
		time.Hour:                 "1h",
		15 * time.Minute:          "15m",
		90 * time.Minute:          "1h30m",
		2*time.Hour + time.Second: "2h1s",
		0:                         "0s",
	}
	for d, want := range cases {
		if got := Duration(d).String(); got != want {
			t.Errorf("Duration(%v).String() = %q, want %q", d, got, want)
		}
	}
}
