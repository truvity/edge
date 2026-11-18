package staticsite

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
)

// limits caps what one artifact may write, across all its layers.
type limits struct {
	maxBytes int64
	maxFiles int
	bytes    int64
	files    int
}

func (l *limits) file() error {
	l.files++
	if l.files > l.maxFiles {
		return fmt.Errorf("artifact has more than %d files", l.maxFiles)
	}
	return nil
}

// relPath cleans an archive or annotation name into a path that stays inside
// the target, or refuses it.
func relPath(name string) (string, error) {
	if name == "" || strings.ContainsRune(name, 0) || strings.Contains(name, `\`) {
		return "", fmt.Errorf("unsafe path %q", name)
	}
	if strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("absolute path %q", name)
	}
	p := path.Clean(name)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") {
		return "", fmt.Errorf("path %q leaves the target", name)
	}
	return p, nil
}

// extractTar unpacks a tar stream, gzip or not, into dest. Only regular files
// and directories are written; a link, device or fifo is refused outright.
func extractTar(r io.Reader, dest string, lim *limits) error {
	br := bufio.NewReader(r)
	if magic, err := br.Peek(2); err == nil && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, err := gzip.NewReader(br)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	} else {
		r = br
	}
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "" || name == "." {
			continue
		}
		p, err := relPath(name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			// A directory counts toward the file limit: an archive of nothing
			// but directory entries is as much work to unpack as one of files.
			if err := lim.file(); err != nil {
				return err
			}
			if err := root.MkdirAll(p, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := lim.file(); err != nil {
				return err
			}
			if err := writeFile(root, p, tr, lim); err != nil {
				return err
			}
		case tar.TypeXGlobalHeader:
		default:
			return fmt.Errorf("%q: entry type %q is not allowed (regular files and directories only)", hdr.Name, string(hdr.Typeflag))
		}
	}
}

func writeFile(root *os.Root, p string, r io.Reader, lim *limits) error {
	if dir := path.Dir(p); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	f, err := root.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, lim.maxBytes-lim.bytes+1))
	lim.bytes += n
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if lim.bytes > lim.maxBytes {
		return fmt.Errorf("artifact is larger than %d bytes", lim.maxBytes)
	}
	return nil
}

// writeLayerFile writes one single-file layer to dest/p.
func writeLayerFile(dest, p string, r io.Reader, lim *limits) error {
	root, err := os.OpenRoot(dest)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return writeFile(root, p, r, lim)
}
