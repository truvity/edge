package docspack

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode/utf8"
)

// Search limits.
const (
	DefaultSearchLimit = 10
	MaxSearchLimit     = 50
	maxQueryTerms      = 16
	maxPrefixTerms     = 24
	minPrefixLen       = 3
	snippetBytes       = 240
	snippetScanBytes   = 64 << 10

	bm25K1 = 1.2
	bm25B  = 0.75
	// A term in the section's own heading counts for more than one in the
	// text; in a heading above it, for a little more.
	ownHeadingBoost    = 2.0
	parentHeadingBoost = 0.5
	// A term found only as the start of another term counts for less.
	prefixWeight = 0.5
)

// ErrNotFound is wrapped by the errors of a lookup that found nothing.
var ErrNotFound = errors.New("not found")

// DocInfo is what a listing says about a document.
type DocInfo struct {
	Route, URL, Title string
	Sections          int
}

// Hit is one result of a search.
type Hit struct {
	Route, URL, Title string
	// Heading is the path of headings of the section; empty for the text
	// before a document's first heading.
	Heading []string
	// Anchor is the section's anchor, "" for the text before the first heading.
	Anchor  string
	Score   float64
	Snippet string
}

// SearchOptions narrow a search.
type SearchOptions struct {
	// Limit is the number of hits, DefaultSearchLimit when zero, at most
	// MaxSearchLimit.
	Limit int
	// Subdir keeps only documents whose route is below it.
	Subdir string
}

// Docs lists the documents whose route is below subdir ("" for all), in
// route order.
func (p *Pack) Docs(subdir string) []DocInfo {
	var out []DocInfo
	for i := range p.Manifest.Docs {
		d := &p.Manifest.Docs[i]
		if inSubdir(d.Route, subdir) {
			out = append(out, DocInfo{Route: d.Route, URL: d.URL, Title: d.Title, Sections: len(d.Sections)})
		}
	}
	return out
}

// CleanSubdir normalizes a directory a caller names: no leading or trailing
// slash, and a clean relative path or an error.
func CleanSubdir(s string) (string, error) {
	s = strings.Trim(strings.TrimSpace(s), "/")
	if s == "" {
		return "", nil
	}
	if err := ValidRoute(s); err != nil {
		return "", fmt.Errorf("directory %q: %w", s, err)
	}
	return s, nil
}

func inSubdir(route, subdir string) bool {
	return subdir == "" || route == subdir || strings.HasPrefix(route, subdir+"/")
}

// Resolve finds the document a caller means by route: the route as listed,
// or without its extension, or the URL path of its page ("/guides/install/").
func (p *Pack) Resolve(route string) (int, bool) {
	r := strings.Trim(strings.TrimSpace(route), "/")
	r = strings.TrimPrefix(r, "./")
	if r == "" {
		return 0, false
	}
	for _, c := range []string{r, r + ".md", r + ".mdx", r + ".markdown", r + "/index.md", r + "/index.mdx", r + "/index.markdown"} {
		if i, ok := p.routes[c]; ok {
			return i, true
		}
	}
	return 0, false
}

// Read returns the text of a document, or of one of its sections with their
// subsections when anchor is not empty, as raw Markdown.
func (p *Pack) Read(route, anchor string) (DocInfo, string, error) {
	i, ok := p.Resolve(route)
	if !ok {
		return DocInfo{}, "", fmt.Errorf("%w: no document %q (list_docs shows the routes)", ErrNotFound, route)
	}
	d := &p.Manifest.Docs[i]
	info := DocInfo{Route: d.Route, URL: d.URL, Title: d.Title, Sections: len(d.Sections)}
	data := p.docs[i].data
	if anchor == "" {
		return info, validUTF8(data), nil
	}
	anchor = strings.TrimPrefix(strings.TrimSpace(anchor), "#")
	for j := range d.Sections {
		if s := &d.Sections[j]; s.Anchor == anchor && s.Level > 0 {
			return info, validUTF8(data[s.Start:s.SubtreeEnd]), nil
		}
	}
	return info, "", fmt.Errorf("%w: %q has no section %q; its sections: %s", ErrNotFound, d.Route, anchor, p.anchorList(d))
}

func (p *Pack) anchorList(d *Doc) string {
	var parts []string
	for j := range d.Sections {
		if s := &d.Sections[j]; s.Level > 0 {
			parts = append(parts, s.Anchor)
		}
		if len(parts) == 50 {
			parts = append(parts, "...")
			break
		}
	}
	if len(parts) == 0 {
		return "(none: the document has no headings)"
	}
	return strings.Join(parts, ", ")
}

// LLMS returns the llms-full.txt of the site, else its llms.txt, else an
// outline of the pack's documents; name says which one it is.
func (p *Pack) LLMS() (name, text string) {
	switch {
	case p.llmsFull != nil:
		return LLMSFullName, validUTF8(p.llmsFull)
	case p.llms != nil:
		return LLMSName, validUTF8(p.llms)
	}
	var b strings.Builder
	b.WriteString("# Documentation outline\n\nThis pack carries no llms.txt; these are its documents. Read one with read_doc.\n\n")
	for i := range p.Manifest.Docs {
		d := &p.Manifest.Docs[i]
		fmt.Fprintf(&b, "- %s: %s\n", d.Route, d.Title)
	}
	return "outline", b.String()
}

type scored struct {
	sec   int
	score float64
}

