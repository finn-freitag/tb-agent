package adbkey

import (
	"os"
	"strings"
	"testing"
)

// TestAdbCompat compares Encode with a key pair made by `adb keygen`
// (set ADBKEY_COMPAT to the private key path; adbkey.pub next to it).
func TestAdbCompat(t *testing.T) {
	p := os.Getenv("ADBKEY_COMPAT")
	if p == "" {
		t.Skip("ADBKEY_COMPAT not set")
	}
	k, err := LoadOrCreate(p)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(p + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(k.PublicKey("x"))[0]; got != strings.Fields(string(want))[0] {
		t.Fatalf("encoding differs from adb:\n got %.60s\nwant %.60s", got, want)
	}
}
