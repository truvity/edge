package staticsite

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// entry is one servable file. Text types that take placeholders are held in
// body, already substituted; everything else is opened from disk on demand.
type entry struct {
	disk   string
	body   []byte
	size   int64
	ctype  string
	etag   string
	hashed bool
	html   bool
}

// Site is one loaded, immutable snapshot of the content.
type Site struct {
	files    map[string]*entry // "/a/b.css"
	dirs     map[string]bool   // "/a"
	Digest   string
	LoadedAt time.Time
	root     string
}

// builtin content types win over the host's mime database, which differs
// between images (a distroless image has none).
var builtinTypes = map[string]string{
	".html":        "text/html; charset=utf-8",
	".htm":         "text/html; charset=utf-8",
	".css":         "text/css; charset=utf-8",
	".js":          "text/javascript; charset=utf-8",
	".mjs":         "text/javascript; charset=utf-8",
	".json":        "application/json",
	".webmanifest": "application/manifest+json",
	".map":         "application/json",
	".txt":         "text/plain; charset=utf-8",
	".xml":         "application/xml",
	".svg":         "image/svg+xml",
	".png":         "image/png",
	".jpg":         "image/jpeg",
	".jpeg":        "image/jpeg",
	".gif":         "image/gif",
	".webp":        "image/webp",
	".avif":        "image/avif",
	".ico":         "image/x-icon",
	".woff":        "font/woff",
	".woff2":       "font/woff2",
	".ttf":         "font/ttf",
	".otf":         "font/otf",
	".wasm":        "application/wasm",
	".pdf":         "application/pdf",
	".mp4":         "video/mp4",
	".webm":        "video/webm",
}

func contentType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := builtinTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	return "application/octet-stream"
}

// Build indexes root. Only regular files are served: a symlink, device or
// socket is skipped, so nothing under root can lead outside it. Placeholders
// are substituted here, once, and the ETag is the hash of what is served.
func Build(root string, cfg *Config) (*Site, error) {
	s := &Site{files: map[string]*entry{}, dirs: map[string]bool{}, LoadedAt: time.Now(), root: root}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		key := "/" + filepath.ToSlash(rel)
		if d.IsDir() {
			s.dirs[key] = true
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		e, err := buildEntry(p, key, cfg)
		if err != nil {
			return err
		}
		s.files[key] = e
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(s.files) == 0 {
		return nil, fmt.Errorf("%s holds no files", root)
	}
	return s, nil
}

func buildEntry(disk, key string, cfg *Config) (*entry, error) {
	f, err := os.Open(disk)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	e := &entry{disk: disk, ctype: contentType(key)}
	if t := cfg.ContentTypes[key]; t != "" {
		e.ctype = t
	}
	kind := kindOf(key)
	e.html = kind == kindHTML
	e.hashed = !e.html && cfg.HashedPattern != nil && cfg.HashedPattern.MatchString(path.Base(key))
	h := sha256.New()
	if kind != kindNone && len(cfg.Placeholders) > 0 {
		raw, err := io.ReadAll(f)
		if err != nil {
			return nil, err
		}
		if err := checkTokens(key, kind, raw, cfg); err != nil {
			return nil, err
		}
		e.body = substitute(kind, raw, cfg.Placeholders)
		e.size = int64(len(e.body))
		h.Write(e.body)
	} else {
		n, err := io.Copy(h, f)
		if err != nil {
			return nil, err
		}
		e.size = n
	}
	e.etag = `"` + hex.EncodeToString(h.Sum(nil)[:16]) + `"`
	return e, nil
}
