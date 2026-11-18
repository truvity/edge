// Package docspack is the "MCP pack": the versioned directory a documentation
// site is turned into so that docs-mcp can serve it to agents without
// scanning Markdown at query time.
//
// A pack is built once, in CI, by `docs-mcp pack` (Build) and read once at
// start by `docs-mcp serve` (Load). It holds the Markdown files as they are,
// a manifest that lists them with a section index (heading path, anchor and
// byte ranges of every section), and an inverted index of the terms of every
// section, which is what ranked search reads. See docs/docs-mcp.md for the
// format.
//
// The format is owned and versioned here. FormatVersion changes whenever a
// reader of the previous version could misread the pack; fields that only
// add information do not change it.
package docspack

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// Format names the kind of directory a manifest describes.
	Format = "docs-mcp-pack"
	// FormatVersion is the version of the pack format this code writes and the
	// only one it reads.
	FormatVersion = 1
	// Tokenizer names how text is cut into the terms of the index. A reader
	// must tokenize a query the same way, so a different tokenizer is a
	// different format version.
	Tokenizer = "unicode-alnum-lower-v1"

	// ManifestName is the entry point of a pack.
	ManifestName = "manifest.json"
	// IndexName is the inverted index over the sections.
	IndexName = "index.json"
	// DocsDir holds the Markdown files, at their routes.
	DocsDir = "docs"
	// LLMSName is the site's llms.txt, when it has one, at the root of the pack.
	LLMSName = "llms.txt"
	// LLMSFullName is the site's llms-full.txt, likewise.
	LLMSFullName = "llms-full.txt"

	// The limits below are shared by the writer and the reader: a pack the
	// writer accepts is one every reader accepts at its default limits.

	// MaxDocs is the number of documents of a pack.
	MaxDocs = 20000
	// MaxDocBytes is the size of one document.
	MaxDocBytes = 8 << 20
	// MaxSections is the number of sections of a pack.
	MaxSections = 1_000_000
	// MaxRouteBytes is the length of a route.
	MaxRouteBytes = 512
	// MaxAnchorBytes is the length of an anchor.
	MaxAnchorBytes = 256
	// MaxHeadingDepth is the deepest heading level, and so the longest heading path.
	MaxHeadingDepth = 6
	// MaxLLMSBytes is the size of llms.txt and of llms-full.txt each.
	MaxLLMSBytes = 16 << 20
	// DefaultMaxBytes is the default cap on the bytes of all files of a pack.
	DefaultMaxBytes = 64 << 20

	maxManifestBytes = 256 << 20
)

// File names one file of the pack, relative to the pack root, with its size
// and SHA-256 so that a reader can tell a damaged pack from a good one.
type File struct {
	Path   string `json:"path"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// Section is one heading of a document and the text under it.
//
// Start is the offset of the heading line in the document file; End is where
// the next heading of any level starts (or the end of the file): the text the
// section owns and the index covers. SubtreeEnd is where the next heading of
// the same or a higher level starts: what a reader gets for the section with
// its subsections. All three are byte offsets into the file as stored in the
// pack, front matter included. The text before a document's first heading is
// a section of its own with no heading and no anchor, at Level 0.
type Section struct {
	// Heading is the path of heading texts from the top of the document to
	// this section; empty for the text before the first heading.
	Heading    []string `json:"heading"`
	Anchor     string   `json:"anchor"`
	Level      int      `json:"level"`
	Start      int      `json:"start"`
	End        int      `json:"end"`
	SubtreeEnd int      `json:"subtreeEnd"`
	// Tokens is the number of terms of the section, the length ranking
	// normalizes by.
	Tokens int `json:"tokens"`
}

// Doc is one Markdown file of the pack.
type Doc struct {
	// Route is the path of the file relative to the input directory, with
	// forward slashes: the name a reader asks for.
	Route string `json:"route"`
	// URL is the path of the page on the site the Markdown builds to
	// ("/guides/install/"), for a reader that wants to link to it.
	URL   string `json:"url"`
	Title string `json:"title"`
	// File is embedded, so its path, bytes and sha256 sit beside the fields
	// above in the JSON.
	File
	Sections []Section `json:"sections"`
}

// Manifest is manifest.json, the entry point of a pack.
type Manifest struct {
	Format        string `json:"format"`
	FormatVersion int    `json:"formatVersion"`
	// CreatedAt is set only when the build is given a time (SOURCE_DATE_EPOCH):
	// a pack with none is the same bytes every time it is built.
	CreatedAt string `json:"createdAt,omitempty"`
	// Title names the documentation set ("Example Platform Docs"), for the
	// instructions a server gives its clients. Optional.
	Title     string `json:"title,omitempty"`
	Tokenizer string `json:"tokenizer"`
	// Docs are sorted by Route.
	Docs []Doc `json:"docs"`
	// Index is index.json, the inverted index over the sections of Docs.
	Index File `json:"index"`
	// LLMS and LLMSFull are the llms.txt and llms-full.txt the site publishes,
	// when it has them.
	LLMS     *File `json:"llms,omitempty"`
	LLMSFull *File `json:"llmsFull,omitempty"`
}

// Index is index.json: for every term, the sections that hold it and how
// often. Sections are numbered across all documents, in the order of
// Manifest.Docs and then of each document's Sections.
type Index struct {
	FormatVersion int    `json:"formatVersion"`
	Tokenizer     string `json:"tokenizer"`
	// Sections is the total number of sections, a check against the manifest.
	Sections int `json:"sections"`
	// Postings maps a term to (section, term frequency) pairs, ascending by
	// section.
	Postings map[string][][2]int `json:"postings"`
}

// ValidRoute reports whether r is a route the format allows: a relative path
// of clean, non-empty segments with forward slashes and no control
// characters.
func ValidRoute(r string) error {
	switch {
	case r == "":
		return fmt.Errorf("empty route")
	case len(r) > MaxRouteBytes:
		return fmt.Errorf("route is longer than %d bytes", MaxRouteBytes)
	case !utf8.ValidString(r):
		return fmt.Errorf("route %q is not valid UTF-8", r)
	case strings.HasPrefix(r, "/"):
		return fmt.Errorf("route %q is absolute", r)
	}
	for _, c := range r {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return fmt.Errorf("route %q has a control character or a backslash", r)
		}
	}
	for _, seg := range strings.Split(r, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("route %q has an empty, . or .. segment", r)
		}
	}
	return nil
}
