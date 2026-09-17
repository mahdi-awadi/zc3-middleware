// Package auth guards the console of a single-user installation:
//
//   - activation: one issued code, checked against embedded SHA-256
//     fingerprints and stored encrypted, bound to this computer;
//   - one administrator account: username + PBKDF2-SHA256 password hash;
//   - server-side sessions carried in an HttpOnly, SameSite=Strict cookie
//     (only a hash of each session token is kept in memory);
//   - login throttling: repeated failures lock the client out, doubling
//     from 30 seconds up to 15 minutes.
package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	// SessionCookie is the name of the console session cookie.
	SessionCookie = "amad_session"
	// SessionTTL is how long a login lasts.
	SessionTTL = 12 * time.Hour

	pbkdf2Iter  = 600_000
	minPassword = 8
	freeFails   = 5 // failures allowed before a lockout starts
	maxLockout  = 15 * time.Minute

	licenseFile = "license.dat"
	userFile    = "admin.json"
)

var (
	ErrNotActivated  = errors.New("auth: this copy is not activated")
	ErrAlreadySetUp  = errors.New("auth: already set up")
	ErrBadLogin      = errors.New("auth: wrong username or password")
	ErrBadUsername   = errors.New("auth: username must be 3–32 characters")
	ErrWeakPassword  = fmt.Errorf("auth: password must be at least %d characters and differ from the username", minPassword)
	ErrNotConfigured = errors.New("auth: no account yet — finish setup first")
)

// LockedError is returned while a client is locked out after failed attempts.
type LockedError struct{ Retry time.Duration }

func (e LockedError) Error() string {
	return fmt.Sprintf("auth: too many failed attempts, retry in %s", e.Retry.Round(time.Second))
}

type account struct {
	Username  string    `json:"username"`
	Salt      string    `json:"salt"` // hex
	Hash      string    `json:"hash"` // hex, PBKDF2-SHA256
	Iter      int       `json:"iter"`
	UpdatedAt time.Time `json:"updated_at"`
}

type failState struct {
	count int
	until time.Time
}

// Manager holds activation, the account and live sessions.
type Manager struct {
	dir string
	now func() time.Time

	mu       sync.Mutex
	lic      *license
	acct     *account
	sessions map[string]time.Time // sha256(token) → expiry
	fails    map[string]*failState
}

// Open loads the activation and account files from dir (created on setup).
// A license that does not belong to this computer is ignored: the copy then
// asks for activation again.
func Open(dir string) (*Manager, error) {
	if dir == "" {
		dir = "."
	}
	m := &Manager{dir: dir, now: time.Now, sessions: map[string]time.Time{}, fails: map[string]*failState{}}
	if b, err := os.ReadFile(filepath.Join(dir, licenseFile)); err == nil {
		if l, err := openLicense(b, machineID()); err == nil {
			m.lic = &l
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if b, err := os.ReadFile(filepath.Join(dir, userFile)); err == nil {
		var a account
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, fmt.Errorf("auth: parse %s: %w", userFile, err)
		}
		if a.Username != "" && a.Hash != "" {
			m.acct = &a
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return m, nil
}

// Status is what the console needs to pick its first screen.
type Status struct {
	Activated     bool       `json:"activated"`
	Configured    bool       `json:"configured"`
	Authenticated bool       `json:"authenticated"`
	Username      string     `json:"username,omitempty"`
	Code          string     `json:"code,omitempty"` // masked
	ActivatedAt   *time.Time `json:"activated_at,omitempty"`
}

func (m *Manager) Status(token string) Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Activated: m.lic != nil, Configured: m.acct != nil}
	st.Authenticated = st.Activated && st.Configured && m.validLocked(token)
	if st.Authenticated {
		st.Username = m.acct.Username
		st.Code = MaskCode(m.lic.Code)
		t := m.lic.ActivatedAt
		st.ActivatedAt = &t
	}
	return st
}

// Activated reports whether this copy has a valid activation.
func (m *Manager) Activated() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lic != nil
}

// Setup activates the copy (if needed) and creates the only account, then
// logs in. The activation code is required unless this computer is already
// activated. Once an account exists, setup is closed.
func (m *Manager) Setup(client, code, username, password string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.lockedOutLocked(client); err != nil {
		return "", err
	}
	if m.acct != nil {
		return "", ErrAlreadySetUp
	}
	username, err := checkCredentials(username, password)
	if err != nil {
		return "", err
	}
	if m.lic == nil {
		n, ok := NormalizeCode(code)
		if !ok {
			m.failLocked(client)
			return "", ErrInvalidCode
		}
		if _, ok := codeHashes[codeHash(n)]; !ok {
			m.failLocked(client)
			return "", ErrInvalidCode
		}
		l := license{Code: n, Machine: machineID(), ActivatedAt: m.now().UTC()}
		sealed, err := sealLicense(l)
		if err != nil {
			return "", err
		}
		if err := writeFileAtomic(filepath.Join(m.dir, licenseFile), sealed); err != nil {
			return "", err
		}
		m.lic = &l
	}
	a, err := newAccount(username, password, m.now())
	if err != nil {
		return "", err
	}
	if err := m.saveAccountLocked(a); err != nil {
		return "", err
	}
	delete(m.fails, client)
	return m.newSessionLocked()
}

