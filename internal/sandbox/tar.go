package sandbox

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Limits on the tree that crosses the boundary.
const (
	// MaxTreeBytes bounds the extracted tree. A repository larger than this
	// is not one implement mode should be writing to unattended, and the
	// limit is what stops a crafted archive from filling the runner's disk.
	MaxTreeBytes = 512 << 20
	// MaxTreeFiles bounds the file count for the same reason.
	MaxTreeFiles = 200_000
)

// Pack writes dir as a gzipped tar.
//
// Only regular files and directories go in. A symlink is left out rather than
// followed or recreated: the tree is extracted somewhere else, and a link is
// either meaningless there or a way out of the directory it was extracted
// into.
func Pack(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		// Not the agent's scratch space, and not git's object store: the
		// runner builds a tree, it does not need its history.
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return nil
		case info.IsDir():
			return tw.WriteHeader(&tar.Header{
				Name:     rel + "/",
				Mode:     0o755,
				Typeflag: tar.TypeDir,
			})
		case !info.Mode().IsRegular():
			// Sockets, devices and pipes are not source code.
			return nil
		}

		header := &tar.Header{
			Name:     rel,
			Mode:     int64(info.Mode().Perm()),
			Size:     info.Size(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		f, err := os.Open(path) //nolint:gosec // a path from walking the workspace
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		_, err = io.Copy(tw, f)
		return err
	})
	if walkErr != nil {
		return fmt.Errorf("sandbox: packing %s: %w", dir, walkErr)
	}

	if err := tw.Close(); err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	return gz.Close()
}

// Unpack extracts an archive into dir.
//
// Every entry's path is checked against dir. An archive is input from another
// process, and the classic way to abuse one is a name like "../../etc" or an
// absolute path — so a name that does not stay inside is an error rather than
// something to sanitize and continue with.
func Unpack(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("sandbox: the archive is not gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	root, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}

	tr := tar.NewReader(gz)
	var total int64
	var files int

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("sandbox: reading the archive: %w", err)
		}

		target, err := safeJoin(root, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("sandbox: %w", err)
			}
		case tar.TypeReg:
			files++
			if files > MaxTreeFiles {
				return fmt.Errorf("sandbox: the archive holds more than %d files", MaxTreeFiles)
			}
			total += header.Size
			if total > MaxTreeBytes {
				return fmt.Errorf("sandbox: the archive extracts to more than %d bytes", MaxTreeBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("sandbox: %w", err)
			}
			if err := writeFile(target, tr, header); err != nil {
				return err
			}
		default:
			// Links, devices and everything else are skipped rather than
			// recreated. Pack does not produce them, so an archive that has
			// one did not come from Pack.
			continue
		}
	}
}

func writeFile(target string, r io.Reader, header *tar.Header) error {
	mode := os.FileMode(header.Mode).Perm() //nolint:gosec // masked to permission bits
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode) //nolint:gosec // checked by safeJoin
	if err != nil {
		return fmt.Errorf("sandbox: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Bounded by the header rather than by the reader: a stream that claims a
	// small size and keeps going is the other half of the same trick.
	if _, err := io.CopyN(f, r, header.Size); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("sandbox: writing %s: %w", header.Name, err)
	}
	return nil
}

// safeJoin resolves an archive entry's name under root, or refuses it.
func safeJoin(root, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("sandbox: the archive holds an entry with no name")
	}
	if filepath.IsAbs(name) || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("sandbox: %q is an absolute path", name)
	}

	target := filepath.Join(root, filepath.Clean("/"+name))
	// Clean("/"+name) cannot climb above "/", so this holds by construction;
	// it is checked anyway, because the cost of being wrong here is writing
	// outside the directory.
	if target != root && !strings.HasPrefix(target, root+string(filepath.Separator)) {
		return "", fmt.Errorf("sandbox: %q does not stay inside the extraction directory", name)
	}
	return target, nil
}
