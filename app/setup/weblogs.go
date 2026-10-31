// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/monitor"
)

// Where Ubuntu's nginx and Caddy keep their configs.
const (
	NginxDir  = "/etc/nginx"
	Caddyfile = monitor.DefaultCaddyfile
)

// nginxConfName is the file liops adds to nginx's conf.d (included inside http {}).
const nginxConfName = "liops-monitor.conf"

// webLogMark ends every line liops adds to a site, so it can be found again.
const webLogMark = "# liops monitoring"

// WebLogPlan is what liops would change so the monitor can read a web server's requests,
// for a person to approve.
type WebLogPlan struct {
	Server  string       // "nginx" or "Caddy"
	Files   []WebLogFile // files to write
	Skipped []string     // sites left alone, and why
	Done    bool         // nothing to change
}

// WebLogFile is one file's change.
type WebLogFile struct {
	Path   string
	Sites  []string // the server names it touches; empty for a new file
	Adds   []string // the lines it adds
	before []byte
	after  []byte
}

// nginxConf is liops's conf.d file: the format, and an access log for the sites that
// don't name their own.
func nginxConf() string {
	return "# Written by liops: the access log monitoring reads (each request's host, route, status and times).\n" +
		monitor.NginxLogFormat + "\naccess_log " + monitor.NginxLogPath + " liops;\n"
}

// PlanNginxLogs works out the nginx changes: liops's conf.d file, and in each site with
// its own access_log (which turns off the one from conf.d) one more access_log line.
func PlanNginxLogs(dir string) (WebLogPlan, error) {
	plan := WebLogPlan{Server: "nginx"}
	confPath := filepath.Join(dir, "conf.d", nginxConfName)
	if old, _ := os.ReadFile(confPath); string(old) != nginxConf() {
		plan.Files = append(plan.Files, WebLogFile{Path: confPath, Adds: strings.Split(strings.TrimSpace(nginxConf()), "\n"), before: old, after: []byte(nginxConf())})
	}
	var files []string
	for _, pattern := range []string{"sites-enabled/*", "conf.d/*.conf"} {
		m, _ := filepath.Glob(filepath.Join(dir, pattern))
		files = append(files, m...)
	}
	sort.Strings(files)
	for _, path := range files {
		if filepath.Base(path) == nginxConfName {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return plan, err
		}
		after, sites, adds, skipped := planNginxFile(data)
		plan.Skipped = append(plan.Skipped, skipped...)
		if len(adds) > 0 {
			plan.Files = append(plan.Files, WebLogFile{Path: path, Sites: sites, Adds: adds, before: data, after: after})
		}
	}
	plan.Done = len(plan.Files) == 0
	return plan, nil
}

// nginxStmt is one directive: its words and where it ends (the ;) or opens its block.
type nginxStmt struct {
	words      []string
	start, end int // byte offsets: the first word, and just past ; or {
	depth      int
	block      bool
}

// nginxStatements splits a config into directives, skipping comments and honouring quotes.
func nginxStatements(data []byte) []nginxStmt {
	var out []nginxStmt
	var cur nginxStmt
	depth := 0
	word := []byte{}
	inWord := false
	flushWord := func() {
		if inWord {
			cur.words = append(cur.words, string(word))
			word, inWord = word[:0], false
		}
	}
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '#' && !inWord:
			for i < len(data) && data[i] != '\n' {
				i++
			}
		case c == '"' || c == '\'':
			if len(cur.words) == 0 && !inWord {
				cur.start = i
			}
			q := c
			inWord = true
			for i++; i < len(data) && data[i] != q; i++ {
				if data[i] == '\\' && i+1 < len(data) {
					i++
				}
				word = append(word, data[i])
			}
		case c == ';' || c == '{':
			flushWord()
			cur.end, cur.depth, cur.block = i+1, depth, c == '{'
			if len(cur.words) > 0 {
				out = append(out, cur)
			}
			cur = nginxStmt{}
			if c == '{' {
				depth++
			}
		case c == '}':
			flushWord()
			cur = nginxStmt{}
			depth--
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			flushWord()
		default:
			if len(cur.words) == 0 && !inWord {
				cur.start = i
			}
			word = append(word, c)
			inWord = true
		}
	}
	return out
}

// planNginxFile adds liops's access_log after the last access_log of each server block
// that has its own.
func planNginxFile(data []byte) (after []byte, sites, adds, skipped []string) {
	stmts := nginxStatements(data)
	type insert struct {
		at   int
		text string
	}
	var inserts []insert
	for i, s := range stmts {
		if !s.block || s.words[0] != "server" {
			continue
		}
		var names []string
		var last *nginxStmt
		off, ours := false, false
		for j := i + 1; j < len(stmts) && stmts[j].depth > s.depth; j++ {
			t := stmts[j]
			if t.depth != s.depth+1 {
				continue
			}
			switch t.words[0] {
			case "server_name":
				names = append(names, t.words[1:]...)
			case "access_log":
				switch {
				case len(t.words) > 1 && t.words[1] == "off":
					off = true
				case len(t.words) > 1 && t.words[1] == monitor.NginxLogPath:
					ours = true
				default:
					last = &stmts[j]
				}
			}
		}
		name := strings.Join(names, " ")
		if name == "" {
			name = "(no server_name)"
		}
		switch {
		case ours:
		case off && last == nil:
			skipped = append(skipped, name+": access_log is off, so its requests aren't measured")
		case last != nil:
			lineStart := strings.LastIndexByte(string(data[:last.start]), '\n') + 1
			indent := string(data[lineStart:last.start])
			if strings.TrimSpace(indent) != "" {
				indent = "    "
			}
			text := "access_log " + monitor.NginxLogPath + " liops; " + webLogMark
			// On a line of its own after the access_log's line, keeping any comment there.
			if nl := strings.IndexByte(string(data[last.end:]), '\n'); nl >= 0 {
				inserts = append(inserts, insert{last.end + nl + 1, indent + text + "\n"})
			} else {
				inserts = append(inserts, insert{len(data), "\n" + indent + text + "\n"})
			}
			sites = append(sites, name)
			adds = append(adds, text)
		}
	}
	if len(inserts) == 0 {
		return data, nil, nil, skipped
	}
	var b strings.Builder
	prev := 0
	for _, in := range inserts {
		b.Write(data[prev:in.at])
		b.WriteString(in.text)
		prev = in.at
	}
	b.Write(data[prev:])
	return []byte(b.String()), sites, adds, skipped
}

