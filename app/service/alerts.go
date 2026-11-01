// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/notify"
)

// How often the alerts are checked, and the least time between two of the same kind.
var (
	alertEvery = time.Minute
	alertQuiet = 6 * time.Hour
)

// alertInput is what the immediate alerts look at.
type alertInput struct {
	Disks          []alertDisk
	OOMKills       float64 // in the last 5 minutes
	C5xx           float64 // 5xx responses in the last 5 minutes
	Requests       float64 // requests in the last 5 minutes
	WAL, WALDayAgo float64 // pg_wal now and a day ago, bytes
}

type alertDisk struct {
	Mount  string
	Pct    float64
	PerDay float64 // growth, bytes a day
	Free   float64 // bytes
}

// alert is one immediate email.
type alert struct {
	Kind, Headline, Body string
}

// judgeAlerts finds what can't wait for the daily summary: a disk about to fill, the
// kernel killing processes for memory, a burst of server errors, PostgreSQL's WAL piling up.
func judgeAlerts(in alertInput) []alert {
	var out []alert
	for _, d := range in.Disks {
		days := -1.0
		if d.PerDay > 0 {
			days = d.Free / d.PerDay
		}
		if d.Pct >= 95 || (days >= 0 && days <= 3) {
			when := "it keeps growing"
			if days >= 0 {
				when = fmt.Sprintf("at the last day's rate it's full in about %.0f hours", max(1, days*24))
			}
			out = append(out, alert{Kind: "disk:" + d.Mount, Headline: d.Mount + " is almost full",
				Body: fmt.Sprintf("%s is %.0f%% full, with %s free; %s.\n\nAt 100%% writes fail: the database stops, uploads and logs fail.\n\nWhat to do: see what grows on Monitoring → Resources (databases, WAL or binlogs, logs), then clean up or grow the disk.", d.Mount, d.Pct, notify.Bytes(int64(d.Free)), when)})
		}
	}
	if in.OOMKills > 0 {
		out = append(out, alert{Kind: "oom", Headline: "the kernel is killing processes for memory",
			Body: fmt.Sprintf("The server ran out of memory and the kernel killed %.0f process(es) in the last 5 minutes: PHP workers, a queue worker or the database may have died.\n\nWhat to do: check that the services are running (sudo systemctl --failed), then see Monitoring → Resources for what uses the memory; lower pm.max_children or the database's buffers, or add RAM.", in.OOMKills)})
	}
	if in.C5xx >= 50 && in.Requests > 0 && in.C5xx >= 0.1*in.Requests {
		out = append(out, alert{Kind: "5xx", Headline: "a burst of server errors",
			Body: fmt.Sprintf("%.0f of the last 5 minutes' %.0f requests (%.0f%%) failed with a 5xx error.\n\nWhat to do: Monitoring → Requests shows which routes; Monitoring → Apps shows the app's errors; 502/504 usually means PHP-FPM is down or out of workers.", in.C5xx, in.Requests, 100*in.C5xx/in.Requests)})
	}
	if in.WAL >= 1<<30 && in.WALDayAgo > 0 && in.WAL >= 3*in.WALDayAgo {
		out = append(out, alert{Kind: "wal", Headline: "PostgreSQL's WAL (pg_wal) is piling up",
			Body: fmt.Sprintf("pg_wal holds %s, up from %s a day ago. WAL piles up when archiving fails or a replication slot isn't used; if it fills the disk, PostgreSQL stops.\n\nWhat to do: check the backups' status page (archiving errors), and SELECT slot_name, active FROM pg_replication_slots for an unused slot.", notify.Bytes(int64(in.WAL)), notify.Bytes(int64(in.WALDayAgo)))})
	}
	return out
}

// Alerts checks every minute, while monitoring is on, for what can't wait for the daily
// summary, and emails it straight away: each kind at most once every 6 hours. It returns
// when the service stops.
func (s *Service) Alerts(m *monitor.Client) {
	t := time.NewTicker(alertEvery)
	defer t.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		r := s.Runner()
		if r == nil || !s.Config().Monitor.Enabled || !s.SetupComplete() {
			continue
		}
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		in, err := readAlertInput(ctx, m)
		if err == nil {
			for _, a := range judgeAlerts(in) {
				key := "alert:" + a.Kind
				if last, _ := s.State.GetMeta(key); last != "" {
					if at, err := time.Parse(time.RFC3339, last); err == nil && time.Since(at) < alertQuiet {
						continue
					}
				}
				subject := "🔥 " + r.Server + " — " + a.Headline
				if err := r.Mail.Send(ctx, subject, a.Body+"\n\nThis alert won't repeat for 6 hours.\n"); err != nil {
					s.logf("alert %s: send: %v", a.Kind, err)
					continue
				}
				s.logf("alert sent: %s", a.Headline)
				s.State.SetMeta(key, time.Now().UTC().Format(time.RFC3339))
			}
		}
		cancel()
	}
}

// readAlertInput reads the numbers the alerts need from the monitor.
func readAlertInput(ctx context.Context, m *monitor.Client) (alertInput, error) {
	var in alertInput
	latest, err := m.Latest(ctx)
	if err != nil {
		return in, err
	}
	v := latest.Values
	var used []string
	for n := range v {
		if strings.HasPrefix(n, "disk.used:") {
			used = append(used, n)
		}
	}
	day, _ := m.Series(ctx, "24h", append(used, "disk.use:pg_wal")...)
	for _, n := range used {
		mount := strings.TrimPrefix(n, "disk.used:")
		d := alertDisk{Mount: mount, Pct: v["disk.pct:"+mount], Free: v["disk.size:"+mount] - v[n]}
		if g, ok := growthPerDay(day.X, day.Series[n].Avg); ok {
			d.PerDay = g
		}
		in.Disks = append(in.Disks, d)
	}
	in.WAL = v["disk.use:pg_wal"]
	for _, x := range day.Series["disk.use:pg_wal"].Avg {
		if x != nil {
			in.WALDayAgo = *x
			break
		}
	}
	hour, err := m.Series(ctx, "1h", "oom.kills", "req.c5xx", "req.per_second")
	if err == nil {
		// The last 5 minutes: 20 readings of 15 seconds.
		last := func(name string) (sum, avg float64) {
			vals := hour.Series[name].Avg
			n := 0.0
			for _, x := range vals[max(0, len(vals)-20):] {
				if x != nil {
					sum += *x
					n++
				}
			}
			if n > 0 {
				avg = sum / n
			}
			return sum, avg
		}
		in.OOMKills, _ = last("oom.kills")
		_, perMinute := last("req.c5xx") // held per minute: a minute's count in each reading
		in.C5xx = perMinute * 5
		_, perSecond := last("req.per_second")
		in.Requests = perSecond * 300
	}
	return in, nil
}

// growthPerDay is how fast a series grew from its first to its last value, per day;
// false with less than 6 hours between them.
func growthPerDay(x []int64, values []*float64) (float64, bool) {
	first, last := -1, -1
	for i, v := range values {
		if v != nil {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 || x[last]-x[first] < 6*3600 {
		return 0, false
	}
	return (*values[last] - *values[first]) / float64(x[last]-x[first]) * 86400, true
}
