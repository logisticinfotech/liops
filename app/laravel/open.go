// SPDX-License-Identifier: Apache-2.0

package laravel

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// AppRoot is the folder OpenUnder treats as the app: the directory itself, or the
// deploy base (the parent of current) when dir is a current symlink whose parent
// holds releases/ or shared/. A symlink at /app would otherwise make the parent /,
// which turns the protection off.
func AppRoot(dir string) string {
	fi, err := os.Lstat(dir)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return dir
	}
	parent := filepath.Clean(filepath.Dir(dir))
	if parent == string(os.PathSeparator) || parent == "." {
		return dir
	}
	if deployBase(parent) {
		return parent
	}
	return dir
}

func deployBase(dir string) bool {
	for _, name := range []string{"releases", "shared"} {
		st, err := os.Stat(filepath.Join(dir, name))
		if err == nil && st.IsDir() {
			return true
		}
	}
	return false
}

// OpenUnder opens path as a regular file only if it stays under root after following
// symlinks. os.OpenRoot does the check and the open as one step, so the app user
// can't swap storage/logs for a symlink in between. current → releases/N and
// storage → shared/storage are allowed when root is the deploy base.
func OpenUnder(root, path string) (*os.File, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("refusing to follow a symlink")
	}
	r, err := os.OpenRoot(rootAbs)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	f, err := r.Open(rel)
	if err != nil {
		if escapesRoot(err) {
			return nil, fmt.Errorf("refusing to follow a symlink")
		}
		return nil, err
	}
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

func escapesRoot(err error) bool {
	s := err.Error()
	return strings.Contains(s, "escapes") || strings.Contains(s, "symlink")
}