// PlanCaddyLogs works out the Caddy changes: a log line in each site block without one.
// Sites that already log are read as they are (their file, or the journal).
func PlanCaddyLogs(path string) (WebLogPlan, error) {
	plan := WebLogPlan{Server: "Caddy"}
	data, err := os.ReadFile(path)
	if err != nil {
		return plan, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	depth := 0
	var siteLine = -1
	var label string
	hasLog := false
	var out strings.Builder
	var sites, adds []string
	type site struct {
		line  int
		label string
	}
	var need []site
	for i, raw := range lines {
		line := raw
		if j := strings.Index(line, "#"); j >= 0 && (j == 0 || line[j-1] == ' ' || line[j-1] == '\t') {
			line = line[:j]
		}
		trim := strings.TrimSpace(line)
		if depth == 0 && strings.HasPrefix(trim, "import ") {
			plan.Skipped = append(plan.Skipped, trim+": imported sites aren't changed; add log to them by hand")
		}
		if depth == 1 && trim != "" && siteLine >= 0 {
			if f := strings.Fields(trim); f[0] == "log" {
				hasLog = true
			}
		}
		for _, c := range trim {
			switch c {
			case '{':
				if depth == 0 {
					label = strings.TrimSpace(strings.TrimSuffix(trim, "{"))
					siteLine, hasLog = i, false
					// The global options block ({ alone) and snippets ((name)) aren't sites.
					if label == "" || strings.HasPrefix(label, "(") {
						siteLine = -1
					}
				}
				depth++
			case '}':
				depth--
				if depth == 0 && siteLine >= 0 {
					if !hasLog {
						need = append(need, site{siteLine, label})
					}
					siteLine = -1
				}
			}
		}
	}
	add := map[int]bool{}
	for _, s := range need {
		add[s.line] = true
		sites = append(sites, s.label)
		adds = append(adds, "log "+webLogMark)
	}
	for i, l := range lines {
		out.WriteString(l)
		if add[i] {
			out.WriteString("\tlog " + webLogMark + "\n")
		}
	}
	if len(need) > 0 {
		plan.Files = append(plan.Files, WebLogFile{Path: path, Sites: sites, Adds: adds, before: data, after: []byte(out.String())})
	}
	plan.Done = len(plan.Files) == 0
	return plan, nil
}

// ApplyWebLogs writes a plan's files, keeping copies in backupDir, checks the web
// server's config (nginx -t, caddy validate), and reloads it. When the check fails, every
// file is put back and nothing is reloaded.
func ApplyWebLogs(ctx context.Context, run command.Runner, plan WebLogPlan, backupDir string) error {
	if plan.Done {
		return nil
	}
	stamp := filepath.Join(backupDir, strings.ToLower(plan.Server)+"-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(stamp, 0o700); err != nil {
		return err
	}
	// sites-enabled/ holds symlinks into sites-available/: write the file they point to,
	// not over the link.
	real := func(path string) string {
		if r, err := filepath.EvalSymlinks(path); err == nil {
			return r
		}
		return path
	}
	restore := func() {
		for _, f := range plan.Files {
			if f.before == nil {
				os.Remove(f.Path)
			} else {
				writeAtomic(real(f.Path), string(f.before), fileMode(f.Path))
			}
		}
	}
	for i, f := range plan.Files {
		if f.before != nil {
			if err := os.WriteFile(filepath.Join(stamp, fmt.Sprintf("%d-%s", i, filepath.Base(f.Path))), f.before, 0o600); err != nil {
				return err
			}
		}
		if err := writeAtomic(real(f.Path), string(f.after), fileMode(f.Path)); err != nil {
			restore()
			return err
		}
	}
	check := command.Cmd{Name: "nginx", Args: []string{"-t"}}
	unit := "nginx"
	if plan.Server == "Caddy" {
		check = command.Cmd{Name: "caddy", Args: []string{"validate", "--config", plan.Files[0].Path, "--adapter", "caddyfile"}}
		unit = "caddy"
	}
	if out, err := run.Run(ctx, check); err != nil {
		restore()
		return fmt.Errorf("the %s config check failed, so liops put the files back and reloaded nothing: %w\n%s", plan.Server, err, out)
	}
	if out, err := run.Run(ctx, command.Cmd{Name: "systemctl", Args: []string{"reload", unit}}); err != nil {
		return fmt.Errorf("reload %s: %w\n%s", unit, err, out)
	}
	return nil
}

// fileMode keeps an existing file's permissions (0644 for a new one).
func fileMode(path string) os.FileMode {
	if fi, err := os.Stat(path); err == nil {
		return fi.Mode().Perm()
	}
	return 0o644
}
