// SPDX-License-Identifier: Apache-2.0

package laravel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// OpenUnder opens path as a regular file, without following a symlink, and only if
// the resolved directory stays under root. Laravel's storage/logs is writable by the
// app user and serve runs as root, so a symlink there must not be followed out of
// the app.
func OpenUnder(root, path string) (*os.File, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(pathAbs))
	if err != nil {
		return nil, err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	sep := string(os.PathSeparator)
	prefix := strings.TrimSuffix(rootAbs, sep) + sep
	if dir != rootAbs && !strings.HasPrefix(dir, prefix) {
		return nil, fmt.Errorf("refusing to follow a symlink")
	}
	fd, err := syscall.Open(pathAbs, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if err == syscall.ELOOP {
			return nil, fmt.Errorf("refusing to follow a symlink")
		}
		return nil, err
	}
	f := os.NewFile(uintptr(fd), pathAbs)
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("not a regular file")
	}
	return f, nil
}
