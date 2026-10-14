// Package bundle safely extracts the tar.gz archive sent by the action.
package bundle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Limits bound what an archive may expand to.
type Limits struct {
	MaxUnpacked int64 // total bytes of regular files
	MaxFiles    int   // number of entries
}

// Extract unpacks a gzipped tar into dest, which must exist. Only regular files
// and directories are allowed; every path must stay inside dest.
func Extract(r io.Reader, dest string, lim Limits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("bundle: %w", err)
	}
	tr := tar.NewReader(gz)
	var total int64
	count := 0
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("bundle: %w", err)
		}
		if hdr.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		if count++; count > lim.MaxFiles {
			return fmt.Errorf("bundle: more than %d entries", lim.MaxFiles)
		}
		name, err := entryPath(hdr.Name)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(name))

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if hdr.Size > lim.MaxUnpacked-total {
				return fmt.Errorf("bundle: unpacked size exceeds %d bytes", lim.MaxUnpacked)
			}
			n, err := writeFile(target, tr, hdr.Size, hdr.FileInfo().Mode())
			if err != nil {
				return err
			}
			total += n
		default:
			return fmt.Errorf("bundle: %s: only regular files and directories are allowed (symlinks are not supported)", hdr.Name)
		}
	}
}

func entryPath(name string) (string, error) {
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("bundle: %s: absolute path", name)
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("bundle: %s: path escapes the archive", name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return "", fmt.Errorf("bundle: %q: empty path", name)
	}
	return clean, nil
}

func writeFile(target string, r io.Reader, size int64, mode os.FileMode) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return 0, err
	}
	n, err := io.CopyN(f, r, size)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return n, fmt.Errorf("bundle: %s: %w", target, err)
	}
	return n, nil
}
