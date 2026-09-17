package auth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCode = "AMAD-TEST0-00000-00000-0000T"

func newManager(t *testing.T) (*Manager, string) {
	t.Helper()
	AllowCodeForTests(testCode)
	dir := t.TempDir()
	m, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m, dir
}

func TestCodeNormalizeAndValidate(t *testing.T) {
	AllowCodeForTests(testCode)
	for _, in := range []string{testCode, "amad test0 00000 00000 0000t", "TESTO-OOOOO-OOOOO-OOOOT"} {
		if !ValidCode(in) {
			t.Errorf("ValidCode(%q) = false", in)
		}
	}
	for _, in := range []string{"", "AMAD-TEST0-00000-00000-0000", "AMAD-TEST0-00000-00000-0000U", "AMAD-TEST1-00000-00000-0000T"} {
		if ValidCode(in) {
			t.Errorf("ValidCode(%q) = true", in)
		}
	}
	if got := MaskCode("TEST0000000000000000"); got != "AMAD-TEST0-•••••-•••••-00000" {
		t.Errorf("mask = %q", got)
	}
	if len(codeHashes) < 100 {
		t.Fatalf("only %d issued codes embedded", len(codeHashes))
	}
}

func TestSetupLoginLogout(t *testing.T) {
	m, dir := newManager(t)
	if st := m.Status(""); st.Activated || st.Configured || st.Authenticated {
		t.Fatalf("fresh status = %+v", st)
	}
	if _, err := m.Login("c", "admin", "whatever1"); !errors.Is(err, ErrNotActivated) {
		t.Fatalf("login before setup: %v", err)
	}
	if _, err := m.Setup("c", "AMAD-TEST1-00000-00000-0000T", "admin", "s3cret-pass"); !errors.Is(err, ErrInvalidCode) {
		t.Fatalf("wrong code: %v", err)
	}
	if _, err := m.Setup("c", testCode, "ad", "s3cret-pass"); !errors.Is(err, ErrBadUsername) {
		t.Fatalf("short username: %v", err)
	}
	if _, err := m.Setup("c", testCode, "admin", "short"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("weak password: %v", err)
	}
	tok, err := m.Setup("c", testCode, "admin", "s3cret-pass")
	if err != nil {
		t.Fatal(err)
	}
	st := m.Status(tok)
	if !st.Activated || !st.Configured || !st.Authenticated || st.Username != "admin" || !strings.HasPrefix(st.Code, "AMAD-TEST0-") {
		t.Fatalf("after setup = %+v", st)
	}
	if _, err := m.Setup("c", testCode, "other", "another-pass"); !errors.Is(err, ErrAlreadySetUp) {
		t.Fatalf("second setup: %v", err)
	}

	// Stored files: no plaintext password, license not readable as JSON.
	acct, _ := os.ReadFile(filepath.Join(dir, userFile))
	lic, _ := os.ReadFile(filepath.Join(dir, licenseFile))
	if strings.Contains(string(acct), "s3cret-pass") || strings.Contains(string(lic), "TEST0") {
		t.Fatal("secrets stored in plaintext")
	}

	m.Logout(tok)
	if m.Valid(tok) {
		t.Fatal("session survived logout")
	}
	if _, err := m.Login("c", "admin", "wrong-pass"); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("wrong password: %v", err)
	}
	tok2, err := m.Login("c", " admin ", "s3cret-pass")
	if err != nil || !m.Valid(tok2) {
		t.Fatalf("login: %v", err)
	}

	// Reopen from disk: activation and account persist, sessions do not.
	m2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !m2.Activated() || m2.Valid(tok2) {
		t.Fatal("reopen: activation lost or session leaked")
	}
	if _, err := m2.Login("c", "admin", "s3cret-pass"); err != nil {
		t.Fatal(err)
	}
}

func TestLicenseBoundToMachine(t *testing.T) {
	m, dir := newManager(t)
	if _, err := m.Setup("c", testCode, "admin", "s3cret-pass"); err != nil {
		t.Fatal(err)
	}
	machineID = func() string { return "another-computer" }
	defer func() { machineID = func() string { return "test-machine" } }()
	m2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if m2.Activated() {
		t.Fatal("copied license activated another computer")
	}
	// Tampering with the file also invalidates it.
	machineID = func() string { return "test-machine" }
	path := filepath.Join(dir, licenseFile)
	b, _ := os.ReadFile(path)
	b[len(b)-1] ^= 0xFF
	_ = os.WriteFile(path, b, 0o600)
	if m3, _ := Open(dir); m3.Activated() {
		t.Fatal("tampered license accepted")
	}
}

func TestLockoutAndSessionExpiry(t *testing.T) {
	m, _ := newManager(t)
	clock := time.Date(2026, 9, 17, 9, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return clock }
	if _, err := m.Setup("c", testCode, "admin", "s3cret-pass"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < freeFails-1; i++ {
		if _, err := m.Login("bad", "admin", "nope-nope"); !errors.Is(err, ErrBadLogin) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := m.Login("bad", "admin", "nope-nope")
	var locked LockedError
	if !errors.As(err, &locked) || locked.Retry != 30*time.Second {
		t.Fatalf("5th failure should lock for 30s: %v", err)
	}
	// Even the right password is refused while locked; other clients are unaffected.
	if _, err := m.Login("bad", "admin", "s3cret-pass"); !errors.As(err, &locked) {
		t.Fatalf("locked client logged in: %v", err)
	}
	tok, err := m.Login("good", "admin", "s3cret-pass")
	if err != nil {
		t.Fatalf("other client: %v", err)
	}
	clock = clock.Add(31 * time.Second)
	if _, err := m.Login("bad", "admin", "s3cret-pass"); err != nil {
		t.Fatalf("after lockout: %v", err)
	}
	clock = clock.Add(SessionTTL + time.Second)
	if m.Valid(tok) {
		t.Fatal("session outlived its TTL")
	}
}

func TestChangeCredentials(t *testing.T) {
	m, _ := newManager(t)
	tok, err := m.Setup("c", testCode, "admin", "s3cret-pass")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := m.Login("c2", "admin", "s3cret-pass")
	if _, err := m.ChangeCredentials("c", "wrong", "boss", "new-pass-123"); !errors.Is(err, ErrBadLogin) {
		t.Fatalf("wrong current password: %v", err)
	}
	fresh, err := m.ChangeCredentials("c", "s3cret-pass", "boss", "new-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	if m.Valid(tok) || m.Valid(other) || !m.Valid(fresh) {
		t.Fatal("old sessions must end, the new one must work")
	}
	if _, err := m.Login("c", "admin", "s3cret-pass"); err == nil {
		t.Fatal("old credentials still work")
	}
	if _, err := m.Login("c", "boss", "new-pass-123"); err != nil {
		t.Fatal(err)
	}
}
