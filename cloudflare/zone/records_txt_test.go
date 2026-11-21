package zone

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// txtRecords is the mail-policy shape a zone that sends no mail declares:
// SPF and a site verification on the apex beside its address, and DMARC on
// _dmarc.
func txtRecords() *Records {
	return &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "example.com", Type: "A", Content: "192.0.2.1"},
			{Name: "example.com", Type: "TXT", Content: "v=spf1 -all"},
			{Name: "example.com", Type: "TXT", Content: "google-site-verification=abc123", TTL: 300},
			{Name: "_dmarc.example.com", Type: "TXT", Content: "v=DMARC1; p=reject; sp=none;"},
		},
	}
}

func withTXT(mutate func(*Records)) Args {
	r := txtRecords()
	mutate(r)

	return Args{ZoneID: "zone-1", Records: r}
}

// TestTXTRendersQuotedContent pins the child names (a hash of the declared
// text, never the text) and the content Cloudflare is given: the text in
// double quotes, as the API stores TXT content and returns it, so the
// provider reads back what it sent.
func TestTXTRendersQuotedContent(t *testing.T) {
	m := run(t, Args{ZoneID: "zone-1", Records: txtRecords()})

	for name, want := range map[string]struct {
		host, content string
		ttl           float64
	}{
		// sha256("v=spf1 -all")[:8]
		"record-example-example-com-txt-f90139dd": {"example.com", `"v=spf1 -all"`, 1},
		// sha256("google-site-verification=abc123")[:8]
		"record-example-example-com-txt-b0d609f4": {"example.com", `"google-site-verification=abc123"`, 300},
		// sha256("v=DMARC1; p=reject; sp=none;")[:8]
		"record-example-_dmarc-example-com-txt-f6247928": {"_dmarc.example.com", `"v=DMARC1; p=reject; sp=none;"`, 1},
	} {
		rec, ok := m.res[dnsRecordType+"/"+name]
		require.True(t, ok, "child name is a contract consumers alias onto: %s", name)

		assert.Equal(t, "zone-1", rec["zoneId"].StringValue())
		assert.Equal(t, want.host, rec["name"].StringValue())
		assert.Equal(t, "TXT", rec["type"].StringValue())
		assert.Equal(t, want.content, rec["content"].StringValue())
		assert.False(t, rec["proxied"].BoolValue(), "a TXT record is DNS only")
		assert.Equal(t, want.ttl, rec["ttl"].NumberValue())
	}

	apex := m.res[dnsRecordType+"/record-example-example-com-a-192-0-2-1"]
	require.NotNil(t, apex, "the apex address keeps its name beside the TXT records")
	assert.Equal(t, "192.0.2.1", apex["content"].StringValue(), "only TXT content is quoted")
}

func TestRecordContentQuotesOnlyTXT(t *testing.T) {
	assert.Equal(t, `"v=spf1 -all"`, RecordContent(&Record{Type: "TXT", Content: "v=spf1 -all"}))
	assert.Equal(t, "192.0.2.1", RecordContent(&Record{Type: "A", Content: "192.0.2.1"}))
	assert.Equal(t, "lb.example.net", RecordContent(&Record{Type: "CNAME", Content: "lb.example.net"}))
}

// TestTXTResourceNamesIgnoreInputOrder keeps two TXT records on one name
// distinct and their identities stable when a config file reorders them.
func TestTXTResourceNamesIgnoreInputOrder(t *testing.T) {
	names := func(entries []Record) []string {
		var out []string
		for k := range run(t, Args{ZoneID: "zone-1", Records: &Records{Zone: "example.com", Entries: entries}}).res {
			if strings.HasPrefix(k, dnsRecordType+"/") {
				out = append(out, k)
			}
		}

		sort.Strings(out)

		return out
	}

	regular := txtRecords().Entries
	reversed := []Record{regular[3], regular[2], regular[1], regular[0]}

	got := names(regular)
	assert.Len(t, got, 4, "two TXT records on the apex are two resources")
	assert.Equal(t, got, names(reversed))

	spf := RecordName("example", &regular[1])
	verification := RecordName("example", &regular[2])
	assert.NotEqual(t, spf, verification)
	assert.Equal(t, spf, RecordName("example", &Record{Name: "example.com", Type: "TXT", Content: "v=spf1 -all", TTL: 600}),
		"the name follows the text, not the TTL or the comment")
}

