// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/logisticinfotech/liops/app/command"
	"github.com/logisticinfotech/liops/app/engines/mysql"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/http/controllers"
	"github.com/logisticinfotech/liops/app/monitor"
	"github.com/logisticinfotech/liops/app/php"
	"github.com/logisticinfotech/liops/app/service"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/config"
)

// runMonitorSetup is Settings → Monitoring from the command line: it lists what monitoring
// needs changed (database logins, web server logs, PHP-FPM, Redis) and, with --apply,
// changes it. It never restarts PostgreSQL: it prints the command instead.
func runMonitorSetup(args []string, out io.Writer) error {
	flags, p := newFlags("monitor-setup")
	apply := flags.Bool("apply", false, "make the changes (without it, only list them)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireRoot("liops monitor-setup"); err != nil {
		return err
	}
	cfg, err := config.LoadDraft(p.config)
	if err != nil {
		cfg = config.Default()
	}
	ctx, stop := signalContext()
	defer stop()
	run := command.Exec{}
	backups := "/var/lib/liops/backups"
	var problems []string
	planned := func(what string, err error) {
		if err != nil {
			problems = append(problems, what+": "+err.Error())
			fmt.Fprintf(out, "  ✗ %v\n", err)
		}
	}
	step := func(what string, err error) {
		if err != nil {
			problems = append(problems, what+": "+err.Error())
			fmt.Fprintf(out, "  ✗ %v\n", err)
		} else if *apply {
			fmt.Fprintln(out, "  ✓ done")
		}
	}

	fmt.Fprintln(out, "Databases (the monitor's read-only login):")
	found := false
	// Every database server on the machine, not only the one liops backs up.
	if _, err := os.Stat(cfg.MySQL.Socket); err == nil {
		found = true
		fmt.Fprintln(out, "  MySQL: 'liops-monitor'@'localhost' IDENTIFIED WITH auth_socket (PROCESS, REFERENCES, performance_schema, sys)")
		if *apply {
			err := setup.MySQLMonitorAccess(ctx, mysql.Instance{Cfg: cfg.MySQL, Run: run}, "")
			if errors.Is(err, setup.ErrNeedRootPassword) {
				err = errors.New("MySQL's root needs a password: use Set up database access in Settings → Monitoring, which asks for it")
			}
			step("MySQL", err)
		}
	}
	if clusters, err := setup.DetectClusters(ctx, run); err == nil {
		for _, c := range clusters {
			if !c.Online() {
				continue
			}
			found = true
			pcfg := *cfg
			setup.Apply(&pcfg, c)
			fmt.Fprintf(out, "  PostgreSQL %d (port %d): the role liops-monitor in pg_monitor, and pg_stat_statements\n", pcfg.Postgres.Version, pcfg.Postgres.Port)
			if *apply {
				restart, err := setup.PostgresMonitorAccess(ctx, postgres.Cluster{Cfg: pcfg.Postgres, Run: run})
				step("PostgreSQL", err)
				if restart {
					fmt.Fprintf(out, "  PostgreSQL needs one restart to load pg_stat_statements, at a quiet time: %s\n", setup.RestartCommand(&pcfg))
				}
			}
		}
	}
	if !found {
		fmt.Fprintln(out, "  no PostgreSQL or MySQL found")
	}

	fmt.Fprintln(out, "Web servers (an access log liops reads):")
	var plans []setup.WebLogPlan
	if _, err := os.Stat(setup.NginxDir); err == nil {
		pl, err := setup.PlanNginxLogs(setup.NginxDir)
		planned("nginx", err)
		plans = append(plans, pl)
	}
	if _, err := os.Stat(setup.ApacheDir); err == nil {
		pl, err := setup.PlanApacheLogs(setup.ApacheDir)
		planned("Apache", err)
		plans = append(plans, pl)
	}
	if _, err := os.Stat(setup.Caddyfile); err == nil {
		pl, err := setup.PlanCaddyLogs(setup.Caddyfile)
		planned("Caddy", err)
		plans = append(plans, pl)
	}
	for _, pl := range plans {
		printPlan(out, pl.Server, pl.Done, pl.Files, pl.Skipped)
		if *apply && !pl.Done {
			step(pl.Server, setup.ApplyWebLogs(ctx, run, pl, backups))
		}
	}
	if len(plans) == 0 {
		fmt.Fprintln(out, "  no nginx, Apache or Caddy config found")
	}

	fmt.Fprintln(out, "PHP-FPM (status pages, slow log, OPcache script):")
	if pl, err := setup.PlanPHPFPM(php.DefaultEtc); err != nil || len(pl.Pools) == 0 {
		fmt.Fprintln(out, "  no PHP-FPM pools found")
	} else {
		printPlan(out, "PHP-FPM", pl.Done, pl.Files, nil)
		if pl.Script {
			fmt.Fprintln(out, "  install "+php.OPcacheScript)
		}
		if *apply && !pl.Done {
			step("PHP-FPM", setup.ApplyPHPFPM(ctx, run, pl, backups))
		}
	}

	fmt.Fprintln(out, "Redis (the monitor's ACL user: INFO, SLOWLOG GET, and queue lengths):")
	if r := monitor.FindRedis(monitor.DefaultRedis); r != nil && reachable(r.Addr) {
		fmt.Fprintln(out, "  ACL user liops-monitor at "+r.Addr)
		if *apply {
			err := setup.RedisMonitorAccess(ctx, run, r.Addr, "")
			if errors.Is(err, setup.ErrRedisPassword) {
				err = errors.New("Redis has a password: use Set up Redis access in Settings → Monitoring, which asks for it")
			}
			step("Redis", err)
		}
	} else {
		fmt.Fprintln(out, "  no Redis at "+monitor.DefaultRedis)
	}

	if !*apply {
		fmt.Fprintln(out, "\nNothing was changed. Run again with --apply to make these changes (files are backed up and checked, services reloaded, never restarted).")
		return nil
	}
	if len(problems) > 0 {
		return fmt.Errorf("%d step(s) failed:\n  %s", len(problems), strings.Join(problems, "\n  "))
	}
	return nil
}

