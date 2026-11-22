package adbkey

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"math/big"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "adbkey")
	k, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := LoadOrCreate(path)
	if err != nil || k2.priv.N.Cmp(k.priv.N) != 0 {
		t.Fatal("key not persisted", err)
	}
	pub := k.PublicKey("tossbox-agent")
	norm, err := Parse(pub)
	if err != nil || norm != pub {
		t.Fatalf("parse: %v %q", err, norm)
	}
	// Decode the Android layout back and compare with the key.
	raw, _ := base64.StdEncoding.DecodeString(strings.Fields(pub)[0])
	words := int(binary.LittleEndian.Uint32(raw))
	if words != 64 {
		t.Fatalf("words %d", words)
	}
	n := new(big.Int)
	for i := words - 1; i >= 0; i-- {
		n.Lsh(n, 32)
		n.Or(n, big.NewInt(int64(binary.LittleEndian.Uint32(raw[8+i*4:]))))
	}
	if n.Cmp(k.priv.N) != 0 {
		t.Fatal("modulus mismatch")
	}
	n0inv := binary.LittleEndian.Uint32(raw[4:])
	if uint32(k.priv.N.Uint64())*n0inv != 0xffffffff { // n0 * n0inv == -1 mod 2^32
		t.Fatal("bad n0inv")
	}
	if e := binary.LittleEndian.Uint32(raw[len(raw)-4:]); e != 65537 {
		t.Fatalf("exponent %d", e)
	}
	// The signature verifies as PKCS#1 v1.5 over a "SHA-1 digest" token.
	token := sha1.Sum([]byte("challenge"))
	sig, err := k.Sign(token[:])
	if err != nil {
		t.Fatal(err)
	}
	if err := rsa.VerifyPKCS1v15(&k.priv.PublicKey, crypto.SHA1, token[:], sig); err != nil {
		t.Fatal(err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, s := range []string{"", "ssh-ed25519 AAAA", "QUFBQQ== x", "abc\ndef"} {
		if _, err := Parse(s); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
