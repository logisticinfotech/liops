// SPDX-License-Identifier: Apache-2.0

package monitor

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"
)

// Log formats.
const (
	FormatNginx = "nginx" // NginxLogFormat
	FormatCaddy = "caddy" // Caddy's JSON access log
)

// DefaultCaddyfile is where Ubuntu's Caddy package keeps its config.
const DefaultCaddyfile = "/etc/caddy/Caddyfile"

// LogSource is an access log the monitor reads: a file, or a systemd unit's journal.
type LogSource struct {
	Format string `json:"format"`
	Path   string `json:"path,omitempty"`
	Unit   string `json:"unit,omitempty"`
}

func (s LogSource) String() string {
	if s.Unit != "" {
		return "journal of " + s.Unit
	}
	return s.Path
}

var caddyOutputRE = regexp.MustCompile(`(?m)^\s*output\s+file\s+(\S+)`)

// FindLogSources lists the access logs on this machine: nginx's liops log once setup has
// added it, and Caddy's (files named in the Caddyfile, and its journal for sites that
// log to the default output).
func FindLogSources(nginxLog, caddyfile string) []LogSource {
	var out []LogSource
	if _, err := os.Stat(nginxLog); err == nil {
		out = append(out, LogSource{Format: FormatNginx, Path: nginxLog})
	}
	if data, err := os.ReadFile(caddyfile); err == nil {
		for _, m := range caddyOutputRE.FindAllStringSubmatch(string(data), -1) {
			out = append(out, LogSource{Format: FormatCaddy, Path: m[1]})
		}
		out = append(out, LogSource{Format: FormatCaddy, Unit: "caddy.service"})
	}
	return out
}

// maxReadPerPoll bounds one poll of a file, so a huge burst can't stall the loop.
const maxReadPerPoll = 64 << 20

// fileTail follows a log file from where it ended when first opened, through rotation
// (a new inode) and truncation.
type fileTail struct {
	path    string
	f       *os.File
	ino     uint64
	off     int64
	partial []byte
}

func inode(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

// poll reads the new lines and calls fn for each.
func (t *fileTail) poll(fn func([]byte)) error {
	if t.f == nil {
		f, err := os.Open(t.path)
		if err != nil {
			return err
		}
		fi, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		t.f, t.ino, t.partial = f, inode(fi), nil
		// Start at the end: what's already there was before the monitor looked.
		if t.off, err = f.Seek(0, io.SeekEnd); err != nil {
			return err
		}
	}
	t.read(fn)
	fi, err := os.Stat(t.path)
	switch {
	case err != nil:
		return nil // rotated away; the new file appears soon
	case inode(fi) != t.ino:
		// Rotated: the old file is read to its end above; the new one from its start.
		t.f.Close()
		f, err := os.Open(t.path)
		if err != nil {
			t.f = nil
			return err
		}
		t.f, t.ino, t.off, t.partial = f, inode(fi), 0, nil
		t.read(fn)
	case fi.Size() < t.off:
		t.f.Seek(0, io.SeekStart) // truncated (copytruncate)
		t.off, t.partial = 0, nil
		t.read(fn)
	}
	return nil
}

func (t *fileTail) read(fn func([]byte)) {
	buf := make([]byte, 256<<10)
	read := 0
	for read < maxReadPerPoll {
		n, err := t.f.Read(buf)
		if n > 0 {
			read += n
			t.off += int64(n)
			data := append(t.partial, buf[:n]...)
			for {
				i := bytes.IndexByte(data, '\n')
				if i < 0 {
					break
				}
				fn(data[:i])
				data = data[i+1:]
			}
			t.partial = append([]byte(nil), data...)
			if len(t.partial) > 1<<20 {
				t.partial = nil // not a log line
			}
		}
		if err != nil || n == 0 {
			return
		}
	}
}

func (t *fileTail) close() {
	if t.f != nil {
		t.f.Close()
	}
}

// parseLine reads a line in a source's format.
func parseLine(format string, line []byte) (Request, bool) {
	if format == FormatNginx {
		return parseNginx(line)
	}
	return parseCaddy(line)
}

// followJournal reads a unit's new journal lines until ctx is done, starting journalctl
// again when it exits.
func (m *Monitor) followJournal(ctx context.Context, src LogSource) {
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, "journalctl", "--unit", src.Unit, "--follow", "--lines=0", "--output=cat", "--no-pager")
		out, err := cmd.StdoutPipe()
		if err == nil && cmd.Start() == nil {
			sc := bufio.NewScanner(out)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() {
				if r, ok := parseLine(src.Format, sc.Bytes()); ok {
					m.AddRequest(r)
				}
			}
			cmd.Wait()
		}
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
	}
}

// reqLoop reads the access logs every second and writes each finished minute.
func (m *Monitor) reqLoop(ctx context.Context) {
	tails := map[string]*fileTail{}
	journals := map[string]context.CancelFunc{}
	defer func() {
		for _, t := range tails {
			t.close()
		}
		for _, cancel := range journals {
			cancel()
		}
	}()
	sources := m.LogSources
	find := func() {
		if m.FindLogs != nil {
			sources = m.FindLogs()
		}
		a := m.reqs()
		a.mu.Lock()
		a.sources = len(sources) > 0
		a.mu.Unlock()
		for _, s := range sources {
			if s.Unit != "" {
				if _, ok := journals[s.Unit]; !ok {
					jctx, cancel := context.WithCancel(ctx)
					journals[s.Unit] = cancel
					go m.followJournal(jctx, s)
				}
			} else if _, ok := tails[s.Path]; !ok {
				tails[s.Path] = &fileTail{path: filepath.Clean(s.Path)}
			}
		}
	}
	find()
	formats := func(path string) string {
		for _, s := range sources {
			if s.Path == path {
				return s.Format
			}
		}
		return FormatNginx
	}
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	refind := time.NewTicker(QueryInterval)
	defer refind.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refind.C:
			find()
		case <-poll.C:
			for path, t := range tails {
				format := formats(path)
				t.poll(func(line []byte) {
					if r, ok := parseLine(format, line); ok {
						m.AddRequest(r)
					}
				})
			}
			m.flushRequests(m.Now())
		}
	}
}
