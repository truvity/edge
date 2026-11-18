package docspack

import (
	"bytes"
	"path"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

// parsed is what the pack keeps of one Markdown file.
type parsed struct {
	title    string
	sections []Section
	// terms[i] is the term frequency of sections[i].
	terms []map[string]int
}

var (
	customID = regexp.MustCompile(`\s*\{#([A-Za-z0-9_-]+)\}\s*$`)
	mdLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	htmlTag  = regexp.MustCompile(`<[^>]*>`)
	emphasis = strings.NewReplacer("`", "", "**", "", "~~", "", "*", "")
)

const maxTermRun = 64

// nextLine returns the line of data that starts at off, without its line
// ending, and the offset the following line starts at.
func nextLine(data []byte, off int) (string, int) {
	i := bytes.IndexByte(data[off:], '\n')
	if i < 0 {
		return strings.TrimRight(string(data[off:]), "\r"), len(data)
	}
	return strings.TrimRight(string(data[off:off+i]), "\r"), off + i + 1
}

// frontMatterEnd returns the offset just after a YAML front matter block at
// the start of data and the block's text, or 0 and nil when there is none.
func frontMatterEnd(data []byte) (int, []byte) {
	off := 0
	if bytes.HasPrefix(data, []byte("\xef\xbb\xbf")) {
		off = 3
	}
	line, off := nextLine(data, off)
	if strings.TrimRight(line, " \t") != "---" {
		return 0, nil
	}
	fmStart := off
	for off < len(data) {
		at := off
		line, off = nextLine(data, off)
		if t := strings.TrimRight(line, " \t"); t == "---" || t == "..." {
			return off, data[fmStart:at]
		}
	}
	return 0, nil
}

// frontMatterTitle reads title from a front matter block; a block that does
// not parse, or has no string title, gives "".
func frontMatterTitle(fm []byte) string {
	var m map[string]any
	if yaml.Unmarshal(fm, &m) != nil {
		return ""
	}
	if s, ok := m["title"].(string); ok {
		return cleanInline(s)
	}
	return ""
}

// cleanInline reduces a heading or title to the words a reader sees: link
// and image syntax, HTML tags and emphasis marks are dropped.
func cleanInline(s string) string {
	s = mdLink.ReplaceAllString(s, "$1")
	s = htmlTag.ReplaceAllString(s, "")
	s = emphasis.Replace(s)
	return strings.Join(strings.Fields(s), " ")
}

// slug is the anchor of a heading: lower case, letters, digits, hyphens and
// underscores kept, spaces turned into hyphens, the rest dropped.
func slug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-':
			b.WriteRune(r)
		case unicode.IsSpace(r):
			b.WriteByte('-')
		}
	}
	if b.Len() == 0 {
		return "section"
	}
	return b.String()
}

// anchors hands out the anchors of one document: the second "Install" is
// "install-1", as a site builder numbers repeated headings.
type anchors struct {
	used  map[string]bool
	count map[string]int
}

func newAnchors() *anchors { return &anchors{used: map[string]bool{}, count: map[string]int{}} }

func (a *anchors) next(base string) string {
	if len(base) > MaxAnchorBytes {
		base = strings.ToValidUTF8(base[:MaxAnchorBytes], "")
	}
	n := a.count[base]
	cand := base
	if n > 0 {
		cand = base + "-" + strconv.Itoa(n)
	}
	for a.used[cand] {
		n++
		cand = base + "-" + strconv.Itoa(n)
	}
	a.count[base] = n + 1
	a.used[cand] = true
	return cand
}

type fence struct {
	char byte
	n    int
}

// fenceOpen reports the code fence a line opens, if it opens one.
func fenceOpen(line string) (fence, bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || len(t) < 3 || (t[0] != '`' && t[0] != '~') {
		return fence{}, false
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 || (t[0] == '`' && strings.ContainsRune(t[n:], '`')) {
		return fence{}, false
	}
	return fence{char: t[0], n: n}, true
}

// fenceCloses reports whether line closes the fence f.
func fenceCloses(line string, f fence) bool {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return false
	}
	n := 0
	for n < len(t) && t[n] == f.char {
		n++
	}
	return n >= f.n && strings.TrimSpace(t[n:]) == ""
}

