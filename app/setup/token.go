// SPDX-License-Identifier: Apache-2.0

package setup

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultTokenPath holds the one-time token that unlocks the setup wizard. The
// installer prints it; it's deleted once the first admin account exists.
const DefaultTokenPath = "/var/lib/pgtijori/setup-token"

// NewToken writes a fresh token (mode 0600) and returns it.
func NewToken(path string) (string, error) {
	b := make([]byte, 15)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	raw := base32.StdEncoding.EncodeToString(b) // 24 characters, A–Z and 2–7
	token := raw[0:6] + "-" + raw[6:12] + "-" + raw[12:18] + "-" + raw[18:24]
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		return "", err
	}
	return token, nil
}

// CheckToken reports whether given matches the stored token. Case, spaces and
// dashes don't matter. No token file means no token is accepted.
func CheckToken(path, given string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	norm := func(s string) string {
		return strings.NewReplacer("-", "", " ", "", "\n", "").Replace(strings.ToUpper(strings.TrimSpace(s)))
	}
	want := norm(string(data))
	return want != "" && subtle.ConstantTimeCompare([]byte(norm(given)), []byte(want)) == 1
}

// RemoveToken deletes the token.
func RemoveToken(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