func TestValidateTXTRecords(t *testing.T) {
	cases := []struct {
		name string
		args Args
		want string
	}{
		{"spf, verification and dmarc ok", withTXT(func(*Records) {}), ""},
		{"wildcard ok", withTXT(func(r *Records) { r.Entries[1].Name = "*.example.com" }), ""},
		{"255 characters ok", withTXT(func(r *Records) { r.Entries[1].Content = strings.Repeat("a", 255) }), ""},
		{"ttl 60 ok", withTXT(func(r *Records) { r.Entries[1].TTL = 60 }), ""},
		{"proxied", withTXT(func(r *Records) { r.Entries[1].Proxied = true }), "a TXT record is DNS only"},
		{"waf", withTXT(func(r *Records) { no := false; r.Entries[1].WAF = &no }), "a TXT record is not in the managed WAF"},
		{"empty", withTXT(func(r *Records) { r.Entries[1].Content = "" }), "content is empty"},
		{"256 characters", withTXT(func(r *Records) { r.Entries[1].Content = strings.Repeat("a", 256) }), "is 256 characters; at most 255"},
		{"quoted", withTXT(func(r *Records) { r.Entries[1].Content = `"v=spf1 -all"` }), "without surrounding quotes"},
		{"inner quote", withTXT(func(r *Records) { r.Entries[1].Content = `v=spf1 "x" -all` }), "double quote or a backslash"},
		{"backslash", withTXT(func(r *Records) { r.Entries[1].Content = `v=spf1 \-all` }), "double quote or a backslash"},
		{"tab", withTXT(func(r *Records) { r.Entries[1].Content = "v=spf1\t-all" }), "printable ASCII"},
		{"newline", withTXT(func(r *Records) { r.Entries[1].Content = "v=spf1\n-all" }), "printable ASCII"},
		{"non-ascii", withTXT(func(r *Records) { r.Entries[1].Content = "v=spf1 -all é" }), "printable ASCII"},
		{"ttl too short", withTXT(func(r *Records) { r.Entries[1].TTL = 30 }), "ttl 30 must be"},
		{"ttl too long", withTXT(func(r *Records) { r.Entries[1].TTL = 172800 }), "ttl 172800 must be"},
		{"exact duplicate", withTXT(func(r *Records) { r.Entries = append(r.Entries, r.Entries[1]) }), `"v=spf1 -all" is listed twice`},
		{"same text on another name ok", withTXT(func(r *Records) {
			r.Entries = append(r.Entries, Record{Name: "mail.example.com", Type: "TXT", Content: "v=spf1 -all"})
		}), ""},
		{"beside a CNAME", withTXT(func(r *Records) {
			r.Entries = append(r.Entries, Record{Name: "_dmarc.example.com", Type: "CNAME", Content: "dmarc.example.net"})
		}), "a CNAME cannot share its name"},
		{"after a CNAME", withTXT(func(r *Records) {
			r.Entries = append([]Record{{Name: "_dmarc.example.com", Type: "CNAME", Content: "dmarc.example.net"}}, r.Entries...)
		}), "a CNAME cannot share its name"},
		{"beside an AAAA ok", withTXT(func(r *Records) {
			r.Entries = append(r.Entries, Record{Name: "example.com", Type: "AAAA", Content: "2001:db8::1"})
		}), ""},
		{"at an NS owner", withTXT(func(r *Records) {
			r.Entries = append(r.Entries,
				Record{Name: "team.example.com", Type: "NS", Content: "ns1.nameservers.example", TTL: 300},
				Record{Name: "team.example.com", Type: "TXT", Content: "v=spf1 -all"})
		}), "an NS delegation cannot share its name"},
		{"before an NS owner", withTXT(func(r *Records) {
			r.Entries = append(r.Entries,
				Record{Name: "team.example.com", Type: "TXT", Content: "v=spf1 -all"},
				Record{Name: "team.example.com", Type: "NS", Content: "ns1.nameservers.example", TTL: 300})
		}), "an NS delegation cannot share its name"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.args.Validate()
			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestTXTIsNotAWAFHost keeps a TXT record out of the derived WAF's
// vocabulary even if a caller inspects the record's coverage directly.
func TestTXTIsNotAWAFHost(t *testing.T) {
	assert.False(t, txtRecords().Entries[1].WAFCovered())
}
