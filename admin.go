// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/logisticinfotech/liops/app/engines/pgbackrest"
	"github.com/logisticinfotech/liops/app/engines/pgdump"
	"github.com/logisticinfotech/liops/app/engines/postgres"
	"github.com/logisticinfotech/liops/app/notify"
	"github.com/logisticinfotech/liops/app/setup"
	"github.com/logisticinfotech/liops/app/state"
	"github.com/logisticinfotech/liops/app/storage"
	"github.com/logisticinfotech/liops/app/version"
	"github.com/logisticinfotech/liops/config"
)

//go:embed deploy/liops.service
var systemdUnit []byte

//go:embed deploy/liops.logrotate
var logrotateConf []byte

const (
	binPath       = "/usr/local/bin/liops"
	unitPath      = "/etc/systemd/system/liops.service"
	logrotatePath = "/etc/logrotate.d/liops"
	configDir     = "/etc/liops"
	stateDir      = "/var/lib/liops"
	logDir        = "/var/log/liops"
)

// runLogs prints the end of the log, and with -f keeps printing as it grows.
func runLogs(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("logs", flag.ContinueOnError)
	n := flags.Int("n", 100, "number of lines")
	follow := flags.Bool("f", false, "keep printing new lines")
	path := flags.String("log", defaultPaths().log, "log file path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Keep the last n lines while reading through.
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > *n {
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		fmt.Fprintln(out, l)
	}
	if !*follow {
		return sc.Err()
	}

	ctx, stop := signalContext()
	defer stop()
	offset, _ := f.Seek(0, io.SeekEnd)
	buf := make([]byte, 64<<10)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		info, err := os.Stat(*path)
		if err != nil {
			continue
		}
		if info.Size() < offset { // rotated with copytruncate
			offset = 0
		}
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return err
		}
		for {
			m, err := f.Read(buf)
			out.Write(buf[:m])
			offset += int64(m)
			if err != nil || m == 0 {
				break
			}
		}
	}
}

// parseFlagsWithArgs is parseFlags for commands that take a positional argument
// before or after the flags.
func parseFlagsWithArgs(name string, args []string) (*flag.FlagSet, *paths, error) {
	var positional, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			if !strings.Contains(a, "=") && i+1 < len(args) {
				rest = append(rest, args[i+1])
				i++
			}
			continue
		}
		positional = append(positional, a)
	}
	flags, p, err := parseFlags(name, append(rest, positional...))
	return flags, p, err
}

func systemctl(args ...string) error {
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemctl %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

// runInstallSystem installs the directories, systemd unit and logrotate file and
// (re)starts the service. install.sh runs it after placing the binary; running it
// again is how an upgrade restarts the service.
func runInstallSystem(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("install-system", flag.ContinueOnError)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireRoot("liops install-system"); err != nil {
		return err
	}
	for _, d := range []struct {
		path string
		mode os.FileMode
	}{{configDir, 0o700}, {stateDir, 0o700}, {logDir, 0o750}} {
		if err := os.MkdirAll(d.path, d.mode); err != nil {
			return err
		}
		if err := os.Chmod(d.path, d.mode); err != nil {
			return err
		}
	}
	if err := os.WriteFile(unitPath, systemdUnit, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(logrotatePath, logrotateConf, 0o644); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}

	// Left behind by early versions, which had a login and a setup token.
	_ = os.Remove(stateDir + "/setup-token")

	upgrade := exec.Command("systemctl", "is-active", "--quiet", "liops").Run() == nil
	if upgrade {
		if err := systemctl("restart", "liops"); err != nil {
			return err
		}
	} else if err := systemctl("enable", "--now", "liops"); err != nil {
		return err
	}

	listen := "127.0.0.1:8899"
	if cfg, err := config.LoadDraft(config.DefaultPath); err == nil {
		listen = cfg.UI.Listen
	}
	verb := "installed and started"
	if upgrade {
		verb = "upgraded and restarted; the config and backups are unchanged"
	}
	fmt.Fprintf(out, "\n%s is %s.\n", version.String(), verb)
	fmt.Fprintf(out, `
To open it, run this on your own computer and leave it open:

  ssh -L %s <you>@<this server>

then browse to http://localhost:%s`, tunnelSpec(listen), portOf(listen))
	if !upgrade {
		fmt.Fprint(out, " and follow the setup wizard.")
	}
	fmt.Fprintln(out, "\n\nThere's no login: anyone who can open that tunnel can use it.")
	return nil
}

func portOf(listen string) string {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		return listen[i+1:]
	}
	return listen
}

