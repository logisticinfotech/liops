// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	out, err := Exec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo hello; echo oops >&2; exit 3"}})
	if !strings.Contains(string(out), "hello") || !strings.Contains(string(out), "oops") {
		t.Errorf("output should hold stdout and stderr, got %q", out)
	}
	var ce *Error
	if !errors.As(err, &ce) || ce.ExitCode != 3 {
		t.Fatalf("expected *Error with exit code 3, got %v", err)
	}
	if !strings.Contains(err.Error(), "oops") {
		t.Errorf("error should end with the last output line, got %q", err)
	}
}

func TestStreamSeparatesStdout(t *testing.T) {
	var stdout bytes.Buffer
	err := Exec{}.Stream(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "printf data; echo warn >&2"}}, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "data" {
		t.Errorf("stdout = %q, want %q", stdout.String(), "data")
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	var tl tail
	tl.Write(bytes.Repeat([]byte("a"), maxOutput))
	tl.Write([]byte("the end"))
	s := tl.String()
	if !strings.HasSuffix(s, "the end") || !strings.HasPrefix(s, "[earlier output dropped]") {
		t.Errorf("tail should keep the end and mark the drop, got prefix %q suffix %q", s[:30], s[len(s)-10:])
	}
}

func TestLastLines(t *testing.T) {
	if got := LastLines("a\nb\n\nc\n\n", 2); got != "b\nc" {
		t.Errorf("LastLines = %q", got)
	}
}
