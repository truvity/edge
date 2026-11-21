package config_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mailPolicy is a zone that sends no mail: SPF on the apex, DMARC on
// _dmarc, both DNS-only TXT records written without quotes.
const mailPolicy = "  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n" +
	"    records:\n" +
	"      - {name: example.com, type: TXT, content: \"v=spf1 -all\"}\n" +
	"      - {name: _dmarc.example.com, type: TXT, content: \"v=DMARC1; p=reject; sp=none;\", ttl: 300}\n"

func kernelServesA() string {
	return gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	})
}

// TestTXTRecordsLoadAsTheLibrarysRecords is the config path of a TXT
// record: the text arrives unquoted, DNS only, with no WAF.
func TestTXTRecordsLoadAsTheLibrarysRecords(t *testing.T) {
	cf := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}\n", mailPolicy)

	c, err := load(t, cf, kernelServesA())
	require.NoError(t, err)

	recs := c.Zones["example.com"].RecordsLibrary("example.com")
	require.NotNil(t, recs)
	require.Len(t, recs.Entries, 2)

	assert.Equal(t, "example.com", recs.Entries[0].Name)
	assert.Equal(t, "TXT", recs.Entries[0].Type)
	assert.Equal(t, "v=spf1 -all", recs.Entries[0].Content, "the file's text is the library's, unquoted")
	assert.False(t, recs.Entries[0].Proxied)
	assert.Nil(t, recs.Entries[0].WAF)

	assert.Equal(t, "_dmarc.example.com", recs.Entries[1].Name)
	assert.Equal(t, "v=DMARC1; p=reject; sp=none;", recs.Entries[1].Content)
	assert.Equal(t, 300, recs.Entries[1].TTL)
}

// TestTXTIsNotAWAFHost keeps a TXT record out of a managed WAF's derived
// host list.
func TestTXTIsNotAWAFHost(t *testing.T) {
	cf := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}\n",
		mailPolicy+"    managed_waf: {enabled: true}\n")

	c, err := load(t, cf, kernelServesA())
	require.NoError(t, err)

	hosts := c.Zones["example.com"].WAFHosts
	assert.NotContains(t, hosts, "example.com", "a TXT name is never a WAF host")
	assert.NotContains(t, hosts, "_dmarc.example.com")
	assert.Contains(t, hosts, "a.example.com", "a proxied tunnel name is")
}

func TestTXTRefusalsAtLoad(t *testing.T) {
	zone := func(records string, extra ...string) string {
		block := "  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n    records:\n" + records
		for _, e := range extra {
			block += e
		}

		return replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}\n", block)
	}

	for name, tc := range map[string]struct {
		cf   string
		want string
	}{
		"proxied": {
			cf:   zone("      - {name: example.com, type: TXT, content: \"v=spf1 -all\", proxied: true}\n"),
			want: "a TXT record is DNS only",
		},
		"proxied under a derived WAF": {
			cf: zone("      - {name: example.com, type: TXT, content: \"v=spf1 -all\", proxied: true}\n",
				"    managed_waf: {enabled: true}\n"),
			want: "a TXT record is DNS only",
		},
		"waf opt-out": {
			cf:   zone("      - {name: example.com, type: TXT, content: \"v=spf1 -all\", waf: false, waf_reason: nope}\n"),
			want: "records[example.com]: a TXT record takes no WAF options",
		},
		"waf reason alone": {
			cf:   zone("      - {name: example.com, type: TXT, content: \"v=spf1 -all\", waf_reason: nope}\n"),
			want: "a TXT record takes no WAF options",
		},
		"quoted text": {
			cf:   zone("      - {name: example.com, type: TXT, content: '\"v=spf1 -all\"'}\n"),
			want: "without surrounding quotes",
		},
		"too long": {
			cf:   zone("      - {name: example.com, type: TXT, content: " + strings.Repeat("a", 256) + "}\n"),
			want: "at most 255",
		},
		"exact duplicate": {
			cf: zone("      - {name: example.com, type: TXT, content: \"v=spf1 -all\"}\n" +
				"      - {name: example.com, type: TXT, content: \"v=spf1 -all\"}\n"),
			want: "is listed twice",
		},
		"beside a record CNAME": {
			cf: zone("      - {name: b.example.com, type: CNAME, content: lb.example.net}\n" +
				"      - {name: b.example.com, type: TXT, content: \"v=spf1 -all\"}\n"),
			want: "a CNAME cannot share its name",
		},
		"on a tunnel host": {
			cf:   zone("      - {name: a.example.com, type: TXT, content: \"v=spf1 -all\"}\n"),
			want: "records[a.example.com]: tunnel kernel writes this host's CNAME (the edge catalog), and a CNAME cannot share its name with a TXT record",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.cf, kernelServesA())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestTXTBesideATunnelHostsParent: a TXT on a name the tunnel does not
// write is fine, even on the apex of a zone whose subdomain it serves.
func TestTXTBesideATunnelHostsParent(t *testing.T) {
	cf := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}\n",
		"  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n    records:\n"+
			"      - {name: example.com, type: TXT, content: \"v=spf1 -all\"}\n"+
			"      - {name: _acme.a.example.com, type: TXT, content: token}\n")

	_, err := load(t, cf, kernelServesA())
	require.NoError(t, err)
}

