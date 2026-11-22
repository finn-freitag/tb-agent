// Package adbkey implements adb's RSA authentication: the public key format
// used in adb_keys / adbkey.pub and signing of AUTH tokens.
package adbkey

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
)

// Key is an adb RSA key pair.
type Key struct {
	priv *rsa.PrivateKey
}

// LoadOrCreate loads an RSA private key (PEM) from path or creates a new one.
func LoadOrCreate(path string) (*Key, error) {
	if b, err := os.ReadFile(path); err == nil {
		blk, _ := pem.Decode(b)
		if blk == nil {
			return nil, errors.New("adbkey: invalid PEM in " + path)
		}
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, fmt.Errorf("adbkey: %w", err)
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("adbkey: not an RSA key")
		}
		return &Key{priv: rk}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(rk)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return &Key{priv: rk}, nil
}

// Sign answers an AUTH TOKEN challenge. adbd verifies the 20-byte token as if
// it were a SHA-1 digest (RSA_verify(NID_sha1, ...)).
func (k *Key) Sign(token []byte) ([]byte, error) {
	return rsa.SignPKCS1v15(nil, k.priv, crypto.SHA1, token)
}

// Public returns the RSA public key.
func (k *Key) Public() *rsa.PublicKey { return &k.priv.PublicKey }

// PublicKey returns the key in adb_keys format: "<base64> <comment>".
func (k *Key) PublicKey(comment string) string {
	return Encode(&k.priv.PublicKey) + " " + comment
}

// Encode serializes an RSA public key in Android's RSAPublicKey layout:
// modulus size in 32-bit words, n0inv = -1/n mod 2^32, modulus, rr = R^2 mod n
// (R = 2^bits), exponent; all little endian.
func Encode(pub *rsa.PublicKey) string {
	words := (pub.N.BitLen() + 31) / 32
	buf := make([]byte, 0, 12+8*words)
	put := func(v uint32) { buf = binary.LittleEndian.AppendUint32(buf, v) }
	putBig := func(x *big.Int) {
		b := x.FillBytes(make([]byte, words*4)) // big endian
		for i := words - 1; i >= 0; i-- {
			put(binary.BigEndian.Uint32(b[i*4:]))
		}
	}
	r32 := new(big.Int).Lsh(big.NewInt(1), 32)
	n0 := new(big.Int).Mod(pub.N, r32)
	n0inv := new(big.Int).ModInverse(n0, r32)
	n0inv.Sub(r32, n0inv)
	rr := new(big.Int).Lsh(big.NewInt(1), uint(words*32*2))
	rr.Mod(rr, pub.N)

	put(uint32(words))
	put(uint32(n0inv.Uint64()))
	putBig(pub.N)
	putBig(rr)
	put(uint32(pub.E))
	return base64.StdEncoding.EncodeToString(buf)
}

// Parse validates one adb public key line ("<base64> [comment]") and returns
// it normalized. Only the key layout written by adb (RSA, 2048-4096 bits) is
// accepted.
func Parse(line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" || strings.ContainsAny(line, "\r\n\x00") {
		return "", errors.New("adbkey: empty or multi-line key")
	}
	b64, comment, _ := strings.Cut(line, " ")
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) < 12 {
		return "", errors.New("adbkey: not an adb public key (expected the content of ~/.android/adbkey.pub)")
	}
	words := int(binary.LittleEndian.Uint32(raw))
	if words < 64 || words > 128 || len(raw) != 12+8*words {
		return "", errors.New("adbkey: unsupported adb key size")
	}
	comment = strings.TrimSpace(comment)
	if comment == "" {
		return b64, nil
	}
	return b64 + " " + comment, nil
}