// Search ranks the sections of the pack for query (BM25 over the index built
// into the pack, with a boost for terms in headings) and returns the best.
// It reads the posting lists of the query's terms; it does not scan text,
// except the one section of each hit it takes a snippet from.
func (p *Pack) Search(query string, opt SearchOptions) ([]Hit, error) {
	terms := Terms(query)
	if len(terms) == 0 {
		return nil, errors.New("the query has no searchable word (letters or digits, two or more)")
	}
	if len(terms) > maxQueryTerms {
		terms = terms[:maxQueryTerms]
	}
	limit := opt.Limit
	if limit <= 0 {
		limit = DefaultSearchLimit
	}
	limit = min(limit, MaxSearchLimit)
	subdir, err := CleanSubdir(opt.Subdir)
	if err != nil {
		return nil, err
	}

	n := float64(len(p.secs))
	scores := map[int]float64{}
	matched := map[int]map[string]bool{}
	add := func(qterm, term string, weight float64) {
		posts := p.postings[term]
		idf := math.Log(1 + (n-float64(len(posts))+0.5)/(float64(len(posts))+0.5))
		for _, e := range posts {
			tf := float64(e[1])
			length := float64(p.Manifest.Docs[p.secs[e[0]].doc].Sections[p.secs[e[0]].idx].Tokens)
			norm := tf * (bm25K1 + 1) / (tf + bm25K1*(1-bm25B+bm25B*length/p.avgLen))
			scores[e[0]] += weight * idf * norm
			if matched[e[0]] == nil {
				matched[e[0]] = map[string]bool{}
			}
			matched[e[0]][qterm] = true
		}
	}
	for _, t := range terms {
		if _, ok := p.postings[t]; ok {
			add(t, t, 1)
		}
		if utf8.RuneCountInString(t) < minPrefixLen {
			continue
		}
		// Terms that start with the query term ("installer" for "install")
		// count for less than the term itself.
		i := sort.SearchStrings(p.terms, t)
		for k := 0; k < maxPrefixTerms && i+k < len(p.terms) && strings.HasPrefix(p.terms[i+k], t); k++ {
			if p.terms[i+k] != t {
				add(t, p.terms[i+k], prefixWeight)
			}
		}
	}

	hits := make([]scored, 0, len(scores))
	for sec, sc := range scores {
		ref := p.secs[sec]
		if !inSubdir(p.Manifest.Docs[ref.doc].Route, subdir) {
			continue
		}
		s := &p.Manifest.Docs[ref.doc].Sections[ref.idx]
		sc += p.headingBoost(s, terms)
		// A section that holds every term outranks one that holds some.
		sc *= 1 + float64(len(matched[sec]))/float64(len(terms))
		hits = append(hits, scored{sec, sc})
	}
	sort.Slice(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score > hits[b].score
		}
		return hits[a].sec < hits[b].sec
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Hit, 0, len(hits))
	for _, h := range hits {
		ref := p.secs[h.sec]
		d := &p.Manifest.Docs[ref.doc]
		s := &d.Sections[ref.idx]
		heading := s.Heading
		if len(heading) == 0 {
			heading = []string{d.Title}
		}
		out = append(out, Hit{
			Route: d.Route, URL: d.URL, Title: d.Title, Heading: heading, Anchor: s.Anchor,
			Score: math.Round(h.score*1000) / 1000, Snippet: p.snippet(ref, terms),
		})
	}
	return out, nil
}

// headingBoost adds to the score for each query term found in the section's
// own heading, and less for each found in the headings above it.
func (p *Pack) headingBoost(s *Section, terms []string) float64 {
	if len(s.Heading) == 0 {
		return 0
	}
	own := map[string]bool{}
	for _, t := range Terms(s.Heading[len(s.Heading)-1]) {
		own[t] = true
	}
	parent := map[string]bool{}
	for _, h := range s.Heading[:len(s.Heading)-1] {
		for _, t := range Terms(h) {
			parent[t] = true
		}
	}
	var b float64
	for _, t := range terms {
		switch {
		case own[t] || hasPrefixTerm(own, t):
			b += ownHeadingBoost
		case parent[t] || hasPrefixTerm(parent, t):
			b += parentHeadingBoost
		}
	}
	return b
}

func hasPrefixTerm(set map[string]bool, prefix string) bool {
	if utf8.RuneCountInString(prefix) < minPrefixLen {
		return false
	}
	for t := range set {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

// snippet picks the line of the section that holds the most query terms (the
// first such line on a tie, never the heading line unless nothing else has
// text) and shortens it.
func (p *Pack) snippet(ref secRef, terms []string) string {
	d := &p.Manifest.Docs[ref.doc]
	s := &d.Sections[ref.idx]
	text := validUTF8(p.docs[ref.doc].data[s.Start:s.End])
	if len(text) > snippetScanBytes {
		text = strings.ToValidUTF8(text[:snippetScanBytes], "")
	}
	best, bestN, first := "", 0, ""
	for k, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || (k == 0 && s.Level > 0) {
			continue
		}
		if first == "" {
			first = line
		}
		lower := strings.ToLower(line)
		n := 0
		for _, t := range terms {
			if strings.Contains(lower, t) {
				n++
			}
		}
		if n > bestN {
			best, bestN = line, n
		}
	}
	if best == "" {
		best = first
	}
	if best == "" {
		return ""
	}
	return shorten(best, snippetBytes)
}

// shorten cuts s to at most limit bytes at a character boundary, marking the cut.
func shorten(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	cut := limit - len("...")
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.TrimSpace(s[:cut]) + "..."
}
