// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"io"
	"path/filepath"
	"sync"
)

// Fake is a Runner for tests. Handle gets each command, keyed by its program's base
// name, and returns the command's stdout and error.
type Fake struct {
	Handle func(program string, c Cmd) ([]byte, error)

	mu    sync.Mutex
	Calls []Cmd
}

func (f *Fake) record(c Cmd) ([]byte, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, c)
	f.mu.Unlock()
	if f.Handle == nil {
		return nil, nil
	}
	return f.Handle(filepath.Base(c.Name), c)
}

func (f *Fake) Run(ctx context.Context, c Cmd) ([]byte, error) { return f.record(c) }

func (f *Fake) Stream(ctx context.Context, c Cmd, w io.Writer) error {
	out, err := f.record(c)
	if len(out) > 0 {
		if _, werr := w.Write(out); werr != nil && err == nil {
			err = werr
		}
	}
	return err
}

// Programs returns the base names of the programs run so far, in order.
func (f *Fake) Programs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, c := range f.Calls {
		names = append(names, filepath.Base(c.Name))
	}
	return names
}
