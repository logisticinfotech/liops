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
	"github.com/logisticinfotech/liops/app/php"
)

// PHPSlowTimeout is the request_slowlog_timeout liops sets on pools without a slow log:
// PHP writes the stack trace of every request slower than this.
const PHPSlowTimeout = "3s"

// PHPPlan is what liops would change so the monitor can read PHP-FPM, for a person to
// approve.
type PHPPlan struct {
	Files    []WebLogFile // pool files: lines added to their sections
	Script   bool         // the OPcache script to install or update
	Versions []string     // PHP versions to check and reload
	Pools    []php.Pool
	Done     bool
}

// PlanPHPFPM works out the PHP-FPM changes: each pool needs pm.status_path (its status
// page, only reachable through its socket) and a slow log, and OPcache needs liops's
// read-only script.
func PlanPHPFPM(etc string) (PHPPlan, error) {
	var plan PHPPlan
	pools, err := php.FindPools(etc)
	if err != nil {
		return plan, err
	}
	plan.Pools = pools
	byFile := map[string][]php.Pool{}
	for _, p := range pools {
		byFile[p.File] = append(byFile[p.File], p)
	}
	versions := map[string]bool{}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return plan, err
		}
		after, sites, adds := planPoolFile(string(data), byFile[f])
		if len(adds) > 0 {
			plan.Files = append(plan.Files, WebLogFile{Path: f, Sites: sites, Adds: adds, before: data, after: []byte(after)})
			versions[byFile[f][0].Version] = true
		}
	}
	if len(pools) > 0 {
		if old, _ := os.ReadFile(php.OPcacheScript); string(old) != php.OPcacheScriptSource {
			plan.Script = true
		}
	}
	for v := range versions {
		plan.Versions = append(plan.Versions, v)
	}
	sort.Strings(plan.Versions)
	plan.Done = len(plan.Files) == 0 && !plan.Script
	return plan, nil
}

// planPoolFile adds the missing settings at the end of each pool's section.
func planPoolFile(data string, pools []php.Pool) (after string, names, adds []string) {
	lines := strings.SplitAfter(data, "\n")
	if len(lines) > 0 && !strings.HasSuffix(lines[len(lines)-1], "\n") {
		lines[len(lines)-1] += "\n"
	}
	need := map[string][]string{}
	for _, p := range pools {
		var add []string
		if p.StatusPath == "" {
			add = append(add, "pm.status_path = "+php.StatusPath)
		}
		if !p.SlowLogOn() {
			add = append(add, "request_slowlog_timeout = "+PHPSlowTimeout, "slowlog = /var/log/php"+p.Version+"-fpm."+p.Name+".slow.log")
		}
		if len(add) > 0 {
			need[p.Name] = add
			names = append(names, p.Label())
			adds = append(adds, add...)
		}
	}
	if len(need) == 0 {
		return data, nil, nil
	}
	var b strings.Builder
	section := ""
	flush := func(more string) {
		if add := need[section]; len(add) > 0 {
			b.WriteString("; liops monitoring\n" + strings.Join(add, "\n") + "\n" + more)
		}
	}
	for _, l := range lines {
		if t := strings.TrimSpace(l); strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			flush("\n")
			section = t[1 : len(t)-1]
		}
		b.WriteString(l)
	}
	flush("")
	return b.String(), names, adds
}

// ApplyPHPFPM writes the plan (copies of the files in backupDir), checks each changed PHP
// version's config (php-fpmX.Y -t) and reloads it. When a check fails, its files are put
// back and nothing is reloaded.
func ApplyPHPFPM(ctx context.Context, run command.Runner, plan PHPPlan, backupDir string) error {
	if plan.Done {
		return nil
	}
	if plan.Script {
		if err := os.MkdirAll(filepath.Dir(php.OPcacheScript), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(php.OPcacheScript, php.OPcacheScriptSource, 0o644); err != nil {
			return err
		}
	}
	if len(plan.Files) == 0 {
		return nil
	}
	stamp := filepath.Join(backupDir, "php-fpm-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(stamp, 0o700); err != nil {
		return err
	}
	restore := func() {
		for _, f := range plan.Files {
			writeAtomic(f.Path, string(f.before), fileMode(f.Path))
		}
	}
	for i, f := range plan.Files {
		if err := os.WriteFile(filepath.Join(stamp, fmt.Sprintf("%d-%s", i, filepath.Base(f.Path))), f.before, 0o600); err != nil {
			return err
		}
		if err := writeAtomic(f.Path, string(f.after), fileMode(f.Path)); err != nil {
			restore()
			return err
		}
	}
	for _, v := range plan.Versions {
		if out, err := run.Run(ctx, command.Cmd{Name: "php-fpm" + v, Args: []string{"-t"}}); err != nil {
			restore()
			return fmt.Errorf("the PHP %s config check failed, so liops put the files back and reloaded nothing: %w\n%s", v, err, out)
		}
	}
	for _, v := range plan.Versions {
		if out, err := run.Run(ctx, command.Cmd{Name: "systemctl", Args: []string{"reload", "php" + v + "-fpm"}}); err != nil {
			return fmt.Errorf("reload php%s-fpm: %w\n%s", v, err, out)
		}
	}
	return nil
}