// atxHeading parses an ATX heading line ("## Title ##"): its level, its
// cleaned text and an explicit {#id}, if any.
func atxHeading(line string) (level int, text, id string, ok bool) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, "", "", false
	}
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || (level < len(t) && t[level] != ' ' && t[level] != '\t') {
		return 0, "", "", false
	}
	t = strings.TrimSpace(t[level:])
	// A closing sequence of #s, set off by a space, is not part of the text.
	if trimmed := strings.TrimRight(t, "#"); trimmed != t && (trimmed == "" || strings.HasSuffix(trimmed, " ") || strings.HasSuffix(trimmed, "\t")) {
		t = strings.TrimSpace(trimmed)
	}
	if m := customID.FindStringSubmatch(t); m != nil {
		id = m[1]
		t = strings.TrimSpace(t[:len(t)-len(m[0])])
	}
	text = cleanInline(t)
	if text == "" {
		text = "(untitled)"
	}
	return level, text, id, true
}

// parse cuts one Markdown file into sections and counts the terms of each.
// fallback is the title when neither the front matter nor a level-1 heading
// gives one.
func parse(data []byte, fallback string) parsed {
	end, fm := frontMatterEnd(data)
	p := parsed{title: frontMatterTitle(fm)}

	type open struct {
		level int
		text  string
		idx   int
	}
	var (
		stack   []open
		seen    = newAnchors()
		inFence bool
		fenced  fence
		off     = end
	)
	// body is where the text before the first heading starts.
	body := end
	closeSection := func(idx, at int) {
		if idx >= 0 {
			p.sections[idx].End = at
		}
	}
	cur := -1
	for off < len(data) {
		line, next := nextLine(data, off)

		if inFence {
			if fenceCloses(line, fenced) {
				inFence = false
			}
			off = next
			continue
		}
		if f, ok := fenceOpen(line); ok {
			inFence, fenced = true, f
			off = next
			continue
		}
		level, text, id, ok := atxHeading(line)
		if !ok {
			off = next
			continue
		}
		if cur < 0 && hasText(data[body:off]) {
			p.sections = append(p.sections, Section{Heading: []string{}, Start: body, End: off, SubtreeEnd: off})
			cur = len(p.sections) - 1
		}
		closeSection(cur, off)
		for len(stack) > 0 && stack[len(stack)-1].level >= level {
			p.sections[stack[len(stack)-1].idx].SubtreeEnd = off
			stack = stack[:len(stack)-1]
		}
		if p.title == "" && level == 1 {
			p.title = text
		}
		anchor := id
		if anchor == "" {
			anchor = slug(text)
		}
		anchor = seen.next(anchor)
		stack = append(stack, open{level: level, text: text, idx: len(p.sections)})
		heading := make([]string, len(stack))
		for i := range stack {
			heading[i] = stack[i].text
		}
		p.sections = append(p.sections, Section{Heading: heading, Anchor: anchor, Level: level, Start: off, End: len(data), SubtreeEnd: len(data)})
		cur = len(p.sections) - 1
		off = next
	}
	if cur < 0 && hasText(data[body:]) {
		p.sections = append(p.sections, Section{Heading: []string{}, Start: body, End: len(data), SubtreeEnd: len(data)})
	}
	if p.title == "" {
		p.title = fallback
	}
	p.terms = make([]map[string]int, len(p.sections))
	for i := range p.sections {
		s := &p.sections[i]
		tf := map[string]int{}
		n := 0
		Tokenize(string(data[s.Start:s.End]), func(term string) {
			tf[term]++
			n++
		})
		s.Tokens = n
		p.terms[i] = tf
	}
	return p
}

func hasText(b []byte) bool { return len(bytes.TrimSpace(b)) > 0 }

// Tokenize calls emit for every term of s: a maximal run of letters and
// digits, lower-cased. A single ASCII character is not a term; a longer run
// than 64 characters is cut.
func Tokenize(s string, emit func(term string)) {
	var b strings.Builder
	runes := 0
	flush := func() {
		if runes > 1 || (runes == 1 && b.Len() > 1) {
			emit(b.String())
		}
		b.Reset()
		runes = 0
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if runes == maxTermRun {
				flush()
			}
			b.WriteRune(unicode.ToLower(r))
			runes++
			continue
		}
		flush()
	}
	flush()
}

// Terms returns the distinct terms of s, in the order they first appear.
func Terms(s string) []string {
	var out []string
	seen := map[string]bool{}
	Tokenize(s, func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	})
	return out
}

// siteURL is the path of the page a Markdown route builds to: the extension
// goes, and a directory's index is the directory.
func siteURL(route string) string {
	p := strings.TrimSuffix(route, path.Ext(route))
	if p == "index" {
		return "/"
	}
	p = strings.TrimSuffix(p, "/index")
	return "/" + p + "/"
}

// fallbackTitle is the title of a document with no title of its own: its file
// name without the extension.
func fallbackTitle(route string) string {
	base := path.Base(route)
	t := strings.TrimSuffix(base, path.Ext(base))
	if !utf8.ValidString(t) {
		return "untitled"
	}
	return t
}
