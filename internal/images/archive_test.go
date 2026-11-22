package images

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
	"github.com/ulikunitz/xz"
)

// disk is 3 MiB with data at the start and end and zeros between.
func disk() []byte {
	b := make([]byte, 3<<20)
	copy(b, "BOOT")
	copy(b[len(b)-4:], "END!")
	return b
}

func TestExtract(t *testing.T) {
	dir := t.TempDir()
	img := disk()

	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	w, _ := zw.Create("readme.txt")
	w.Write([]byte("hi"))
	w, _ = zw.Create("chromeos.bin")
	w.Write(img)
	zw.Close()

	var tb bytes.Buffer
	xw, _ := xz.NewWriter(&tb)
	tw := tar.NewWriter(xw)
	tw.WriteHeader(&tar.Header{Name: "small", Mode: 0o644, Size: 2, Typeflag: tar.TypeReg})
	tw.Write([]byte("hi"))
	tw.WriteHeader(&tar.Header{Name: "chromiumos_base_image.bin", Mode: 0o644, Size: int64(len(img)), Typeflag: tar.TypeReg})
	tw.Write(img)
	tw.Close()
	xw.Close()

	files := map[string][]byte{"/a.bin.zip": zb.Bytes(), "/b.tar.xz": tb.Bytes()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "x", time.Time{}, bytes.NewReader(files[r.URL.Path]))
	}))
	defer srv.Close()
	s, _ := New(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	for p := range files {
		got, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: srv.URL + p, Format: "raw"}, nil)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		b, _ := os.ReadFile(got)
		if !bytes.Equal(b, img) {
			t.Fatalf("%s: unpacked image differs (%d bytes)", p, len(b))
		}
		if _, err := os.Stat(s.CachePath(srv.URL + p)); !os.IsNotExist(err) {
			t.Errorf("%s: archive not removed", p)
		}
		// Second fetch: served from the unpacked file.
		if again, err := s.Fetch(context.Background(), &agentv1.ImageSpec{Url: srv.URL + p}, nil); err != nil || again != got {
			t.Errorf("%s: refetch %s %v", p, again, err)
		}
	}
	if k := archiveKind("https://x/y.qcow2"); k != "" {
		t.Error(k)
	}
}
