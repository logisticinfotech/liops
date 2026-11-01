// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/php"
	"github.com/logisticinfotech/liops/app/tuning"
)

// The files liops manages for tuning: never the main configs.
const (
	MySQLTuningConf    = "/etc/mysql/mysql.conf.d/zz-liops-tuning.cnf"
	postgresTuningConf = "30-liops-tuning.conf"
	phpTuningIni       = "zz-liops-tuning.ini"
)

// TuningEnv is where and how tuning changes are applied.
type TuningEnv struct {
	Run           command.Runner
	MySQL         *mysql.Instance   // nil: this server's liops doesn't manage MySQL
	MySQLPassword string            // root's, when its socket login needs one; used and forgotten
	MySQLConf     string            // MySQLTuningConf
	Postgres      *postgres.Cluster // nil: this server's liops doesn't manage PostgreSQL
	PHPEtc        string            // php.DefaultEtc
	RedisAddr     string
	RedisPassword string
	BackupDir     string
}

// ApplyTuning sets the values of one kind of setting (for a PHP version or pool, target),
// and returns what they were, for a revert. It writes liops's own file (or, for a pool,
// the pool's file, keeping a copy), checks the config, and applies it the least disruptive
// way: MySQL online, PostgreSQL and PHP-FPM by a reload, Redis with CONFIG SET. A check
// that fails puts everything back. needsRestart: PostgreSQL waits for a restart, which a
// person starts.
func ApplyTuning(ctx context.Context, env TuningEnv, kind, target string, values map[string]string) (old map[string]string, needsRestart bool, err error) {
	for k, v := range values {
		if !tuning.Allowed[kind][k] {
			return nil, false, fmt.Errorf("liops doesn't change %s", k)
		}
		// "" removes a PHP or pool setting again (a revert of one that wasn't set).
		if !safeValue.MatchString(v) && !(v == "" && (kind == tuning.KindPHP || kind == tuning.KindPool)) {
			return nil, false, fmt.Errorf("%s: %q isn't a value liops writes", k, v)
		}
	}
	switch kind {
	case tuning.KindMySQL:
		return applyMySQLTuning(ctx, env, values)
	case tuning.KindPostgres:
		return applyPostgresTuning(ctx, env, values)
	case tuning.KindPHP:
		old, err := applyPHPIni(ctx, env, target, values)
		return old, false, err
	case tuning.KindPool:
		old, err := applyPoolTuning(ctx, env, target, values)
		return old, false, err
	case tuning.KindRedis:
		old, err := applyRedisTuning(ctx, env, values)
		return old, false, err
	}
	return nil, false, fmt.Errorf("unknown kind of setting %q", kind)
}

// safeValue is what a tuned value may look like: a number with an optional unit.
var safeValue = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?([kKmMgG][bB]?|ms|s|mb|gb)?$`)

// readKeyValues reads "key = value" lines (comments and sections skipped).
func readKeyValues(path string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "[") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `'"`)
		}
	}
	return out
}

// writeManaged writes liops's tuning file: a header, an optional section, sorted keys.
func writeManaged(path, section, quote string, values map[string]string) error {
	var b strings.Builder
	b.WriteString("# Written by liops: tuning changes approved in Monitoring → Tuning.\n# Revert them there; edits here are overwritten by the next change.\n")
	if section != "" {
		b.WriteString(section + "\n")
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s = %s%s%s\n", k, quote, values[k], quote)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return writeAtomic(path, b.String(), 0o644)
}

// restoreFile puts a file back as it was (removed when it didn't exist).
func restoreFile(path string, before []byte, existed bool) {
	if !existed {
		os.Remove(path)
		return
	}
	writeAtomic(path, string(before), fileMode(path))
}

// bytesOf turns "3840M", "4G", "64MB" into bytes, for SET GLOBAL.
func bytesOf(v string) string {
	u := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(v, "B"), "b"))
	mult := int64(1)
	switch {
	case strings.HasSuffix(u, "K"):
		mult, u = 1<<10, strings.TrimSuffix(u, "K")
	case strings.HasSuffix(u, "M"):
		mult, u = 1<<20, strings.TrimSuffix(u, "M")
	case strings.HasSuffix(u, "G"):
		mult, u = 1<<30, strings.TrimSuffix(u, "G")
	}
	n, err := strconv.ParseInt(u, 10, 64)
	if err != nil {
		return v
	}
	return strconv.FormatInt(n*mult, 10)
}

