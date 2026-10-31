// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/logisticinfotech/liops/app/monitor"
)

// Where the liops-monitor service keeps its socket and history (systemd's
// RuntimeDirectory and StateDirectory in deploy/liops-monitor.service).
const (
	monitorDataDir = "/var/lib/liops-monitor"
	monitorUser    = "liops-monitor"
)

// runMonitor is the liops-monitor service: it samples the machine every 15 s, keeps the
// history, and answers the UI on a Unix socket. It runs as its own unprivileged user and
// never reads liops's config or credentials.
func runMonitor(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("monitor", flag.ContinueOnError)
	d := defaultPaths()
	socket := flags.String("socket", d.monitorSocket, "the Unix socket the UI reads from")
	data := flags.String("data", d.monitorData, "where the history (metrics.db) is kept")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := os.MkdirAll(*data, 0o700); err != nil {
		return err
	}
	store, err := monitor.OpenStore(filepath.Join(*data, "metrics.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signalContext()
	defer stop()
	m := monitor.New(monitor.NewCollector("/"), store, os.Stderr)
	errc := make(chan error, 1)
	go func() { errc <- monitor.Serve(ctx, *socket, m.Handler()) }()
	fmt.Fprintf(out, "liops monitor: sampling every %s, answering on %s\n", monitor.Interval, *socket)
	if err := m.Run(ctx); err != nil {
		return err
	}
	stop()
	return <-errc
}
