package docspack

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sample = "---\ntitle: Install the thing\n---\nIntro text before any heading.\n\n" +
	"# Install\n\nRun it.\n\n" +
	"## Prerequisites\n\nYou need `go`.\n\n```sh\n# not a heading\n```\n\n" +
	"## Prerequisites\n\nAgain.\n\n" +
	"# Usage {#use}\n\nDone.\n"

func TestParseSectionsAndRanges(t *testing.T) {
	p := parse([]byte(sample), "fallback")
	assert.Equal(t, "Install the thing", p.title)
	require.Len(t, p.sections, 5)

	pre := p.sections[0]
	assert.Empty(t, pre.Heading)
	assert.Equal(t, 0, pre.Level)
	assert.Equal(t, "", pre.Anchor)
	assert.Equal(t, "Intro text before any heading.\n\n", sample[pre.Start:pre.End])

	install := p.sections[1]
	assert.Equal(t, []string{"Install"}, install.Heading)
	assert.Equal(t, "install", install.Anchor)
	assert.Equal(t, "# Install\n\nRun it.\n\n", sample[install.Start:install.End])
	// The subtree runs to the next level-1 heading: both Prerequisites are in it.
	assert.Contains(t, sample[install.Start:install.SubtreeEnd], "Again.")
	assert.NotContains(t, sample[install.Start:install.SubtreeEnd], "Done.")

	pr1, pr2 := p.sections[2], p.sections[3]
	assert.Equal(t, []string{"Install", "Prerequisites"}, pr1.Heading)
	assert.Equal(t, "prerequisites", pr1.Anchor)
	assert.Equal(t, "prerequisites-1", pr2.Anchor, "a repeated heading is numbered")
	assert.Contains(t, sample[pr1.Start:pr1.End], "# not a heading", "a code fence holds no heading")

	use := p.sections[4]
	assert.Equal(t, "use", use.Anchor, "an explicit {#id} is the anchor")
	assert.Equal(t, []string{"Usage"}, use.Heading)
	assert.Equal(t, len(sample), use.End)
	assert.Equal(t, len(sample), use.SubtreeEnd)
}

func TestParseTitleFallbacks(t *testing.T) {
	assert.Equal(t, "Heading", parse([]byte("# Heading\n\ntext\n"), "file").title)
	assert.Equal(t, "file", parse([]byte("just text\n"), "file").title)
	assert.Equal(t, "file", parse([]byte("---\n: : bad yaml\n---\ntext\n"), "file").title)
}

func TestParseWithoutHeadings(t *testing.T) {
	p := parse([]byte("only text here\n"), "f")
	require.Len(t, p.sections, 1)
	assert.Empty(t, p.sections[0].Heading)
	assert.Equal(t, 1, p.terms[0]["only"])
	assert.Empty(t, parse([]byte("---\ntitle: x\n---\n\n"), "f").sections)
}

func TestParseHeadingForms(t *testing.T) {
	p := parse([]byte("## Closed ##\n\n### A [link](http://example.com) and `code`\n\n#NotAHeading\n\n    # indented code\n"), "f")
	require.Len(t, p.sections, 2)
	assert.Equal(t, []string{"Closed"}, p.sections[0].Heading)
	assert.Equal(t, []string{"Closed", "A link and code"}, p.sections[1].Heading)
	assert.Equal(t, "a-link-and-code", p.sections[1].Anchor)
}

func TestAnchorsStayUnique(t *testing.T) {
	a := newAnchors()
	assert.Equal(t, "a-1", a.next("a-1"))
	assert.Equal(t, "a", a.next("a"))
	assert.Equal(t, "a-2", a.next("a"), "a-1 is taken")
	assert.Equal(t, "a-3", a.next("a"))
}

func TestTokenize(t *testing.T) {
	assert.Equal(t, []string{"docs", "mcp", "v2", "naive", "日"}, Terms("Docs-MCP v2, a Naive 日 x"))
	assert.Empty(t, Terms("a b - !"))
}

func TestSiteURL(t *testing.T) {
	assert.Equal(t, "/", siteURL("index.md"))
	assert.Equal(t, "/guides/", siteURL("guides/index.mdx"))
	assert.Equal(t, "/guides/install/", siteURL("guides/install.md"))
}

func TestValidRoute(t *testing.T) {
	for _, ok := range []string{"a.md", "a/b.md", "with space/ü.md"} {
		assert.NoError(t, ValidRoute(ok), ok)
	}
	for _, bad := range []string{"", "/a.md", "a//b.md", "../a.md", "a/./b.md", "a\\b.md", "a\nb.md", "a/"} {
		assert.Error(t, ValidRoute(bad), bad)
	}
}
