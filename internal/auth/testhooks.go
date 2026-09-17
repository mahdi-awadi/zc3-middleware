package auth

import "testing"

// AllowCodeForTests registers an extra valid activation code and a fixed
// machine identity, so other packages can test activation without real codes.
// It panics outside `go test`.
func AllowCodeForTests(code string) {
	if !testing.Testing() {
		panic("auth: AllowCodeForTests used outside tests")
	}
	n, ok := NormalizeCode(code)
	if !ok {
		panic("auth: malformed test code")
	}
	codeHashes[codeHash(n)] = struct{}{}
	machineID = func() string { return "test-machine" }
}