func applyMySQLTuning(ctx context.Context, env TuningEnv, values map[string]string) (map[string]string, bool, error) {
	if env.MySQL == nil {
		return nil, false, fmt.Errorf("liops tunes MySQL only on a server whose MySQL it backs up")
	}
	root, done, err := rootLogin(ctx, *env.MySQL, env.MySQLPassword)
	if err != nil {
		return nil, false, err
	}
	defer done()
	old := map[string]string{}
	for k := range values {
		v, err := root.Value(ctx, "SELECT @@GLOBAL."+k)
		if err != nil {
			return nil, false, err
		}
		old[k] = v
	}
	before, readErr := os.ReadFile(env.MySQLConf)
	cur := readKeyValues(env.MySQLConf)
	for k, v := range values {
		cur[k] = v
	}
	if err := writeManaged(env.MySQLConf, "[mysqld]", "", cur); err != nil {
		return nil, false, err
	}
	if out, err := env.Run.Run(ctx, command.Cmd{Name: "mysqld", Args: []string{"--validate-config"}}); err != nil {
		restoreFile(env.MySQLConf, before, readErr == nil)
		return nil, false, fmt.Errorf("MySQL's config check failed, so liops put the file back: %w\n%s", err, out)
	}
	for k, v := range values {
		if _, err := root.Query(ctx, "", fmt.Sprintf("SET GLOBAL %s = %s", k, bytesOf(v))); err != nil {
			return old, false, fmt.Errorf("written to %s, but setting it online failed (it applies at the next restart): %w", env.MySQLConf, err)
		}
	}
	return old, false, nil
}

func applyPostgresTuning(ctx context.Context, env TuningEnv, values map[string]string) (map[string]string, bool, error) {
	if env.Postgres == nil {
		return nil, false, fmt.Errorf("liops tunes PostgreSQL only on a server whose PostgreSQL it backs up")
	}
	c := *env.Postgres
	old := map[string]string{}
	for k := range values {
		v, err := c.Setting(ctx, k)
		if err != nil {
			return nil, false, err
		}
		old[k] = v
	}
	path := filepath.Join(filepath.Dir(c.ConfDPath()), postgresTuningConf)
	before, readErr := os.ReadFile(path)
	cur := readKeyValues(path)
	for k, v := range values {
		cur[k] = v
	}
	if err := writeManaged(path, "", "'", cur); err != nil {
		return nil, false, err
	}
	if _, err := c.Query(ctx, "postgres", "SELECT pg_reload_conf()"); err != nil {
		restoreFile(path, before, readErr == nil)
		return nil, false, err
	}
	var rows [][]string
	var err error
	for range 10 {
		time.Sleep(300 * time.Millisecond)
		// "setting could not be applied" is a setting waiting for a restart, not a mistake.
		rows, err = c.Query(ctx, "postgres", "SELECT coalesce(string_agg(name || ': ' || error, '; '), '') FROM pg_file_settings WHERE sourcefile LIKE '%"+postgresTuningConf+"' AND error IS NOT NULL AND error <> 'setting could not be applied'")
		if err == nil && len(rows) == 1 && rows[0][0] == "" {
			break
		}
	}
	if err != nil || (len(rows) == 1 && len(rows[0]) == 1 && rows[0][0] != "") {
		restoreFile(path, before, readErr == nil)
		c.Query(ctx, "postgres", "SELECT pg_reload_conf()")
		problem := ""
		if len(rows) == 1 && len(rows[0]) == 1 {
			problem = rows[0][0]
		}
		return nil, false, fmt.Errorf("PostgreSQL refused the settings, so liops put the file back: %s %v", problem, err)
	}
	restart := false
	for k := range values {
		if r, err := c.Query(ctx, "postgres", "SELECT pending_restart FROM pg_settings WHERE name = '"+k+"'"); err == nil && len(r) == 1 && r[0][0] == "t" {
			restart = true
		}
	}
	if r, err := c.Query(ctx, "postgres", "SELECT count(*) FROM pg_file_settings WHERE sourcefile LIKE '%"+postgresTuningConf+"' AND error = 'setting could not be applied'"); err == nil && len(r) == 1 && r[0][0] != "0" {
		restart = true
	}
	return old, restart, nil
}

func applyPHPIni(ctx context.Context, env TuningEnv, version string, values map[string]string) (map[string]string, error) {
	if !regexp.MustCompile(`^[0-9]+\.[0-9]+$`).MatchString(version) {
		return nil, fmt.Errorf("unknown PHP version %q", version)
	}
	path := filepath.Join(env.PHPEtc, version, "fpm", "conf.d", phpTuningIni)
	before, readErr := os.ReadFile(path)
	cur := readKeyValues(path)
	old := map[string]string{}
	for k, v := range values {
		old[k] = cur[k] // empty: PHP's own default, removed again on revert
		if v == "" {
			delete(cur, k)
		} else {
			cur[k] = v
		}
	}
	if err := writeManaged(path, "", "", cur); err != nil {
		return nil, err
	}
	if err := checkAndReloadPHP(ctx, env.Run, version, func() { restoreFile(path, before, readErr == nil) }); err != nil {
		return nil, err
	}
	return old, nil
}

