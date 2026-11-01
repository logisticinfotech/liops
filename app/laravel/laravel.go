// SPDX-License-Identifier: Apache-2.0

// Package laravel finds the Laravel apps behind the web servers and checks the few
// settings that make them slow or unsafe in production. It reads only named .env keys,
// in memory, and returns findings: no .env value is stored, shown or sent.
package laravel

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// App is one Laravel app: the folder above a site's public/.
type App struct {
	Dir   string
	Sites []string // the server names that serve it
}

var (
	nginxRootRE = regexp.MustCompile(`(?m)^\s*root\s+("?)([^\s;"]+)("?)\s*;`)
	nginxNameRE = regexp.MustCompile(`(?m)^\s*server_name\s+([^;]+);`)
	caddyRootRE = regexp.MustCompile(`(?m)^\s*root\s+(?:\*\s+)?(\S+)`)
	caddySiteRE = regexp.MustCompile(`(?m)^([^\s#{(][^{]*?)\s*\{\s*$`)
)

// ApacheDir is where Apache's config is (tests point it elsewhere).
var ApacheDir = "/etc/apache2"

// FindApps lists the Laravel apps the web servers serve: each site root ending in
// /public whose parent holds artisan.
func FindApps(nginxDir, caddyfile string) []App {
	byDir := map[string]*App{}
	add := func(root string, sites []string) {
		dir := strings.TrimSuffix(filepath.Clean(root), "/public")
		if dir == filepath.Clean(root) {
			return
		}
		if _, err := os.Stat(filepath.Join(dir, "artisan")); err != nil {
			return
		}
		a := byDir[dir]
		if a == nil {
			a = &App{Dir: dir}
			byDir[dir] = a
		}
		a.Sites = append(a.Sites, sites...)
	}
	var files []string
	for _, p := range []string{"sites-enabled/*", "conf.d/*.conf"} {
		m, _ := filepath.Glob(filepath.Join(nginxDir, p))
		files = append(files, m...)
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, block := range serverBlocks(string(data)) {
			var names []string
			for _, m := range nginxNameRE.FindAllStringSubmatch(block, -1) {
				names = append(names, strings.Fields(m[1])...)
			}
			for _, m := range nginxRootRE.FindAllStringSubmatch(block, -1) {
				add(m[2], names)
			}
		}
	}
	// Apache: each <VirtualHost>'s DocumentRoot, with its ServerName and ServerAliases.
	apacheFiles, _ := filepath.Glob(filepath.Join(ApacheDir, "sites-enabled", "*.conf"))
	for _, f := range apacheFiles {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var names []string
		var root string
		for _, line := range strings.Split(string(data), "\n") {
			t := strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(strings.ToLower(t), "<virtualhost"):
				names, root = nil, ""
			case strings.HasPrefix(strings.ToLower(t), "</virtualhost"):
				if root != "" {
					add(root, names)
				}
			default:
				if f := strings.Fields(t); len(f) >= 2 {
					switch strings.ToLower(f[0]) {
					case "servername", "serveralias":
						names = append(names, f[1:]...)
					case "documentroot":
						root = strings.Trim(f[1], `"`)
					}
				}
			}
		}
	}
	if data, err := os.ReadFile(caddyfile); err == nil {
		site := ""
		for _, line := range strings.Split(string(data), "\n") {
			if m := caddySiteRE.FindStringSubmatch(line); m != nil {
				site = strings.TrimSpace(m[1])
			}
			if m := caddyRootRE.FindStringSubmatch(line); m != nil {
				add(m[1], []string{site})
			}
		}
	}
	out := make([]App, 0, len(byDir))
	for _, a := range byDir {
		sort.Strings(a.Sites)
		a.Sites = compact(a.Sites)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

var serverRE = regexp.MustCompile(`(?m)^\s*server\s*\{`)

// serverBlocks splits an nginx file into its server { … } blocks (comments dropped).
func serverBlocks(data string) []string {
	var clean strings.Builder
	for _, line := range strings.Split(data, "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		clean.WriteString(line + "\n")
	}
	text := clean.String()
	var out []string
	for _, loc := range serverRE.FindAllStringIndex(text, -1) {
		depth := 0
		for i := loc[1] - 1; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
			}
			if depth == 0 {
				out = append(out, text[loc[0]:i+1])
				break
			}
		}
	}
	return out
}

func compact(s []string) []string {
	var out []string
	for i, v := range s {
		if v != "" && (i == 0 || v != s[i-1]) {
			out = append(out, v)
		}
	}
	return out
}

// envKeys are the only .env keys read: settings, never secrets.
var envKeys = map[string]bool{
	"APP_ENV": true, "APP_DEBUG": true, "LOG_LEVEL": true, "QUEUE_CONNECTION": true, "SESSION_DRIVER": true,
	"SESSION_CONNECTION": true, "CACHE_STORE": true, "CACHE_DRIVER": true, "REDIS_DB": true, "REDIS_CACHE_DB": true,
	"REDIS_CACHE_CONNECTION": true, "TELESCOPE_ENABLED": true, "DEBUGBAR_ENABLED": true, "DB_DATABASE": true,
	"APP_NAME": true, "REDIS_PREFIX": true, "REDIS_QUEUE": true,
}

