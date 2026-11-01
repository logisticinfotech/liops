// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/laravel"
)

// logFilePatterns are the log files the viewer offers, besides the apps' and the
// services' journals.
var logFilePatterns = []string{
	"/var/log/syslog", "/var/log/kern.log", "/var/log/auth.log",
	"/var/log/nginx/*.log", "/var/log/apache2/*.log", "/var/log/caddy/*.log", "/var/log/php*-fpm*.log", "/var/log/php*.slow.log",
	"/var/log/mysql/error.log", "/var/log/postgresql/*.log", "/var/log/redis/*.log",
}

// maxLogTail bounds how much of a file the viewer reads: its last 2 MB.
const maxLogTail = 2 << 20

var errorLineRE = regexp.MustCompile(`(?i)\b(error|err|crit|critical|fatal|alert|emerg|emergency|panic|exception)\b`)

type logsData struct {
	Sources              []logSource
	Source               string
	Filter               string
	Errors               bool
	Lines                int
	Follow               bool
	Output               []string
	Problem              string
	Growth               []logGrowthRow
	FollowLink, StopLink string
}

type logSource struct {
	ID, Label, Group string
}

type logGrowthRow struct {
	Path          string
	Size, PerHour float64
}

// logSources lists what the viewer can show: nothing outside this list is ever read.
func (s *Server) logSources(ctx context.Context) []logSource {
	var out []logSource
	if latest, err := s.Monitor.Latest(ctx); err == nil {
		var units []string
		for k := range latest.Values {
			if u, ok := strings.CutPrefix(k, "svc.mem:"); ok {
				units = append(units, u)
			}
		}
		sort.Strings(units)
		for _, u := range units {
			out = append(out, logSource{"unit:" + u, strings.TrimSuffix(u, ".service"), "Services (journal)"})
		}
	}
	var files []string
	for _, p := range logFilePatterns {
		m, _ := filepath.Glob(p)
		files = append(files, m...)
	}
	for _, f := range files {
		out = append(out, logSource{"file:" + f, f, "Log files"})
	}
	for _, a := range laravel.FindApps(s.NginxDir, s.Caddyfile) {
		m, _ := filepath.Glob(filepath.Join(a.Dir, "storage", "logs", "*.log"))
		sort.Sort(sort.Reverse(sort.StringSlice(m)))
		for _, f := range m {
			out = append(out, logSource{"file:" + f, f, "Apps"})
		}
	}
	return out
}

// logs is the log viewer: the last lines of a service's journal or a log file, filtered,
// read live (never stored), and which logs grow fastest.
func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	d := logsData{Source: q.Get("source"), Filter: strings.TrimSpace(q.Get("q")), Errors: q.Get("errors") == "1", Follow: q.Get("follow") == "1"}
	d.Lines, _ = strconv.Atoi(q.Get("lines"))
	if d.Lines <= 0 || d.Lines > 1000 {
		d.Lines = 200
	}
	ctx := r.Context()
	d.Sources = s.logSources(ctx)
	if g, err := s.Monitor.LogGrowth(ctx); err == nil {
		for _, x := range g {
			d.Growth = append(d.Growth, logGrowthRow{x.Path, x.Size, x.PerHour})
		}
	}
	for _, x := range s.Svc.AppLogGrowth() {
		d.Growth = append(d.Growth, logGrowthRow{Path: x.Path, PerHour: x.PerHour})
	}
	sort.Slice(d.Growth, func(i, j int) bool { return d.Growth[i].PerHour > d.Growth[j].PerHour })
	d.Growth = d.Growth[:min(10, len(d.Growth))]

	p := page{Title: "Logs", Nav: "logs", Data: &d}
	if d.Source != "" {
		known := false
		for _, src := range d.Sources {
			known = known || src.ID == d.Source
		}
		if !known {
			d.Problem = "That log isn't one liops shows."
		} else {
			var err error
			d.Output, err = s.readLog(ctx, d.Source, d.Lines, d.Filter, d.Errors)
			if err != nil {
				d.Problem = "Couldn't read it: " + err.Error()
			}
		}
		link := func(follow string) string {
			v := r.URL.Query()
			v.Set("follow", follow)
			return "/monitoring/logs?" + v.Encode()
		}
		d.FollowLink, d.StopLink = link("1"), link("0")
		if d.Follow {
			p.Refresh = 5
		}
	}
	s.render(w, r, "logs", p)
}

// readLog returns a source's last lines that match the filter.
func (s *Server) readLog(ctx context.Context, source string, n int, filter string, errorsOnly bool) ([]string, error) {
	var lines []string
	if unit, ok := strings.CutPrefix(source, "unit:"); ok {
		args := []string{"--unit=" + unit, "--no-pager", "--output=short-iso", "--lines=" + strconv.Itoa(max(n, 2000))}
		if errorsOnly {
			args = append(args, "--priority=warning")
		}
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, err := s.Cmd.Run(ctx, command.Cmd{Name: "journalctl", Args: args})
		if err != nil {
			return nil, err
		}
		lines = strings.Split(strings.TrimRight(string(out), "\n"), "\n")
		errorsOnly = false // the journal's priority did it
	} else {
		path := strings.TrimPrefix(source, "file:")
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		if fi, err := f.Stat(); err == nil && fi.Size() > maxLogTail {
			f.Seek(-maxLogTail, io.SeekEnd)
		}
		data, err := io.ReadAll(io.LimitReader(f, maxLogTail))
		if err != nil {
			return nil, err
		}
		lines = strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	}
	var out []string
	low := strings.ToLower(filter)
	for _, l := range lines {
		if (filter == "" || strings.Contains(strings.ToLower(l), low)) && (!errorsOnly || errorLineRE.MatchString(l)) {
			if len(l) > 2000 {
				l = l[:1999] + "…"
			}
			out = append(out, l)
		}
	}
	return out[max(0, len(out)-n):], nil
}
