package docspack

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"unicode/utf8"
)

// ErrUnsupported is wrapped by the error Load returns for a pack of a format
// or a version this code does not read.
var ErrUnsupported = errors.New("unsupported pack")

// LoadOptions are the limits Load holds a pack to.
type LoadOptions struct {
	// MaxBytes caps the bytes of all files of the pack; zero means
	// DefaultMaxBytes.
	MaxBytes int64
}

// Pack is a loaded pack: the documents in memory and the index ready to
// search. It is immutable, so any number of requests may read it.
type Pack struct {
	Manifest Manifest

	docs   []docData
	routes map[string]int
	secs   []secRef

	terms    []string // sorted
	postings map[string][][2]int
	avgLen   float64

	llms, llmsFull []byte
	bytes          int64
}

type docData struct {
	data []byte
}

type secRef struct {
	doc, idx int // the document, and the section within it
}

// Load reads and checks the pack in dir. A pack of another format, or of a
// format version this code does not read, is refused (ErrUnsupported) before
// anything else of it is read; a pack whose files do not match the sizes and
// digests of its manifest is refused too.
func Load(dir string, opt LoadOptions) (*Pack, error) {
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = DefaultMaxBytes
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	r := &reader{root: root, left: opt.MaxBytes}

	raw, err := r.read(ManifestName, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	var head struct {
		Format        string `json:"format"`
		FormatVersion int    `json:"formatVersion"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if head.Format != Format {
		return nil, fmt.Errorf("%w: format %q, want %q", ErrUnsupported, head.Format, Format)
	}
	if head.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: format version %d; this server reads version %d", ErrUnsupported, head.FormatVersion, FormatVersion)
	}
	p := &Pack{routes: map[string]int{}}
	if err := json.Unmarshal(raw, &p.Manifest); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	m := &p.Manifest
	if m.Tokenizer != Tokenizer {
		return nil, fmt.Errorf("%w: tokenizer %q, want %q", ErrUnsupported, m.Tokenizer, Tokenizer)
	}
	if err := validTitle(m.Title); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if len(m.Docs) > MaxDocs {
		return nil, fmt.Errorf("manifest lists %d documents; at most %d", len(m.Docs), MaxDocs)
	}

	total := 0
	for i := range m.Docs {
		d := &m.Docs[i]
		if err := ValidRoute(d.Route); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
		if i > 0 && m.Docs[i-1].Route >= d.Route {
			return nil, fmt.Errorf("documents are not in ascending order of route at %q", d.Route)
		}
		if d.Path != DocsDir+"/"+d.Route {
			return nil, fmt.Errorf("document %q: path %q, want %q", d.Route, d.Path, DocsDir+"/"+d.Route)
		}
		data, err := r.readFile(d.File, MaxDocBytes)
		if err != nil {
			return nil, fmt.Errorf("document %q: %w", d.Route, err)
		}
		if err := checkSections(d, len(data)); err != nil {
			return nil, fmt.Errorf("document %q: %w", d.Route, err)
		}
		p.routes[d.Route] = i
		p.docs = append(p.docs, docData{data: data})
		for j := range d.Sections {
			p.secs = append(p.secs, secRef{doc: i, idx: j})
		}
		total += len(d.Sections)
		if total > MaxSections {
			return nil, fmt.Errorf("more than %d sections", MaxSections)
		}
	}

	if m.Index.Path != IndexName {
		return nil, fmt.Errorf("index path %q, want %q", m.Index.Path, IndexName)
	}
	ib, err := r.readFile(m.Index, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	var idx Index
	if err := json.Unmarshal(ib, &idx); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	if idx.FormatVersion != FormatVersion || idx.Tokenizer != Tokenizer || idx.Sections != total {
		return nil, fmt.Errorf("index does not belong to the manifest (format version %d, tokenizer %q, %d sections; the manifest has %d)",
			idx.FormatVersion, idx.Tokenizer, idx.Sections, total)
	}
	for term, posts := range idx.Postings {
		last := -1
		for _, e := range posts {
			if e[0] <= last || e[0] >= total || e[1] <= 0 {
				return nil, fmt.Errorf("index: bad posting for %q", term)
			}
			last = e[0]
		}
	}
	p.postings = idx.Postings
	p.terms = make([]string, 0, len(idx.Postings))
	for t := range idx.Postings {
		p.terms = append(p.terms, t)
	}
	sort.Strings(p.terms)
	sumLen := 0
	for i := range m.Docs {
		for j := range m.Docs[i].Sections {
			sumLen += m.Docs[i].Sections[j].Tokens
		}
	}
	if total > 0 {
		p.avgLen = max(1, float64(sumLen)/float64(total))
	}

	for _, f := range []struct {
		file *File
		name string
		dst  *[]byte
	}{{m.LLMS, LLMSName, &p.llms}, {m.LLMSFull, LLMSFullName, &p.llmsFull}} {
		if f.file == nil {
			continue
		}
		if f.file.Path != f.name {
			return nil, fmt.Errorf("%s: path %q", f.name, f.file.Path)
		}
		if *f.dst, err = r.readFile(*f.file, MaxLLMSBytes); err != nil {
			return nil, fmt.Errorf("%s: %w", f.name, err)
		}
	}
	p.bytes = opt.MaxBytes - r.left
	return p, nil
}

// checkSections holds the sections of a document to what the readers rely on.
func checkSections(d *Doc, size int) error {
	seen := map[string]bool{}
	for i := range d.Sections {
		s := &d.Sections[i]
		if s.Start < 0 || s.Start > s.End || s.End > s.SubtreeEnd || s.SubtreeEnd > size {
			return fmt.Errorf("section %d has bad byte ranges", i)
		}
		badLevel := s.Level < 0 || s.Level > MaxHeadingDepth || len(s.Heading) > MaxHeadingDepth || (s.Level == 0) != (len(s.Heading) == 0)
		if badLevel || s.Tokens < 0 || s.Tokens > s.End-s.Start {
			return fmt.Errorf("section %d has a bad level, heading or token count", i)
		}
		if s.Level > 0 && (s.Anchor == "" || len(s.Anchor) > MaxAnchorBytes || seen[s.Anchor]) {
			return fmt.Errorf("section %d has an empty, too long or repeated anchor %q", i, s.Anchor)
		}
		seen[s.Anchor] = true
		if i > 0 && s.Start < d.Sections[i-1].Start {
			return fmt.Errorf("section %d starts before the one before it", i)
		}
	}
	return nil
}

// reader reads the files of a pack through an os.Root, so a name can never
// leave the directory, and charges every byte to one budget.
type reader struct {
	root *os.Root
	left int64
}

func (r *reader) read(name string, limit int64) ([]byte, error) {
	f, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	limit = min(limit, r.left)
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d bytes allowed", name, limit)
	}
	r.left -= int64(len(data))
	return data, nil
}

// readFile reads a file the manifest describes and checks it against that
// description.
func (r *reader) readFile(f File, limit int64) ([]byte, error) {
	if f.Bytes < 0 || f.Bytes > limit {
		return nil, fmt.Errorf("%s: %d bytes declared, at most %d allowed", f.Path, f.Bytes, limit)
	}
	data, err := r.read(f.Path, f.Bytes)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != f.Bytes {
		return nil, fmt.Errorf("%s: %d bytes, the manifest says %d", f.Path, len(data), f.Bytes)
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), f.SHA256) {
		return nil, fmt.Errorf("%s: sha256 does not match the manifest", f.Path)
	}
	return data, nil
}

// Stats are the sizes of a loaded pack, for logs and metrics.
type Stats struct {
	Docs, Sections, Terms int
	Bytes                 int64
}

// Stats reports the sizes of the pack.
func (p *Pack) Stats() Stats {
	return Stats{Docs: len(p.docs), Sections: len(p.secs), Terms: len(p.terms), Bytes: p.bytes}
}

// validUTF8 makes raw file bytes safe to hand to a client as text. Callers
// slice first: it copies what it is given.
func validUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	return string(bytes.ToValidUTF8(b, []byte("\uFFFD")))
}