// runUninstall removes liops. Backups in storage are never touched, and pgBackRest
// keeps archiving WAL until archiving is turned off, so it says how.
func runUninstall(args []string, in io.Reader, out io.Writer) error {
	flags := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	purge := flags.Bool("purge", false, "also delete the config, state (users, job history) and logs")
	yes := flags.Bool("yes", false, "don't ask for confirmation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := requireRoot("liops uninstall"); err != nil {
		return err
	}
	cfg, _ := config.LoadDraft(config.DefaultPath)

	fmt.Fprintln(out, "This stops and removes the liops service, its systemd and logrotate files and the binary.")
	if *purge {
		fmt.Fprintf(out, "With --purge it also deletes %s (config with secrets), %s (users, job history) and %s.\n", configDir, stateDir, logDir)
	} else {
		fmt.Fprintf(out, "The config, state and logs are kept (%s, %s, %s); --purge deletes them too.\n", configDir, stateDir, logDir)
	}
	fmt.Fprintln(out, "Backups in storage are NOT deleted.")
	if !*yes {
		fmt.Fprint(out, "\nType yes to continue: ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			return errors.New("cancelled")
		}
	}

	_ = systemctl("disable", "--now", "liops")
	for _, f := range []string{unitPath, logrotatePath} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	_ = systemctl("daemon-reload")
	if *purge {
		for _, d := range []string{configDir, stateDir, logDir} {
			if err := os.RemoveAll(d); err != nil {
				return err
			}
		}
	}
	if exe, err := os.Executable(); err == nil && exe == binPath {
		if err := os.Remove(binPath); err != nil {
			return err
		}
	}

	fmt.Fprintln(out, "\nliops is removed.")
	if cfg != nil && cfg.PgBackRest.Stanza != "" {
		cluster := postgres.Cluster{Cfg: cfg.Postgres}
		fmt.Fprintf(out, `
Still in place, on purpose:
  - %s, and WAL archiving through %s.
    PostgreSQL keeps sending WAL to the bucket, but nothing takes new backups or
    expires old ones any more. To turn archiving off:
      sudo rm %s
      %s
  - The backups in folder %q of bucket %q. Delete that folder in your storage
    provider's console when you no longer need them.
`, pgbackrest.ConfPath(cfg), cluster.ConfDPath(), cluster.ConfDPath(), setup.RestartCommand(cfg), storage.Folder(cfg), cfg.Storage.S3.Bucket)
	}
	return nil
}

// runDumps lists stored dumps and prints download links, for restores over SSH.
func runDumps(sub string, args []string, out io.Writer) error {
	flags, p, err := parseFlagsWithArgs("dumps "+sub, args)
	if err != nil {
		return err
	}
	if err := requireRoot("liops dumps"); err != nil {
		return err
	}
	cfg, err := config.Load(p.config)
	if err != nil {
		return err
	}
	store, err := storage.Open(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	switch sub {
	case "list":
		prefix := storage.DumpsPrefix
		if db := flags.Arg(0); db != "" {
			prefix = pgdump.Prefix(db)
		}
		objs, err := store.List(ctx, prefix)
		if err != nil {
			return err
		}
		globals, err := store.List(ctx, storage.GlobalsPrefix)
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "KEY\tSIZE\tSTORED")
		for _, o := range append(objs, globals...) {
			fmt.Fprintf(tw, "%s\t%s\t%s\n", o.Key, notify.Bytes(o.Size), o.Modified.Format("2006-01-02 15:04"))
		}
		return tw.Flush()
	case "url":
		key := flags.Arg(0)
		if !strings.HasPrefix(key, storage.DumpsPrefix) && !strings.HasPrefix(key, storage.GlobalsPrefix) {
			return errors.New("dumps url: give a key from `liops dumps list`, e.g. dumps/app/2026-10-05T200102Z.dump")
		}
		url, err := store.DownloadURL(ctx, key, 10*time.Minute)
		if err != nil {
			return err
		}
		if st, err := state.Open(p.state); err == nil {
			_ = st.Audit(state.AuditEntry{Time: time.Now(), User: "root (CLI)", Action: "download", Detail: key, OK: true})
		}
		fmt.Fprintln(out, url)
		return nil
	default:
		return fmt.Errorf("dumps: unknown subcommand %q (expected list or url)", sub)
	}
}