func checkAndReloadPHP(ctx context.Context, run command.Runner, version string, restore func()) error {
	if out, err := run.Run(ctx, command.Cmd{Name: "php-fpm" + version, Args: []string{"-t"}}); err != nil {
		restore()
		return fmt.Errorf("the PHP %s config check failed, so liops put the file back: %w\n%s", version, err, out)
	}
	if out, err := run.Run(ctx, command.Cmd{Name: "systemctl", Args: []string{"reload", "php" + version + "-fpm"}}); err != nil {
		return fmt.Errorf("reload php%s-fpm: %w\n%s", version, err, out)
	}
	return nil
}

func applyPoolTuning(ctx context.Context, env TuningEnv, label string, values map[string]string) (map[string]string, error) {
	pools, err := php.FindPools(env.PHPEtc)
	if err != nil {
		return nil, err
	}
	var pool *php.Pool
	for i := range pools {
		if pools[i].Label() == label {
			pool = &pools[i]
		}
	}
	if pool == nil {
		return nil, fmt.Errorf("no PHP-FPM pool %s", label)
	}
	before, err := os.ReadFile(pool.File)
	if err != nil {
		return nil, err
	}
	stamp := filepath.Join(env.BackupDir, "php-fpm-tuning-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(stamp, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(stamp, filepath.Base(pool.File)), before, 0o600); err != nil {
		return nil, err
	}
	after, old := setPoolValues(string(before), pool.Name, values)
	for k := range values {
		if _, ok := old[k]; !ok {
			old[k] = "" // wasn't set: a revert removes it
		}
	}
	if err := writeAtomic(pool.File, after, fileMode(pool.File)); err != nil {
		return nil, err
	}
	if err := checkAndReloadPHP(ctx, env.Run, pool.Version, func() { writeAtomic(pool.File, string(before), fileMode(pool.File)) }); err != nil {
		return nil, err
	}
	return old, nil
}

// setPoolValues sets keys in one pool's section: an existing line is changed in place,
// a missing one added at the section's end.
func setPoolValues(data, pool string, values map[string]string) (string, map[string]string) {
	lines := strings.SplitAfter(data, "\n")
	old := map[string]string{}
	done := map[string]bool{}
	section := ""
	var b strings.Builder
	flush := func() {
		if section != pool {
			return
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			if !done[k] {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			if values[k] != "" {
				fmt.Fprintf(&b, "%s = %s ; liops tuning\n", k, values[k])
			}
			done[k] = true
		}
	}
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			flush()
			section = t[1 : len(t)-1]
		}
		if section == pool && !strings.HasPrefix(t, ";") {
			if k, v, ok := strings.Cut(t, "="); ok {
				k = strings.TrimSpace(k)
				if nv, want := values[k]; want && !done[k] {
					old[k] = strings.TrimSpace(strings.SplitN(v, ";", 2)[0])
					if nv != "" {
						b.WriteString(k + " = " + nv + " ; liops tuning\n")
					}
					done[k] = true
					continue
				}
			}
		}
		if !strings.HasSuffix(l, "\n") {
			l += "\n"
		}
		b.WriteString(l)
	}
	flush()
	return b.String(), old
}

func applyRedisTuning(ctx context.Context, env TuningEnv, values map[string]string) (map[string]string, error) {
	args := []string{"-s", env.RedisAddr}
	if host, port, ok := strings.Cut(env.RedisAddr, ":"); ok && !strings.HasPrefix(env.RedisAddr, "/") {
		args = []string{"-h", host, "-p", port}
	}
	var lines []string
	if env.RedisPassword != "" {
		lines = append(lines, "AUTH "+redisQuote(env.RedisPassword))
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
		lines = append(lines, "CONFIG GET "+k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, "CONFIG SET "+k+" "+values[k])
	}
	lines = append(lines, "CONFIG REWRITE")
	out, err := env.Run.Run(ctx, command.Cmd{Name: "redis-cli", Args: args, Stdin: strings.NewReader(strings.Join(lines, "\n") + "\n")})
	text := string(out)
	for _, l := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(l, "NOAUTH"), strings.HasPrefix(l, "WRONGPASS"):
			return nil, ErrRedisPassword
		case strings.HasPrefix(l, "ERR"), strings.HasPrefix(l, "(error)"):
			return nil, fmt.Errorf("redis: %s", l)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("redis-cli: %w\n%s", err, text)
	}
	// CONFIG GET answers name, value (one per line in redis-cli's plain output).
	old := map[string]string{}
	f := strings.Fields(text)
	for i := 0; i+1 < len(f); i++ {
		if _, want := values[f[i]]; want && old[f[i]] == "" {
			old[f[i]] = f[i+1]
		}
	}
	return old, nil
}
