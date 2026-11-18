package docspack

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// BuildOptions are the settings of Build.
type BuildOptions struct {
	// In is the directory of Markdown; Out the pack directory to write. Out
	// must not exist or be empty, and must not lie inside In.
	In, Out string
	// CreatedAt, when not zero, is recorded in the manifest. Leave it zero
	// for a pack whose bytes depend on the input alone.
	CreatedAt time.Time
	// Title is recorded in the manifest; optional.
	Title string
	// MaxBytes caps the bytes of all files of the pack; zero means
	// DefaultMaxBytes.
	MaxBytes int64
}

// Summary is what Build reports.
type Summary struct {
	Docs, Sections, Terms int
	Bytes                 int64
}

var markdownExt = map[string]bool{".md": true, ".markdown": true, ".mdx": true}

// Build turns the Markdown under opt.In into a pack at opt.Out. The output
// depends on the input alone: files and sections are in sorted order, JSON
// maps are written with sorted keys, and no time is recorded unless
// opt.CreatedAt is set.
//
// Files and directories whose name starts with a dot, and node_modules, are
// not read. A symbolic link is an error, not skipped: a pack must not take in
// a file from outside the input. llms.txt and llms-full.txt at the top of the
// input are carried along as they are.
func Build(opt BuildOptions) (Summary, error) {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	in, err := filepath.Abs(opt.In)
	if err != nil {
		return Summary{}, err
	}
	out, err := filepath.Abs(opt.Out)
	if err != nil {
		return Summary{}, err
	}
	if st, err := os.Stat(in); err != nil || !st.IsDir() {
		return Summary{}, fmt.Errorf("input %s is not a directory", opt.In)
	}
	if rel, err := filepath.Rel(in, out); err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		return Summary{}, fmt.Errorf("output %s is inside the input %s", opt.Out, opt.In)
	}
	if entries, err := os.ReadDir(out); err == nil && len(entries) > 0 {
		return Summary{}, fmt.Errorf("output %s is not empty", opt.Out)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Summary{}, err
	}

	routes, err := collect(in)
	if err != nil {
		return Summary{}, err
	}
	if len(routes) == 0 {
		return Summary{}, fmt.Errorf("no Markdown files (.md, .markdown, .mdx) under %s", opt.In)
	}
	if len(routes) > MaxDocs {
		return Summary{}, fmt.Errorf("%d Markdown files; a pack holds at most %d", len(routes), MaxDocs)
	}

	opt.Title = strings.TrimSpace(opt.Title)
	if len(opt.Title) > 200 || !utf8.ValidString(opt.Title) || strings.ContainsFunc(opt.Title, unicode.IsControl) {
		return Summary{}, errors.New("the title is longer than 200 bytes, or not valid UTF-8, or has a control character")
	}
	m := Manifest{Format: Format, FormatVersion: FormatVersion, Title: opt.Title, Tokenizer: Tokenizer}
	if !opt.CreatedAt.IsZero() {
		m.CreatedAt = opt.CreatedAt.UTC().Format(time.RFC3339)
	}
	idx := Index{FormatVersion: FormatVersion, Tokenizer: Tokenizer, Postings: map[string][][2]int{}}
	files := map[string][]byte{}
	var total int64
	for _, route := range routes {
		data, err := os.ReadFile(filepath.Join(in, filepath.FromSlash(route)))
		if err != nil {
			return Summary{}, err
		}
		if len(data) > MaxDocBytes {
			return Summary{}, fmt.Errorf("%s is %d bytes; a document is at most %d", route, len(data), MaxDocBytes)
		}
		total += int64(len(data))
		p := parse(data, fallbackTitle(route))
		first := idx.Sections
		if first+len(p.sections) > MaxSections {
			return Summary{}, fmt.Errorf("more than %d sections", MaxSections)
		}
		for i, tf := range p.terms {
			for term, n := range tf {
				idx.Postings[term] = append(idx.Postings[term], [2]int{first + i, n})
			}
		}
		idx.Sections += len(p.sections)
		store := DocsDir + "/" + route
		files[store] = data
		m.Docs = append(m.Docs, Doc{Route: route, URL: siteURL(route), Title: p.title, File: describe(store, data), Sections: p.sections})
	}
	for _, name := range []string{LLMSName, LLMSFullName} {
		data, err := os.ReadFile(filepath.Join(in, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Summary{}, err
		}
		if len(data) > MaxLLMSBytes {
			return Summary{}, fmt.Errorf("%s is %d bytes; at most %d", name, len(data), MaxLLMSBytes)
		}
		total += int64(len(data))
		files[name] = data
		f := describe(name, data)
		if name == LLMSName {
			m.LLMS = &f
		} else {
			m.LLMSFull = &f
		}
	}

	ib, err := json.Marshal(idx)
	if err != nil {
		return Summary{}, err
	}
	ib = append(ib, '\n')
	files[IndexName] = ib
	m.Index = describe(IndexName, ib)
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Summary{}, err
	}
	mb = append(mb, '\n')
	files[ManifestName] = mb
	total += int64(len(ib) + len(mb))
	if total > opt.MaxBytes {
		return Summary{}, fmt.Errorf("the pack is %d bytes, over the %d limit", total, opt.MaxBytes)
	}

	for name, data := range files {
		dst := filepath.Join(out, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return Summary{}, err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return Summary{}, err
		}
	}
	return Summary{Docs: len(m.Docs), Sections: idx.Sections, Terms: len(idx.Postings), Bytes: total}, nil
}

func describe(name string, data []byte) File {
	sum := sha256.Sum256(data)
	return File{Path: name, Bytes: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
}

// collect lists the routes of the Markdown files under root, sorted.
func collect(root string) ([]string, error) {
	var routes []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == root {
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || name == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			rel, _ := filepath.Rel(root, p)
			return fmt.Errorf("%s is a symbolic link: a pack does not follow links out of the input", filepath.ToSlash(rel))
		}
		if d.IsDir() || !markdownExt[strings.ToLower(path.Ext(name))] {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		route := filepath.ToSlash(rel)
		if err := ValidRoute(route); err != nil {
			return err
		}
		routes = append(routes, route)
		return nil
	})
	sort.Strings(routes)
	return routes, err
}
