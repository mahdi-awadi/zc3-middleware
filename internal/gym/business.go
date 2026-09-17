package gym

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Business is the gym's identity printed on receipts and statements.
type Business struct {
	Name    string `json:"name"`
	Phone   string `json:"phone"`
	Address string `json:"address"` // location printed under the gym name
	Note    string `json:"note"`    // subscription terms printed under every receipt
}

const (
	maxBusinessName  = 80
	maxBusinessPhone = 40
	maxBusinessAddr  = 160
	maxBusinessNote  = 2000
	logoFile         = "gym-logo.img"
)

// Business returns the printable gym details and whether a logo is set.
func (s *Store) Business() (Business, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, hasLogo := s.logoPath()
	return s.d.Config.Business, hasLogo
}

// SetBusiness replaces the printable gym details.
func (s *Store) SetBusiness(b Business) error {
	b.Name = strings.TrimSpace(b.Name)
	b.Phone = strings.TrimSpace(b.Phone)
	b.Address = strings.TrimSpace(b.Address)
	b.Note = strings.TrimSpace(strings.ReplaceAll(b.Note, "\r\n", "\n"))
	switch {
	case utf8.RuneCountInString(b.Name) > maxBusinessName:
		return fmt.Errorf("gym: gym name is limited to %d characters", maxBusinessName)
	case utf8.RuneCountInString(b.Phone) > maxBusinessPhone:
		return fmt.Errorf("gym: phone is limited to %d characters", maxBusinessPhone)
	case utf8.RuneCountInString(b.Address) > maxBusinessAddr:
		return fmt.Errorf("gym: address is limited to %d characters", maxBusinessAddr)
	case utf8.RuneCountInString(b.Note) > maxBusinessNote:
		return fmt.Errorf("gym: receipt note is limited to %d characters", maxBusinessNote)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Config.Business = b
	return s.save()
}

// logoPath is the gym logo file beside the data file (caller holds a lock).
func (s *Store) logoPath() (string, bool) {
	if s.path == "" {
		return "", false
	}
	p := filepath.Join(filepath.Dir(s.path), logoFile)
	if _, err := os.Stat(p); err != nil {
		return p, false
	}
	return p, true
}

// SaveLogo stores the gym logo (raw image bytes).
func (s *Store) SaveLogo(data []byte) error {
	if len(data) == 0 || len(data) > maxPhotoBytes {
		return fmt.Errorf("gym: logo must be 1 byte .. %d MB", maxPhotoBytes>>20)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	p, _ := s.logoPath()
	if p == "" {
		return errors.New("gym: logo storage unavailable (in-memory store)")
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Logo returns the gym logo bytes.
func (s *Store) Logo() ([]byte, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.logoPath()
	if !ok {
		return nil, ErrNotFound
	}
	return os.ReadFile(p)
}

// DeleteLogo removes the gym logo (receipts fall back to the app logo).
func (s *Store) DeleteLogo() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.logoPath()
	if !ok {
		return ErrNotFound
	}
	return os.Remove(p)
}
