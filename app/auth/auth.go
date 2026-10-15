// SPDX-License-Identifier: Apache-2.0

// Package auth handles UI passwords (bcrypt) and TOTP codes (RFC 6238, 30-second
// steps, compatible with Google Authenticator, 1Password, etc.).
package auth

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"math/big"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLength is enforced for every password.
const MinPasswordLength = 12

// CheckNewPassword returns a plain-language reason a password isn't acceptable.
func CheckNewPassword(pw string) error {
	if utf8.RuneCountInString(pw) < MinPasswordLength {
		return fmt.Errorf("the password must be at least %d characters", MinPasswordLength)
	}
	if len(pw) > 72 {
		return errors.New("the password must be at most 72 bytes") // bcrypt's limit
	}
	return nil
}

// Cost is the bcrypt cost. Tests lower it.
var Cost = 12

// HashPassword returns a bcrypt hash.
func HashPassword(pw string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(pw), Cost)
}

// dummyHash makes a failed lookup take as long as a wrong password, so the login
// form doesn't reveal which usernames exist.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("pgtijori-dummy-password"), 12)

// CheckPassword reports whether pw matches hash. A nil hash (unknown user) still
// costs one bcrypt comparison.
func CheckPassword(hash []byte, pw string) bool {
	if hash == nil {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(pw))
		return false
	}
	return bcrypt.CompareHashAndPassword(hash, []byte(pw)) == nil
}

// RandomPassword returns a random password that is easy to type: 20 characters
// from an alphabet without look-alikes.
func RandomPassword() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	var b strings.Builder
	for i := 0; i < 20; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		if err != nil {
			panic(err)
		}
		if i > 0 && i%5 == 0 {
			b.WriteByte('-')
		}
		b.WriteByte(alphabet[n.Int64()])
	}
	return b.String()
}

// Enrollment is a new TOTP secret to show to the user.
type Enrollment struct {
	Secret string
	URL    string // otpauth:// URL
	QR     string // PNG as a data: URI
}

// NewEnrollment creates a TOTP secret for account on server. Passing an existing
// (base32) secret shows that one again, e.g. after a mistyped code.
func NewEnrollment(server, account, secret string) (*Enrollment, error) {
	opts := totp.GenerateOpts{Issuer: "pgtijori " + server, AccountName: account}
	if secret != "" {
		raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
		if err != nil {
			return nil, err
		}
		opts.Secret = raw
	}
	key, err := totp.Generate(opts)
	if err != nil {
		return nil, err
	}
	img, err := key.Image(240, 240)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return &Enrollment{
		Secret: key.Secret(),
		URL:    key.URL(),
		QR:     "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()),
	}, nil
}

const period = 30

var validateOpts = totp.ValidateOpts{Period: period, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}

// VerifyTOTP checks a code against secret, allowing one 30-second step of clock
// drift either way. A code is accepted only for a step after lastStep, so a code
// can't be used twice. It returns the step to store as the new lastStep.
func VerifyTOTP(secret, code string, lastStep int64, now time.Time) (step int64, ok bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return lastStep, false
	}
	current := now.Unix() / period
	for _, s := range []int64{current - 1, current, current + 1} {
		if s <= lastStep {
			continue
		}
		want, err := totp.GenerateCodeCustom(secret, time.Unix(s*period, 0), validateOpts)
		if err != nil {
			return lastStep, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return s, true
		}
	}
	return lastStep, false
}
