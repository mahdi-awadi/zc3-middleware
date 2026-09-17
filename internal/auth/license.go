package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Activation codes look like AMAD-XXXXX-XXXXX-XXXXX-XXXXX: 20 Crockford base32
// characters (100 random bits). Only SHA-256 fingerprints ship in the binary
// (codes.go), so the app cannot be mined for valid codes.

const codeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var codeDomain = []byte("amad-gym/activation/v1\x00")

// ErrInvalidCode is returned for a code that is malformed or not one of ours.
var ErrInvalidCode = errors.New("auth: invalid activation code")

// NormalizeCode uppercases, drops the AMAD prefix, spaces and dashes, and maps
// look-alike characters (O→0, I/L→1) the way Crockford base32 does.
func NormalizeCode(code string) (string, bool) {
	c := strings.ToUpper(strings.TrimSpace(code))
	c = strings.TrimPrefix(c, "AMAD")
	var b strings.Builder
	for _, r := range c {
		switch {
		case r == '-' || r == ' ' || r == '\t':
			continue
		case r == 'O':
			r = '0'
		case r == 'I' || r == 'L':
			r = '1'
		}
		if !strings.ContainsRune(codeAlphabet, r) {
			return "", false
		}
		b.WriteRune(r)
	}
	if b.Len() != 20 {
		return "", false
	}
	return b.String(), true
}

func codeHash(normalized string) string {
	sum := sha256.Sum256(append(append([]byte{}, codeDomain...), normalized...))
	return hex.EncodeToString(sum[:])
}

// ValidCode reports whether code is one of the issued activation codes.
func ValidCode(code string) bool {
	n, ok := NormalizeCode(code)
	if !ok {
		return false
	}
	_, found := codeHashes[codeHash(n)]
	return found
}

// MaskCode shows a code with its middle hidden: AMAD-09HG5-•••••-•••••-3XJ5H.
func MaskCode(normalized string) string {
	if len(normalized) != 20 {
		return ""
	}
	return "AMAD-" + normalized[:5] + "-•••••-•••••-" + normalized[15:]
}

// license is the activation record. It is stored encrypted with a key derived
// from this machine's identity, so a copied file does not activate another PC.
type license struct {
	Code        string    `json:"code"` // normalized
	Machine     string    `json:"machine"`
	ActivatedAt time.Time `json:"activated_at"`
}

// machineID identifies this installation's computer. Replaceable in tests.
var machineID = func() string {
	var id string
	switch runtime.GOOS {
	case "windows":
		out, err := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
		if err == nil {
			f := strings.Fields(string(out))
			if len(f) > 0 {
				id = f[len(f)-1]
			}
		}
	case "linux":
		if b, err := os.ReadFile("/etc/machine-id"); err == nil {
			id = strings.TrimSpace(string(b))
		}
	case "darwin":
		if out, err := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice").Output(); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.Contains(line, "IOPlatformUUID") {
					if i := strings.LastIndex(line, "\""); i > 0 {
						if j := strings.LastIndex(line[:i], "\""); j >= 0 {
							id = line[j+1 : i]
						}
					}
				}
			}
		}
	}
	if id == "" {
		id, _ = os.Hostname()
	}
	sum := sha256.Sum256([]byte("amad-gym/machine/v1\x00" + id))
	return hex.EncodeToString(sum[:])
}

func licenseKey(machine string) []byte {
	sum := sha256.Sum256([]byte("amad-gym/license-key/v1\x00" + machine))
	return sum[:]
}

func sealLicense(l license) ([]byte, error) {
	plain, err := json.Marshal(l)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(licenseKey(l.Machine))
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte("amad-gym/license/v1")), nil
}

// openLicense decrypts and verifies a license file for this machine.
func openLicense(data []byte, machine string) (license, error) {
	var l license
	block, err := aes.NewCipher(licenseKey(machine))
	if err != nil {
		return l, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return l, err
	}
	if len(data) < gcm.NonceSize() {
		return l, errors.New("auth: license file is corrupt")
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte("amad-gym/license/v1"))
	if err != nil {
		return l, errors.New("auth: license belongs to another computer or was modified")
	}
	if err := json.Unmarshal(plain, &l); err != nil {
		return l, fmt.Errorf("auth: license file is corrupt: %w", err)
	}
	if l.Machine != machine {
		return l, errors.New("auth: license belongs to another computer")
	}
	if _, ok := codeHashes[codeHash(l.Code)]; !ok {
		return l, ErrInvalidCode
	}
	return l, nil
}
