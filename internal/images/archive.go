package images

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ulikunitz/xz"
)

// archiveKind returns how a downloaded file must be unpacked ("" = not an archive).
func archiveKind(url string) string {
	u := strings.ToLower(strings.SplitN(url, "?", 2)[0])
	switch {
	case strings.HasSuffix(u, ".zip"):
		return "zip"
	case strings.HasSuffix(u, ".tar.xz"), strings.HasSuffix(u, ".txz"):
		return "tar.xz"
	case strings.HasSuffix(u, ".tar.gz"), strings.HasSuffix(u, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(u, ".xz"):
		return "xz"
	case strings.HasSuffix(u, ".gz"):
		return "gz"
	}
	return ""
}

// extract unpacks archive (of the given kind) into dst. For zip and tar
// archives the largest regular file is the disk image. Zero blocks are
// skipped, so dst stays sparse.
func extract(kind, archive, dst string) error {
	if strings.HasPrefix(kind, "tar.") {
		if _, err := exec.LookPath("tar"); err == nil {
			// GNU tar: handles large GNU sparse maps (Go's archive/tar refuses
			// them, e.g. ChromiumOS images) and keeps the output sparse.
			return extractWithTar(kind, archive, dst)
		}
	}
	tmp := dst + ".extract"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	defer out.Close()

	var src io.Reader
	var closer io.Closer
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	switch kind {
	case "zip":
		fi, err := f.Stat()
		if err != nil {
			return err
		}
		zr, err := zip.NewReader(f, fi.Size())
		if err != nil {
			return err
		}
		var best *zip.File
		for _, zf := range zr.File {
			if !zf.FileInfo().IsDir() && (best == nil || zf.UncompressedSize64 > best.UncompressedSize64) {
				best = zf
			}
		}
		if best == nil {
			return errors.New("images: empty zip archive")
		}
		rc, err := best.Open()
		if err != nil {
			return err
		}
		src, closer = rc, rc
	case "xz", "tar.xz":
		xr, err := xz.NewReader(f)
		if err != nil {
			return err
		}
		src = xr
	case "gz", "tar.gz":
		gr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		src, closer = gr, gr
	default:
		return fmt.Errorf("images: unknown archive kind %q", kind)
	}
	if closer != nil {
		defer closer.Close()
	}
	if strings.HasPrefix(kind, "tar.") {
		// Stream through the tar and keep the largest regular file. Archives
		// of disk images normally hold one image, so the first big file wins
		// unless a larger one follows.
		tr := tar.NewReader(src)
		var size int64 = -1
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if h.Typeflag != tar.TypeReg || h.Size <= size {
				continue
			}
			if err := out.Truncate(0); err != nil {
				return err
			}
			if _, err := out.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := copySparse(out, tr, h.Size); err != nil {
				return err
			}
			size = h.Size
		}
		if size < 0 {
			return errors.New("images: no file in tar archive")
		}
	} else if err := copySparse(out, src, -1); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

// copySparse copies src to dst, seeking over all-zero 1 MiB blocks. size is
// the expected length (-1 = unknown); the file is extended to it at the end.
func copySparse(dst *os.File, src io.Reader, size int64) error {
	buf := make([]byte, 1<<20)
	zero := make([]byte, 1<<20)
	var off int64
	for {
		n, err := io.ReadFull(src, buf)
		if n > 0 {
			if bytes.Equal(buf[:n], zero[:n]) {
				if _, err := dst.Seek(int64(n), io.SeekCurrent); err != nil {
					return err
				}
			} else if _, err := dst.Write(buf[:n]); err != nil {
				return err
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if size >= 0 && off != size {
		return fmt.Errorf("images: archive entry truncated (%d of %d bytes)", off, size)
	}
	return dst.Truncate(off)
}

// extractWithTar unpacks a tar archive with the tar binary into a temporary
// directory and moves its largest regular file to dst.
func extractWithTar(kind, archive, dst string) error {
	dir := dst + ".extract.d"
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	flag := "-J"
	if kind == "tar.gz" {
		flag = "-z"
	}
	cmd := exec.Command("tar", "-x", flag, "--no-same-owner", "-f", archive, "-C", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var best string
	var size int64 = -1
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		if fi, err := d.Info(); err == nil && fi.Size() > size {
			best, size = p, fi.Size()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if best == "" {
		return errors.New("images: no file in tar archive")
	}
	return os.Rename(best, dst)
}
