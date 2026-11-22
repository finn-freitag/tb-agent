package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

func TestDownloadResumeAndChecksum(t *testing.T) {
	body := strings.Repeat("tossbox-image-", 10000)
	sum := sha256.Sum256([]byte(body))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "img.qcow2", testTime, strings.NewReader(body))
	}))
	defer srv.Close()

	s, err := New(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	url := srv.URL + "/img.qcow2"
	// Simulate an interrupted download.
	if err := os.WriteFile(s.CachePath(url)+".part", []byte(body[:1234]), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: url, Sha256: hex.EncodeToString(sum[:])}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != body {
		t.Fatal("content mismatch")
	}
	if _, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: srv.URL + "/other.qcow2", Sha256: "00"}, nil); err == nil {
		t.Fatal("expected checksum error")
	}
}

func TestLocal(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir, slog.Default())
	os.WriteFile(filepath.Join(dir, "win11.qcow2"), []byte("x"), 0o644)
	if _, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: "local:win11.qcow2"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: "local:../etc/passwd"}, nil); err == nil {
		t.Fatal("expected traversal error")
	}
	if _, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: "local:missing.qcow2"}, nil); err == nil {
		t.Fatal("expected not found")
	}
}

var testTime = time.Unix(1700000000, 0)
