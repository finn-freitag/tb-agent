// Package images downloads and caches base images (implements driver.Images).
//
// Layout below the agent data dir:
//
//	images/<file>                 local: images (e.g. licensed Windows golden images)
//	images/cache/<hash>.<ext>     downloaded https:// images
//	images/cache/<hash>.<ext>.part  in-progress download (resumed with Range requests)
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agentv1 "github.com/finn-freitag/tb-proto/gen/agent/v1"
)

// Store implements driver.Images.
type Store struct {
	dir    string
	client *http.Client
	log    *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// New creates a store rooted at dir (usually <data_dir>/images).
func New(dir string, log *slog.Logger) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		return nil, err
	}
	return &Store{
		dir:    dir,
		client: &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: 60 * time.Second}},
		log:    log,
		locks:  map[string]*sync.Mutex{},
	}, nil
}

// Dir returns the root directory (local: images live here).
func (s *Store) Dir() string { return s.dir }

// Local lists the images available for local:<name> URLs (top level of the
// image directory, e.g. Packer golden images).
func (s *Store) Local() []string {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := e.Name()
		if e.Type().IsRegular() && !strings.HasSuffix(n, ".part") && !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}
	return out
}

func (s *Store) lock(key string) func() {
	s.mu.Lock()
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// Fetch makes the image available locally and returns its path.
func (s *Store) Fetch(ctx context.Context, img *agentv1.ImageSpec, progress func(done, total int64)) (string, error) {
	if img == nil || img.Url == "" {
		return "", errors.New("images: no image url")
	}
	u := img.Url
	switch {
	case strings.HasPrefix(u, "local:"):
		return s.local(strings.TrimPrefix(u, "local:"), img.Sha256)
	case strings.HasPrefix(u, "https://"), strings.HasPrefix(u, "http://"):
		kind := archiveKind(u)
		if kind == "" {
			return s.download(ctx, u, img, progress)
		}
		// Archives (e.g. ChromeOS .bin.zip, .tar.xz) are unpacked once; the
		// archive is removed afterwards to save space. sha256 refers to the archive.
		unpacked := s.CachePath(u) + ".img"
		if _, err := os.Stat(unpacked); err == nil {
			return unpacked, nil
		}
		arch, err := s.download(ctx, u, img, progress)
		if err != nil {
			return "", err
		}
		defer s.lock(unpacked)()
		if _, err := os.Stat(unpacked); err == nil {
			return unpacked, nil
		}
		s.log.Info("unpacking image", "url", u, "kind", kind)
		if err := extract(kind, arch, unpacked); err != nil {
			return "", fmt.Errorf("images: unpack %s: %w", u, err)
		}
		_ = os.Remove(arch)
		return unpacked, nil
	default:
		return "", fmt.Errorf("images: unsupported image url %q (drivers handle docker:// oci:// incus: themselves)", u)
	}
}

func (s *Store) local(name, sum string) (string, error) {
	clean := filepath.Clean("/" + name)[1:]
	if clean == "" || strings.Contains(name, "..") {
		return "", fmt.Errorf("images: invalid local image name %q", name)
	}
	p := filepath.Join(s.dir, clean)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("images: local image %q not found in %s (build it with Packer, see tb-templates/packer)", name, s.dir)
	}
	if sum != "" {
		if err := verify(p, sum); err != nil {
			return "", err
		}
	}
	return p, nil
}

// CachePath returns where a downloaded URL is cached.
func (s *Store) CachePath(url string) string {
	h := sha256.Sum256([]byte(url))
	ext := path.Ext(strings.SplitN(url, "?", 2)[0])
	if len(ext) > 8 {
		ext = ""
	}
	return filepath.Join(s.dir, "cache", hex.EncodeToString(h[:12])+ext)
}

func (s *Store) download(ctx context.Context, url string, img *agentv1.ImageSpec, progress func(done, total int64)) (string, error) {
	dst := s.CachePath(url)
	defer s.lock(dst)()
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	}
	part := dst + ".part"
	for attempt := 0; ; attempt++ {
		err := s.downloadOnce(ctx, url, part, progress)
		if err == nil {
			break
		}
		if ctx.Err() != nil || attempt >= 4 {
			return "", fmt.Errorf("images: download %s: %w", url, err)
		}
		s.log.Warn("image download interrupted, resuming", "url", url, "err", err)
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 2 * time.Second):
		}
	}
	if img.Sha256 != "" {
		if err := verify(part, img.Sha256); err != nil {
			os.Remove(part)
			return "", err
		}
	}
	if err := os.Rename(part, dst); err != nil {
		return "", err
	}
	return dst, nil
}

func (s *Store) downloadOnce(ctx context.Context, url, part string, progress func(done, total int64)) error {
	var offset int64
	if fi, err := os.Stat(part); err == nil {
		offset = fi.Size()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "tb-agent")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	flags := os.O_CREATE | os.O_WRONLY
	switch resp.StatusCode {
	case http.StatusOK:
		offset = 0
		flags |= os.O_TRUNC
	case http.StatusPartialContent:
		flags |= os.O_APPEND
	case http.StatusRequestedRangeNotSatisfiable:
		// Probably already complete.
		return nil
	default:
		return fmt.Errorf("HTTP %s", resp.Status)
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = offset + resp.ContentLength
	}
	pw := &progressWriter{done: offset, total: total, fn: progress}
	if _, err := io.Copy(io.MultiWriter(f, pw), resp.Body); err != nil {
		return err
	}
	return f.Sync()
}

type progressWriter struct {
	done, total int64
	last        time.Time
	fn          func(done, total int64)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.fn != nil && time.Since(p.last) > time.Second {
		p.last = time.Now()
		p.fn(p.done, p.total)
	}
	return len(b), nil
}

func verify(p, want string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.EqualFold(got, strings.TrimPrefix(want, "sha256:")) {
		return fmt.Errorf("images: checksum mismatch for %s: got %s want %s", filepath.Base(p), got, want)
	}
	return nil
}
