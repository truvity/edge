package zone

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fixedRedirect sends every path of a regional host to one page of the
// canonical site, the shape a retired subdomain takes.
func fixedRedirect(to string) Redirect {
	keep := false

	return Redirect{Ref: "regional_to_page", From: "regional.example.com", To: to, PreservePath: &keep}
}

func fixedArgs(r ...Redirect) Args {
	return Args{ZoneID: "zone-1", Redirects: redirects(r...)}
}

// TestFixedRedirectRendersAStaticTarget pins the Cloudflare shape of a
// redirect that does not preserve the path: targetUrl.value is exactly To,
// and there is no expression concatenating the request's path.
func TestFixedRedirectRendersAStaticTarget(t *testing.T) {
	m := run(t, fixedArgs(fixedRedirect("https://site.example.net/regional")))

	rule := m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()[0].ObjectValue()
	assert.Equal(t, "regional_to_page", rule["ref"].StringValue())
	assert.Equal(t, `http.host eq "regional.example.com"`, rule["expression"].StringValue(),
		"the match is the same exact host, on every path")

	fromValue := rule["actionParameters"].ObjectValue()["fromValue"].ObjectValue()
	target := fromValue["targetUrl"].ObjectValue()
	assert.Equal(t, "https://site.example.net/regional", target["value"].StringValue())
	assert.False(t, target.HasValue("expression"), "a static target carries no expression")
	assert.Equal(t, 301.0, fromValue["statusCode"].NumberValue())
	assert.True(t, fromValue["preserveQueryString"].BoolValue(), "query preservation keeps its default")
}

// TestFixedRedirectKeepsItsOptions: status and the query string mean what
// they mean on a path-preserving redirect.
func TestFixedRedirectKeepsItsOptions(t *testing.T) {
	r := fixedRedirect("https://site.example.net/")
	drop := false
	r.PreserveQueryString = &drop
	r.Status = 302

	m := run(t, fixedArgs(r))

	fromValue := m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()[0].ObjectValue()["actionParameters"].
		ObjectValue()["fromValue"].ObjectValue()
	assert.Equal(t, "https://site.example.net/", fromValue["targetUrl"].ObjectValue()["value"].StringValue())
	assert.Equal(t, 302.0, fromValue["statusCode"].NumberValue())
	assert.False(t, fromValue["preserveQueryString"].BoolValue())
}

// TestPreservePathTrueIsTheDefault: an explicit true renders exactly what
// nil does.
func TestPreservePathTrueIsTheDefault(t *testing.T) {
	keep := true
	explicit := redirectArgs()
	explicit.Redirects.Entries[0].PreservePath = &keep

	a := run(t, redirectArgs()).res[rulesetType+"/redirect-rules-example"]
	b := run(t, explicit).res[rulesetType+"/redirect-rules-example"]
	assert.Equal(t, a, b)

	target := b["rules"].ArrayValue()[0].ObjectValue()["actionParameters"].ObjectValue()["fromValue"].
		ObjectValue()["targetUrl"].ObjectValue()
	assert.Equal(t, `concat("https://example.com", http.request.uri.path)`, target["expression"].StringValue())
	assert.False(t, target.HasValue("value"))
}

// TestProxiedFixedRedirectSourceWaitsForTheRuleset: the rule-before-proxy
// dependency does not depend on the target's shape.
func TestProxiedFixedRedirectSourceWaitsForTheRuleset(t *testing.T) {
	args := fixedArgs(fixedRedirect("https://site.example.net/regional"))
	args.Records = &Records{Zone: "example.com", Entries: []Record{
		{Name: "regional.example.com", Type: "CNAME", Content: "legacy.example.net", Proxied: true},
	}}

	_, deps := runWithDeps(t, args)
	assert.Equal(t, []string{"redirect-rules-example"}, deps[dnsRecordType+"::record-example-regional-example-com-cname"])
}

func TestFixedRedirectValidation(t *testing.T) {
	keep := true

	cases := []struct {
		name string
		args Args
		want string
	}{
		{"page", fixedArgs(fixedRedirect("https://site.example.net/regional")), ""},
		{"nested page", fixedArgs(fixedRedirect("https://site.example.net/a/b-c_d.html")), ""},
		{"root", fixedArgs(fixedRedirect("https://site.example.net/")), ""},
		{"origin alone", fixedArgs(fixedRedirect("https://site.example.net")), ""},
		{"percent escape", fixedArgs(fixedRedirect("https://site.example.net/a%20b")), ""},
		{"http", fixedArgs(fixedRedirect("http://site.example.net/regional")), "HTTPS"},
		{"query", fixedArgs(fixedRedirect("https://site.example.net/regional?x=1")), "no port, query or fragment"},
		{"empty query", fixedArgs(fixedRedirect("https://site.example.net/regional?")), "no port, query or fragment"},
		{"fragment", fixedArgs(fixedRedirect("https://site.example.net/regional#top")), "no port, query or fragment"},
		{"port", fixedArgs(fixedRedirect("https://site.example.net:8443/regional")), "no port, query or fragment"},
		{"user", fixedArgs(fixedRedirect("https://user@site.example.net/regional")), "no port, query or fragment"},
		{"trailing wildcard", fixedArgs(fixedRedirect("https://site.example.net/regional/*")), `no trailing "*"`},
		{"space", fixedArgs(fixedRedirect("https://site.example.net/a b")), "URL path characters"},
		{"quote", fixedArgs(fixedRedirect(`https://site.example.net/a"b`)), "URL path characters"},
		{"bad escape", fixedArgs(fixedRedirect("https://site.example.net/a%zz")), "not a URL"},
		{"upper-case host", fixedArgs(fixedRedirect("https://SITE.example.net/regional")), "lower-case hostname"},
		{"self loop", fixedArgs(fixedRedirect("https://regional.example.com/landing")), "redirect loop"},
		{"path while preserving", fixedArgs(Redirect{
			Ref: "regional_to_page", From: "regional.example.com", To: "https://site.example.net/regional", PreservePath: &keep,
		}), "set preservePath false"},
		{"path by default", fixedArgs(Redirect{
			Ref: "regional_to_page", From: "regional.example.com", To: "https://site.example.net/regional",
		}), "set preservePath false"},
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