// readEnv reads the named keys of an app's .env.
func readEnv(dir string) map[string]string {
	env := map[string]string{}
	f, err := os.Open(filepath.Join(dir, ".env"))
	if err != nil {
		return env
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		if !ok || !envKeys[k] {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		env[k] = v
	}
	return env
}

func isTrue(v string) bool {
	v = strings.ToLower(v)
	return v == "true" || v == "1" || v == "(true)" || v == "on" || v == "yes"
}

// Finding is one thing found about an app.
type Finding struct {
	Bad  bool   // slow or unsafe; otherwise worth knowing
	Text string // what it is and what to do
}

// Check is an app's findings, and what its sessions and cache use.
type Check struct {
	Findings []Finding
	// Sessions in Redis: true when sessions are stored in Redis, and whether they share
	// a Redis database with the cache (clearing the cache would end every session).
	SessionsInRedis bool
	SessionsShareDB bool
	SessionDB       string // the Redis database number sessions use
	Database        string // DB_DATABASE, to find its jobs tables
	QueueDB         bool   // the database queue driver
	QueueKey        string // the Redis queue's key, with the redis queue driver
}

// CheckApp checks one app's production settings.
func CheckApp(a App) Check {
	env := readEnv(a.Dir)
	var c Check
	bad := func(t string) { c.Findings = append(c.Findings, Finding{true, t}) }
	note := func(t string) { c.Findings = append(c.Findings, Finding{false, t}) }
	production := env["APP_ENV"] == "" || env["APP_ENV"] == "production"
	exists := func(rel string) bool { _, err := os.Stat(filepath.Join(a.Dir, rel)); return err == nil }

	if production && isTrue(env["APP_DEBUG"]) {
		bad("APP_DEBUG is on in production: pages are slower, and errors show stack traces (with settings) to visitors. Set APP_DEBUG=false.")
	}
	if !exists("bootstrap/cache/config.php") {
		bad("The config isn't cached: Laravel reads every config file on every request. Run php artisan config:cache in each deploy (php artisan optimize does all of them).")
	}
	if !exists("bootstrap/cache/routes-v7.php") {
		note("The routes aren't cached: run php artisan route:cache in each deploy.")
	}
	if !exists("bootstrap/cache/events.php") {
		note("Events aren't cached: run php artisan event:cache in each deploy.")
	}
	if production && strings.EqualFold(env["LOG_LEVEL"], "debug") {
		bad("LOG_LEVEL is debug in production: logs grow fast and every line costs disk writes. Use warning or error.")
	}
	queue := strings.ToLower(env["QUEUE_CONNECTION"])
	switch queue {
	case "sync":
		bad("QUEUE_CONNECTION is sync: queued jobs (mail, exports, notifications) run inside the web request, which waits for them. Use redis or database, and run php artisan queue:work.")
	case "database":
		c.QueueDB = true
	case "redis":
		// config/database.php: prefix REDIS_PREFIX, or APP_NAME's slug + "_database_";
		// config/queue.php: REDIS_QUEUE, or "default".
		prefix, ok := env["REDIS_PREFIX"]
		if !ok {
			prefix = slug(or(env["APP_NAME"], "laravel")) + "_database_"
		}
		c.QueueKey = prefix + "queues:" + or(env["REDIS_QUEUE"], "default")
	}
	if production && exists("vendor/laravel/telescope") && !strings.EqualFold(env["TELESCOPE_ENABLED"], "false") {
		bad("Telescope is installed and on in production: it records every request and query into the database. Set TELESCOPE_ENABLED=false, or install it with --dev.")
	}
	if exists("vendor/barryvdh/laravel-debugbar") && (isTrue(env["DEBUGBAR_ENABLED"]) || (production && isTrue(env["APP_DEBUG"]))) {
		bad("Debugbar is on: it collects every query of every request and shows them in the page. Install it with --dev, or set DEBUGBAR_ENABLED=false.")
	}
	session := strings.ToLower(env["SESSION_DRIVER"])
	cache := strings.ToLower(env["CACHE_STORE"])
	if cache == "" {
		cache = strings.ToLower(env["CACHE_DRIVER"])
	}
	for what, driver := range map[string]string{"Sessions": session, "The cache": cache} {
		if driver == "file" || driver == "database" {
			note(what + " use the " + driver + " driver: disk or database work on every request. Redis is the usual choice once an app is busy.")
		}
	}
	c.Database = env["DB_DATABASE"]
	if session == "redis" {
		c.SessionsInRedis = true
		db := func(conn string) string {
			if conn == "cache" {
				return or(env["REDIS_CACHE_DB"], "1")
			}
			return or(env["REDIS_DB"], "0")
		}
		c.SessionDB = db(or(env["SESSION_CONNECTION"], "default"))
		if cache == "redis" {
			c.SessionsShareDB = db(or(env["REDIS_CACHE_CONNECTION"], "cache")) == c.SessionDB
		}
	}
	return c
}

func or(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// ErrorGroup is one kind of error in an app's log: its exception class and where it
// was thrown, or the message's shape.
type ErrorGroup struct {
	Class       string
	Where       string
	Count       int
	First, Last time.Time
	Message     string // the latest one's first line: shown live, never stored
}

var (
	logLineRE   = regexp.MustCompile(`^\[(\d{4}-\d\d-\d\d[ T]\d\d:\d\d:\d\d)[^\]]*\] [\w-]+\.(ERROR|CRITICAL|ALERT|EMERGENCY): (.*)`)
	exceptionRE = regexp.MustCompile(`\(([A-Za-z_\\]+)(?:\(code: [^)]*\))?: .* at (/\S+:\d+)\)`)
	numbersRE   = regexp.MustCompile(`\d+`)
)

// maxLogRead bounds how much of an app's logs Errors reads.
const maxLogRead = 8 << 20

// Errors groups the errors in an app's newest logs (storage/logs/*.log, at most 8 MB
// from their ends), most frequent first.
func Errors(a App, since time.Time) []ErrorGroup {
	files, _ := filepath.Glob(filepath.Join(a.Dir, "storage", "logs", "*.log"))
	sort.Slice(files, func(i, j int) bool { return modTime(files[i]).After(modTime(files[j])) })
	groups := map[string]*ErrorGroup{}
	budget := int64(maxLogRead)
	for _, f := range files {
		if budget <= 0 || modTime(f).Before(since) {
			break
		}
		budget -= scanLog(f, budget, since, groups)
	}
	out := make([]ErrorGroup, 0, len(groups))
	for _, g := range groups {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out
}

func modTime(path string) time.Time {
	if fi, err := os.Stat(path); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

func scanLog(path string, budget int64, since time.Time, groups map[string]*ErrorGroup) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	size := int64(0)
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	start := max(0, size-budget)
	f.Seek(start, io.SeekStart)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		m := logLineRE.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		at, err := time.ParseInLocation("2006-01-02 15:04:05", strings.Replace(m[1], "T", " ", 1), time.Local)
		if err != nil || at.Before(since) {
			continue
		}
		msg := m[3]
		g := ErrorGroup{}
		if e := exceptionRE.FindStringSubmatch(msg); e != nil {
			g.Class, g.Where = e[1], e[2]
			if i := strings.LastIndex(g.Where, "/vendor/"); i >= 0 {
				g.Where = "vendor/" + g.Where[i+len("/vendor/"):]
			} else if i := strings.Index(g.Where, "/app/"); i >= 0 {
				g.Where = g.Where[i+1:]
			}
		} else {
			shape := msg
			if i := strings.Index(shape, " {"); i >= 0 {
				shape = shape[:i]
			}
			g.Where = numbersRE.ReplaceAllString(trim(shape, 100), "N")
		}
		k := g.Class + "|" + g.Where
		cur := groups[k]
		if cur == nil {
			g.First = at
			cur = &g
			groups[k] = cur
		}
		cur.Count++
		cur.Last = at
		if at.Before(cur.First) {
			cur.First = at
		}
		cur.Message = trim(msg, 200)
	}
	return size - start
}

func trim(s string, n int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > n {
		return s[:n-1] + "…"
	}
	return s
}

// Release identifies what's deployed: the release a current symlink points to, or the
// git commit; empty when neither is there.
func Release(a App) string {
	// Deploy tools point a current symlink at the release they've just put in place.
	if fi, err := os.Lstat(a.Dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, err := os.Readlink(a.Dir); err == nil {
			return filepath.Base(target)
		}
	}
	head, err := os.ReadFile(filepath.Join(a.Dir, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	ref, isRef := strings.CutPrefix(strings.TrimSpace(string(head)), "ref: ")
	if !isRef {
		return short(ref)
	}
	if data, err := os.ReadFile(filepath.Join(a.Dir, ".git", ref)); err == nil {
		return short(strings.TrimSpace(string(data)))
	}
	if packed, err := os.ReadFile(filepath.Join(a.Dir, ".git", "packed-refs")); err == nil {
		for _, line := range strings.Split(string(packed), "\n") {
			if id, name, ok := strings.Cut(line, " "); ok && name == ref {
				return short(id)
			}
		}
	}
	return ""
}

func short(id string) string { return id[:min(12, len(id))] }

var slugRE = regexp.MustCompile(`[^a-z0-9]+`)

// slug is Laravel's Str::slug($v, '_') for plain names.
func slug(v string) string {
	return strings.Trim(slugRE.ReplaceAllString(strings.ToLower(v), "_"), "_")
}