// Login checks the account and opens a session.
func (m *Manager) Login(client, username, password string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lic == nil {
		return "", ErrNotActivated
	}
	if m.acct == nil {
		return "", ErrNotConfigured
	}
	if err := m.lockedOutLocked(client); err != nil {
		return "", err
	}
	// Always derive the hash, so a wrong username costs the same time as a wrong password.
	okPass := m.acct.verify(password)
	okUser := subtle.ConstantTimeCompare([]byte(strings.TrimSpace(username)), []byte(m.acct.Username)) == 1
	if !okPass || !okUser {
		m.failLocked(client)
		if err := m.lockedOutLocked(client); err != nil {
			return "", err
		}
		return "", ErrBadLogin
	}
	delete(m.fails, client)
	return m.newSessionLocked()
}

// Logout ends one session.
func (m *Manager) Logout(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, tokenHash(token))
}

// Valid reports whether token is a live session of an activated copy.
func (m *Manager) Valid(token string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lic != nil && m.acct != nil && m.validLocked(token)
}

// ChangeCredentials replaces the username and/or password after checking the
// current password. Every session ends; the caller gets a fresh one.
func (m *Manager) ChangeCredentials(client, current, username, password string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.acct == nil {
		return "", ErrNotConfigured
	}
	if err := m.lockedOutLocked(client); err != nil {
		return "", err
	}
	if !m.acct.verify(current) {
		m.failLocked(client)
		return "", ErrBadLogin
	}
	if strings.TrimSpace(username) == "" {
		username = m.acct.Username
	}
	if password == "" {
		password = current
	}
	username, err := checkCredentials(username, password)
	if err != nil {
		return "", err
	}
	a, err := newAccount(username, password, m.now())
	if err != nil {
		return "", err
	}
	if err := m.saveAccountLocked(a); err != nil {
		return "", err
	}
	delete(m.fails, client)
	m.sessions = map[string]time.Time{}
	return m.newSessionLocked()
}

// ---- internals (caller holds m.mu) ----

func (m *Manager) validLocked(token string) bool {
	if token == "" {
		return false
	}
	h := tokenHash(token)
	exp, ok := m.sessions[h]
	if !ok {
		return false
	}
	if !m.now().Before(exp) {
		delete(m.sessions, h)
		return false
	}
	return true
}

func (m *Manager) newSessionLocked() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := m.now()
	for h, exp := range m.sessions { // drop expired sessions
		if !now.Before(exp) {
			delete(m.sessions, h)
		}
	}
	m.sessions[tokenHash(token)] = now.Add(SessionTTL)
	return token, nil
}

func (m *Manager) lockedOutLocked(client string) error {
	f := m.fails[client]
	if f == nil {
		return nil
	}
	if wait := f.until.Sub(m.now()); wait > 0 {
		return LockedError{Retry: wait}
	}
	return nil
}

func (m *Manager) failLocked(client string) {
	f := m.fails[client]
	if f == nil {
		f = &failState{}
		m.fails[client] = f
	}
	f.count++
	if f.count >= freeFails {
		lock := 30 * time.Second << min(f.count-freeFails, 10)
		if lock > maxLockout {
			lock = maxLockout
		}
		f.until = m.now().Add(lock)
	}
}

func (m *Manager) saveAccountLocked(a account) error {
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(m.dir, userFile), b); err != nil {
		return err
	}
	m.acct = &a
	return nil
}

func checkCredentials(username, password string) (string, error) {
	username = strings.TrimSpace(username)
	if n := utf8.RuneCountInString(username); n < 3 || n > 32 {
		return "", ErrBadUsername
	}
	if utf8.RuneCountInString(password) < minPassword || password == username {
		return "", ErrWeakPassword
	}
	return username, nil
}

func newAccount(username, password string, now time.Time) (account, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return account{}, err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iter, 32)
	if err != nil {
		return account{}, err
	}
	return account{Username: username, Salt: hex.EncodeToString(salt), Hash: hex.EncodeToString(key), Iter: pbkdf2Iter, UpdatedAt: now.UTC()}, nil
}

func (a *account) verify(password string) bool {
	salt, err1 := hex.DecodeString(a.Salt)
	want, err2 := hex.DecodeString(a.Hash)
	iter := a.Iter
	if iter <= 0 {
		iter = pbkdf2Iter
	}
	got, err3 := pbkdf2.Key(sha256.New, password, salt, iter, 32)
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic writes owner-only files via a temp file + rename.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