func printPlan(out io.Writer, name string, done bool, files []setup.WebLogFile, skipped []string) {
	if done {
		fmt.Fprintf(out, "  %s: ready\n", name)
	}
	for _, f := range files {
		fmt.Fprintf(out, "  %s: add to %s", name, f.Path)
		if len(f.Sites) > 0 {
			fmt.Fprintf(out, " (%s)", strings.Join(f.Sites, ", "))
		}
		fmt.Fprintln(out)
		for _, a := range f.Adds {
			fmt.Fprintln(out, "      "+a)
		}
	}
	for _, s := range skipped {
		fmt.Fprintln(out, "  left alone: "+s)
	}
}

func reachable(addr string) bool {
	if strings.HasPrefix(addr, "/") {
		_, err := os.Stat(addr)
		return err == nil
	}
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// runVerdict prints the monitoring verdict (Monitoring → Overview) in the terminal.
func runVerdict(args []string, out io.Writer) error {
	_, p, err := parseFlags("verdict", args)
	if err != nil {
		return err
	}
	if err := requireRoot("liops verdict"); err != nil {
		return err
	}
	st, err := state.Open(p.state)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	svc, err := service.New(ctx, p.config, p.lock(), st, io.Discard, nil)
	if err != nil {
		return err
	}
	defer svc.Close()
	if !svc.Config().Monitor.Enabled {
		return errors.New("monitoring is off: turn it on in Settings → Monitoring (or monitor.enabled: true in the config)")
	}
	ui, err := controllers.New(svc)
	if err != nil {
		return err
	}
	ui.Monitor = monitor.NewClient(p.monitorSocket)
	lines := ui.AllVerdictLines(ctx)
	if lines == nil {
		return errors.New("the monitoring service isn't answering: sudo systemctl status liops-monitor")
	}
	fmt.Fprintln(out, strings.Join(lines, "\n"))
	return nil
}
