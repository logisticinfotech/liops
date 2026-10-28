// SPDX-License-Identifier: Apache-2.0

// Package command runs the external programs liops drives (pgbackrest, psql,
// pg_dump), optionally as another OS user, and keeps the end of their output for
// logs and failure emails.
package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"strings"
	"sync"
	"syscall"
	"time"
)

// maxOutput bounds how much output is kept in memory per command. The end of the
// output is what explains a failure, so the start is dropped first.
const maxOutput = 256 << 10

// Cmd is a program to run.
type Cmd struct {
	Name string
	Args []string
	// User runs the program as this OS user (through runuser) when it isn't the
	// current user. Database commands run as the postgres user.
	User string
}

// String returns the command line, for logs.
func (c Cmd) String() string {
	s := c.Name
	if len(c.Args) > 0 {
		s += " " + strings.Join(c.Args, " ")
	}
	if c.User != "" {
		s += " (as " + c.User + ")"
	}
	return s
}

// Runner runs commands. Tests replace it with a fake.
type Runner interface {
	// Run runs the command and returns its combined stdout and stderr.
	Run(ctx context.Context, c Cmd) ([]byte, error)
	// Stream runs the command with stdout written to w. stderr is kept for the error.
	Stream(ctx context.Context, c Cmd, w io.Writer) error
}

// Error is returned when a command can't start or exits unsuccessfully.
type Error struct {
	Cmd      string
	ExitCode int // -1 if the command didn't start or was killed
	Output   string
	Err      error
}

func (e *Error) Error() string {
	msg := fmt.Sprintf("%s: %v", e.Cmd, e.Err)
	if last := LastLines(e.Output, 1); last != "" {
		msg += ": " + last
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Err }

// OutputOf returns the command output carried by err, if any.
func OutputOf(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Output
	}
	return ""
}

// Exec runs commands on this machine.
type Exec struct{}

func (Exec) Run(ctx context.Context, c Cmd) ([]byte, error) {
	cmd := build(ctx, c)
	var out tail
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.Bytes(), wrap(c, cmd, err, out.String())
}

func (Exec) Stream(ctx context.Context, c Cmd, w io.Writer) error {
	cmd := build(ctx, c)
	var stderr tail
	cmd.Stdout = w
	cmd.Stderr = &stderr
	err := cmd.Run()
	return wrap(c, cmd, err, stderr.String())
}

func build(ctx context.Context, c Cmd) *exec.Cmd {
	name, args := c.Name, c.Args
	if c.User != "" && !isCurrentUser(c.User) {
		name, args = "runuser", append([]string{"-u", c.User, "--", c.Name}, c.Args...)
	}
	cmd := exec.CommandContext(ctx, name, args...)
	// On cancellation ask nicely first (runuser passes SIGTERM on), then kill.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 30 * time.Second
	return cmd
}

func wrap(c Cmd, cmd *exec.Cmd, err error, output string) error {
	if err == nil {
		return nil
	}
	code := -1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return &Error{Cmd: c.String(), ExitCode: code, Output: output, Err: err}
}

func isCurrentUser(name string) bool {
	u, err := user.Current()
	return err == nil && u.Username == name
}

// LastLines returns the last n non-empty lines of s.
func LastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	var kept []string
	for i := len(lines) - 1; i >= 0 && len(kept) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			kept = append(kept, lines[i])
		}
	}
	for i, j := 0, len(kept)-1; i < j; i, j = i+1, j-1 {
		kept[i], kept[j] = kept[j], kept[i]
	}
	return strings.Join(kept, "\n")
}

// tail is an io.Writer that keeps the last maxOutput bytes written to it.
type tail struct {
	mu        sync.Mutex
	buf       []byte
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - maxOutput; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.truncated = true
	}
	return len(p), nil
}

func (t *tail) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.truncated {
		return "[earlier output dropped]\n" + string(t.buf)
	}
	return string(t.buf)
}
