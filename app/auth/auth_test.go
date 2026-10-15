// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"
)

func TestPasswords(t *testing.T) {
	if CheckNewPassword("short") == nil {
		t.Error("short passwords must be rejected")
	}
	pw := RandomPassword()
	if CheckNewPassword(pw) != nil || strings.Count(pw, "-") != 3 {
		t.Errorf("random password %q", pw)
	}
	hash, err := HashPassword(pw)
	if err != nil {
		t.Fatal(err)
	}
	if !CheckPassword(hash, pw) || CheckPassword(hash, pw+"x") || CheckPassword(nil, pw) {
		t.Error("CheckPassword")
	}
}

func TestTOTP(t *testing.T) {
	e, err := NewEnrollment("db-1", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := NewEnrollment("db-1", "admin", e.Secret); err != nil || again.Secret != e.Secret {
		t.Fatalf("re-showing a secret: %v %v", again, err)
	}
	if !strings.HasPrefix(e.QR, "data:image/png;base64,") || !strings.HasPrefix(e.URL, "otpauth://totp/") {
		t.Errorf("enrollment = %+v", e)
	}
	now := time.Date(2026, 10, 15, 10, 0, 15, 0, time.UTC)
	code, _ := totp.GenerateCodeCustom(e.Secret, now, validateOpts)

	step, ok := VerifyTOTP(e.Secret, code[:3]+" "+code[3:], 0, now)
	if !ok {
		t.Fatal("a valid code (with a space) should pass")
	}
	if _, ok := VerifyTOTP(e.Secret, code, step, now); ok {
		t.Error("the same code must not be accepted twice")
	}
	if _, ok := VerifyTOTP(e.Secret, code, 0, now.Add(29*time.Second)); !ok {
		t.Error("one step of drift is allowed")
	}
	if _, ok := VerifyTOTP(e.Secret, code, 0, now.Add(2*time.Minute)); ok {
		t.Error("an old code must be rejected")
	}
	if _, ok := VerifyTOTP(e.Secret, "000000x", 0, now); ok {
		t.Error("garbage must be rejected")
	}
}