// TestFixedRedirectLoadsAsTheLibrarysRedirect is the config path of a
// redirect that sends every path to one URL.
func TestFixedRedirectLoadsAsTheLibrarysRedirect(t *testing.T) {
	cf := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}\n",
		"  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n    redirects:\n"+
			"      - {ref: regional_to_page, from: regional.example.com, to: https://site.example.net/regional, preserve_path: false}\n"+
			"      - {ref: www_to_apex, from: www.example.com, to: https://example.com}\n")

	c, err := load(t, cf, kernelServesA())
	require.NoError(t, err)

	r := c.Zones["example.com"].RedirectsLibrary("example.com")
	require.NotNil(t, r)
	require.Len(t, r.Entries, 2)

	require.NotNil(t, r.Entries[0].PreservePath)
	assert.False(t, *r.Entries[0].PreservePath)
	assert.False(t, r.Entries[0].PreservesPath())
	assert.Equal(t, "https://site.example.net/regional", r.Entries[0].To)

	assert.Nil(t, r.Entries[1].PreservePath, "absent is nil, which preserves the path")
	assert.True(t, r.Entries[1].PreservesPath())
}

func TestFixedRedirectRefusalsAtLoad(t *testing.T) {
	redirect := func(row string) string {
		return replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}\n",
			"  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n    redirects:\n      - "+row+"\n")
	}

	for name, tc := range map[string]struct {
		cf   string
		want string
	}{
		"path without preserve_path": {
			cf:   redirect("{ref: r, from: regional.example.com, to: https://site.example.net/regional}"),
			want: "redirects[r].to: \"https://site.example.net/regional\" has a path, and the request's path is appended to an origin",
		},
		"path with preserve_path true": {
			cf:   redirect("{ref: r, from: regional.example.com, to: https://site.example.net/regional, preserve_path: true}"),
			want: "set preserve_path: false",
		},
		"fixed with a query": {
			cf:   redirect("{ref: r, from: regional.example.com, to: 'https://site.example.net/regional?x=1', preserve_path: false}"),
			want: "no port, query or fragment",
		},
		"fixed with a trailing wildcard": {
			cf:   redirect("{ref: r, from: regional.example.com, to: 'https://site.example.net/regional/*', preserve_path: false}"),
			want: `no trailing "*"`,
		},
		"fixed over http": {
			cf:   redirect("{ref: r, from: regional.example.com, to: http://site.example.net/regional, preserve_path: false}"),
			want: "HTTPS",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.cf, kernelServesA())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
