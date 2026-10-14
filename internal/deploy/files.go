package deploy

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/nikitakiselev/pushbot/internal/manifest"
)

// copyOp copies one extracted file into the project directory.
type copyOp struct {
	from string // absolute path in the extracted bundle
	to   string // slash path relative to the project directory
	mode fs.FileMode
}

// planCopies expands manifest files (which may be directories) into single-file
// copies, checking that every source exists in the bundle.
func planCopies(bundleDir string, files []manifest.File) ([]copyOp, error) {
	var ops []copyOp
	for _, f := range files {
		src := filepath.Join(bundleDir, filepath.FromSlash(f.Src))
		info, err := os.Lstat(src)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("files: %s is not in the bundle", f.Src)
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if f.Dest == "." {
				return nil, fmt.Errorf("files: %s is a file, give it a destination name instead of \".\"", f.Src)
			}
			ops = append(ops, copyOp{from: src, to: f.Dest, mode: info.Mode().Perm()})
			continue
		}
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(src, p)
			if err != nil {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			ops = append(ops, copyOp{from: p, to: path.Join(f.Dest, filepath.ToSlash(rel)), mode: info.Mode().Perm()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	for _, op := range ops {
		if op.to == ".env" {
			return nil, fmt.Errorf("files: .env is written by pushbot from the env input and cannot come from files")
		}
	}
	return ops, nil
}

// copyFile atomically replaces dst with the contents of src.
func copyFile(src, dst string, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".pushbot-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}
