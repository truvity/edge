package rules_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/rules"
	"github.com/truvity/edge/v4/cloudflare/zonespec"
)

const domainName = "access.example.com"

// TestNoNonStdlibImports guards this package's reason to exist: a caller
// must be able to import rules with no Pulumi or Cloudflare SDK riding along.
func TestNoNonStdlibImports(t *testing.T) {
	const pkg = "github.com/truvity/edge/v4/cloudflare/rules"

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	require.NoError(t, err, "go list -deps")

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || line == pkg {
			continue
		}

		root, _, _ := strings.Cut(line, "/")
		if line == "github.com/truvity/edge/v4/cloudflare/zonespec" || line == "github.com/truvity/edge/v4/cloudflare/rulespec" {
			continue
		}

		assert.NotContains(t, root, ".", "cloudflare/rules must depend on the standard library, zonespec and rulespec only, found %q", line)
	}
}

func refs(rs []rules.Rule) []string {
	out := make([]string, len(rs))
	for i := range rs {
		out[i] = rs[i].Ref
	}

	return out
}

func cacheContribution(source string, order int, claim string, rs ...string) rules.Contribution {
	c := rules.Contribution{Source: source, Phase: rules.PhaseCacheSettings, Order: order, Claim: claim}
	for _, r := range rs {
		c.Rules = append(c.Rules, rules.Rule{Ref: r, Action: rules.ActionSetCacheSettings, Expression: "true"})
	}

	return c
}

func TestAssembleOrdering(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []rules.Contribution
		want []string
	}{
		"by order": {
			in:   []rules.Contribution{cacheContribution("p", 900, "", "paths"), cacheContribution("h", 100, "", "hosts")},
			want: []string{"hosts", "cache-bypass", "paths"},
		},
		"ties by source": {
			in:   []rules.Contribution{cacheContribution("b", 100, "", "rb"), cacheContribution("a", 100, "", "ra")},
			want: []string{"ra", "rb", "cache-bypass"},
		},
		"rules keep their order inside a contribution": {
			in:   []rules.Contribution{cacheContribution("a", 100, "", "r2", "r1")},
			want: []string{"r2", "r1", "cache-bypass"},
		},
		"order 500 ties with the bypass by source": {
			in:   []rules.Contribution{cacheContribution("zzz", 500, "", "late"), cacheContribution("a", 500, "", "early")},
			want: []string{"early", "cache-bypass", "late"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := rules.Assemble(tc.in)
			require.NoError(t, err)
			assert.Equal(t, tc.want, refs(got[rules.PhaseCacheSettings]))
		})
	}
}

func TestAssembleDoesNotTouchItsInputAndIgnoresInputOrder(t *testing.T) {
	in := []rules.Contribution{cacheContribution("p", 900, "", "paths"), cacheContribution("h", 100, "", "hosts")}
	_, err := rules.Assemble(in)
	require.NoError(t, err)
	assert.Equal(t, "p", in[0].Source)
}

func TestAssembleOnlyPhasesWithContributions(t *testing.T) {
	got, err := rules.Assemble([]rules.Contribution{{Source: "x", Phase: rules.PhaseRateLimit,
		Rules: []rules.Rule{{Ref: "r", Action: "block", Expression: "true"}}}})
	require.NoError(t, err)
	assert.Len(t, got, 1)
	assert.NotContains(t, got, rules.PhaseCacheSettings, "no bypass without a cache contribution")

	empty, err := rules.Assemble(nil)
	require.NoError(t, err)
	assert.Empty(t, empty)
}

func TestAssembleRefuses(t *testing.T) {
	rule := func(ref string) []rules.Rule {
		return []rules.Rule{{Ref: ref, Action: "block", Expression: "true"}}
	}

	for name, tc := range map[string]struct {
		in   []rules.Contribution
		want string
	}{
		"duplicate ref in a phase": {
			in: []rules.Contribution{
				{Source: "a", Phase: rules.PhaseFirewallCustom, Rules: rule("r")},
				{Source: "b", Phase: rules.PhaseFirewallCustom, Order: 1, Rules: rule("r")},
			},
			want: `rule ref "r" in http_request_firewall_custom is already written by "a"`,
		},
		"duplicate ref inside one contribution": {
			in:   []rules.Contribution{{Source: "a", Phase: rules.PhaseFirewallCustom, Rules: append(rule("r"), rule("r")...)}},
			want: `already written by "a"`,
		},
		"claim outside the cache phase": {
			in:   []rules.Contribution{{Source: "a", Phase: rules.PhaseFirewallCustom, Claim: "(true)", Rules: rule("r")}},
			want: "a claim belongs to the http_request_cache_settings phase",
		},
		"unparenthesised claim": {
			in:   []rules.Contribution{cacheContribution("a", 1, "http.host eq \"x\"", "r")},
			want: "parenthesised",
		},
		"the bypass ref is the assembler's": {
			in:   []rules.Contribution{cacheContribution("a", 1, "", rules.BypassRef)},
			want: "reserved for the assembler",
		},
		"unknown phase": {
			in:   []rules.Contribution{{Source: "a", Phase: "http_nope", Rules: rule("r")}},
			want: `unknown phase "http_nope"`,
		},
		"rule without a ref": {
			in:   []rules.Contribution{{Source: "a", Phase: rules.PhaseRateLimit, Rules: rule("")}},
			want: "no ref",
		},
		"no source": {
			in:   []rules.Contribution{{Phase: rules.PhaseRateLimit, Rules: rule("r")}},
			want: "has no source",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := rules.Assemble(tc.in)
			require.Error(t, err)
			assert.Nil(t, got)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestBypassFold(t *testing.T) {
	for name, tc := range map[string]struct {
		claims []string
		want   string
	}{
		"no claims":    {nil, "true"},
		"one claim":    {[]string{"(a)"}, "not (a)"},
		"two claims":   {[]string{"(a)", "(b)"}, "(not (a)) and not (b)"},
		"three claims": {[]string{"(a)", "(b)", "(c)"}, "((not (a)) and not (b)) and not (c)"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := rules.Assemble(func() []rules.Contribution {
				var in []rules.Contribution
				for i, c := range tc.claims {
					in = append(in, cacheContribution(fmt.Sprintf("s%d", i), i, c, fmt.Sprintf("r%d", i)))
				}

				if len(in) == 0 {
					in = append(in, cacheContribution("s", 0, ""))
				}

				return in
			}())
			require.NoError(t, err)

			var bypass rules.Rule

			for _, r := range got[rules.PhaseCacheSettings] {
				if r.Ref == rules.BypassRef {
					bypass = r
				}
			}

			assert.Equal(t, tc.want, bypass.Expression)
			assert.Equal(t, rules.ActionSetCacheSettings, bypass.Action)
			assert.False(t, bypass.Params.Cache.Cache)
		})
	}
}

func TestClaimsFoldInOrderNotInputOrder(t *testing.T) {
	got, err := rules.Assemble([]rules.Contribution{
		cacheContribution("late", 900, "(b)", "rb"),
		cacheContribution("early", 100, "(a)", "ra"),
	})
	require.NoError(t, err)
	assert.Equal(t, "(not (a)) and not (b)", got[rules.PhaseCacheSettings][1].Expression)
}

// The estate's zone today: the library's cache ruleset for the zone's
// cacheable hosts, whose catch-all bypass a transformation then narrows with
// one sluis public-documents rule appended. The expected values are computed
// the way those two programs compute them (zone.cacheRules, then the
// estate's narrowBypass and publicCacheRuleArgs), written out here rather
// than shared, so a change on either side fails this test.
func TestAssembleReproducesTheLiveCacheRuleset(t *testing.T) {
	hosts := []string{"www.example.com", "docs.example.com", "*.assets.example.com"}

	// zone.cacheRules: hosts rule, then "not (<hosts>)".
	cacheable := zonespec.HostExpression(hosts)
	libraryBypass := "not (" + cacheable + ")"

	paths, err := rules.PublicPathsCache(domainName, rules.PublicPathsArgs{})
	require.NoError(t, err)

	// narrowBypass: "(<old>) and not <rule expression>".
	liveBypass := fmt.Sprintf("(%s) and not %s", libraryBypass, paths.Expression)

	got, err := rules.Assemble([]rules.Contribution{
		paths.Contribution(),
		rules.CacheHosts(hosts),
	})
	require.NoError(t, err)

	cache := got[rules.PhaseCacheSettings]
	require.Equal(t, []string{"cacheable-hosts", "cache-bypass", "public-paths-" + domainName}, refs(cache))

	assert.Equal(t, cacheable, cache[0].Expression)
	assert.Equal(t, `http.host in {"www.example.com" "docs.example.com"} or ends_with(http.host, ".assets.example.com")`, cache[0].Expression)
	assert.Equal(t, liveBypass, cache[1].Expression)
	assert.Equal(t,
		`(not (http.host in {"www.example.com" "docs.example.com"} or ends_with(http.host, ".assets.example.com"))) and not `+
			`(http.host eq "access.example.com" and http.request.method in {"GET" "HEAD"} and `+
			`http.request.uri.path in {"/.well-known/oauth-authorization-server" "/.well-known/openid-configuration" "/keys"} `+
			`and not any(lower(http.request.headers.names[*])[*] eq "origin"))`,
		cache[1].Expression)
	assert.Equal(t, "Not a cacheable hostname", cache[1].Description)
	assert.Equal(t, paths.Expression, cache[2].Expression)

	// Settings of the three rules, as the live ones send them.
	assert.Equal(t, &rules.CacheParams{Cache: true,
		EdgeTTL: &rules.TTL{Mode: "bypass_by_default"}, BrowserTTL: &rules.TTL{Mode: "respect_origin"}}, cache[0].Params.Cache)
	assert.Equal(t, &rules.CacheParams{Cache: false}, cache[1].Params.Cache)
	assert.Equal(t, &rules.CacheParams{Cache: true,
		EdgeTTL: &rules.TTL{Mode: "override_origin", Default: 300}, BrowserTTL: &rules.TTL{Mode: "respect_origin"}}, cache[2].Params.Cache)
}

func TestNoHostsIsTheBypassAlone(t *testing.T) {
	got, err := rules.Assemble([]rules.Contribution{rules.CacheHosts(nil)})
	require.NoError(t, err)
	require.Len(t, got[rules.PhaseCacheSettings], 1)
	assert.Equal(t, "true", got[rules.PhaseCacheSettings][0].Expression)
}

// The default rule caches the three public documents of the domain, for five
// minutes, for GET and HEAD without an Origin header, and nothing else.
func TestPublicPathsCacheDefaults(t *testing.T) {
	r, err := rules.PublicPathsCache(domainName, rules.PublicPathsArgs{})
	require.NoError(t, err)

	want := `(http.host eq "access.example.com" and http.request.method in {"GET" "HEAD"} and ` +
		`http.request.uri.path in {"/.well-known/oauth-authorization-server" "/.well-known/openid-configuration" "/keys"} ` +
		`and not any(lower(http.request.headers.names[*])[*] eq "origin"))`
	assert.Equal(t, want, r.Expression)
	assert.Equal(t, 300, r.EdgeTTLSeconds)
	assert.Equal(t, domainName, r.Host)
	assert.Len(t, r.Paths, len(rules.DefaultPublicCachePaths))
	assert.Contains(t, r.Description, domainName)
	assert.Contains(t, r.Description, "Origin")
}

// A browser request carries an Origin, which the service echoes in
// Access-Control-Allow-Origin: the rule must leave it uncached.
func TestPublicPathsCacheNeverCachesARequestWithAnOrigin(t *testing.T) {
	r, err := rules.PublicPathsCache(domainName, rules.PublicPathsArgs{Paths: []string{"/keys"}})
	require.NoError(t, err)
	assert.True(t, strings.HasSuffix(r.Expression, `and not any(lower(http.request.headers.names[*])[*] eq "origin"))`))
}

func TestPublicPathsCacheOptions(t *testing.T) {
	r, err := rules.PublicPathsCache(domainName, rules.PublicPathsArgs{TTL: 30 * time.Second, Paths: []string{"/keys"}})
	require.NoError(t, err)
	assert.Equal(t, 30, r.EdgeTTLSeconds)
	assert.Equal(t, []string{"/keys"}, r.Paths)
	assert.Contains(t, r.Expression, `{"/keys"}`)

	// The caller's slice is not reordered.
	in := []string{"/keys", "/.well-known/openid-configuration"}
	_, err = rules.PublicPathsCache(domainName, rules.PublicPathsArgs{Paths: in})
	require.NoError(t, err)
	assert.Equal(t, "/keys", in[0])
}

func TestPublicPathsCacheRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		host string
		args rules.PublicPathsArgs
		want string
	}{
		"quote in host":      {`a".example.test`, rules.PublicPathsArgs{}, "not a domain name"},
		"bare host":          {"localhost", rules.PublicPathsArgs{}, "not a domain name"},
		"TTL too long":       {domainName, rules.PublicPathsArgs{TTL: 2 * time.Hour}, "between 1s and"},
		"TTL under a second": {domainName, rules.PublicPathsArgs{TTL: time.Millisecond}, "between 1s and"},
		"TTL not whole":      {domainName, rules.PublicPathsArgs{TTL: 1500 * time.Millisecond}, "whole seconds"},
		"relative path":      {domainName, rules.PublicPathsArgs{Paths: []string{"keys"}}, "must start with /"},
		"quote in path":      {domainName, rules.PublicPathsArgs{Paths: []string{`/keys" or true`}}, "URL path characters"},
		"query in path":      {domainName, rules.PublicPathsArgs{Paths: []string{"/keys?x=1"}}, "URL path characters"},
		"duplicate path":     {domainName, rules.PublicPathsArgs{Paths: []string{"/keys", "/keys"}}, "listed twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rules.PublicPathsCache(tc.host, tc.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestStaticSiteCache(t *testing.T) {
	c, err := rules.StaticSiteCache("www.example.com", rules.StaticSiteArgs{HashedPrefix: "/assets/"})
	require.NoError(t, err)
	assert.Equal(t, `(http.host eq "www.example.com")`, c.Claim)
	require.Len(t, c.Rules, 2)

	hashed, rest := c.Rules[0], c.Rules[1]
	assert.Equal(t, `http.host eq "www.example.com" and starts_with(http.request.uri.path, "/assets/")`, hashed.Expression)
	assert.Equal(t, &rules.TTL{Mode: "override_origin", Default: 365 * 24 * 3600}, hashed.Params.Cache.EdgeTTL)
	assert.Equal(t, `http.host eq "www.example.com" and not starts_with(http.request.uri.path, "/assets/")`, rest.Expression)
	assert.Equal(t, &rules.TTL{Mode: "respect_origin"}, rest.Params.Cache.EdgeTTL, "the long TTL is only under the hashed prefix")
	assert.True(t, rest.Params.Cache.RespectStrongETags)

	// Both rules together partition the host, and the contribution assembles.
	got, err := rules.Assemble([]rules.Contribution{rules.CacheHosts(nil), c})
	require.NoError(t, err)
	assert.Equal(t, []string{"cache-bypass", "static-hashed-www.example.com", "static-site-www.example.com"}, refs(got[rules.PhaseCacheSettings]))
	assert.Equal(t, `not (http.host eq "www.example.com")`, got[rules.PhaseCacheSettings][0].Expression)
}

func TestStaticSiteCacheOptionsAndRefusals(t *testing.T) {
	c, err := rules.StaticSiteCache("www.example.com", rules.StaticSiteArgs{HTMLTTL: time.Minute})
	require.NoError(t, err)
	require.Len(t, c.Rules, 1, "no hashed prefix, no hashed rule")
	assert.Equal(t, &rules.TTL{Mode: "override_origin", Default: 60}, c.Rules[0].Params.Cache.EdgeTTL)

	for name, args := range map[string]rules.StaticSiteArgs{
		"prefix without trailing slash": {HashedPrefix: "/assets"},
		"prefix with a quote":           {HashedPrefix: `/a"/`},
		"edge TTL too long":             {EdgeTTL: 400 * 24 * time.Hour},
		"html TTL not whole":            {HTMLTTL: 1500 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rules.StaticSiteCache("www.example.com", args)
			assert.Error(t, err)
		})
	}

	_, err = rules.StaticSiteCache("localhost", rules.StaticSiteArgs{})
	assert.Error(t, err)
}

func TestRateLimits(t *testing.T) {
	row := zonespec.RateLimitRule{
		Name: "login", Endpoints: []zonespec.Endpoint{{Host: "app.example.com", Path: "/login"}},
		Period: 60, Requests: 10, MitigationTimeout: 600, Action: "block",
	}

	c, err := rules.RateLimits(zonespec.PlanPro, []zonespec.RateLimitRule{row})
	require.NoError(t, err)
	require.Len(t, c.Rules, 1)

	r := c.Rules[0]
	assert.Equal(t, rules.PhaseRateLimit, c.Phase)
	assert.Equal(t, "block", r.Action)
	assert.Equal(t, `(http.host eq "app.example.com" and http.request.uri.path eq "/login")`, r.Expression)
	assert.Equal(t, &rules.RateLimit{Characteristics: []string{"cf.colo.id", "ip.src"}, Period: 60, Requests: 10, MitigationTimeout: 600}, r.RateLimit)

	// zonespec's plan validation is the contribution's validation.
	row.Period = 120
	_, err = rules.RateLimits(zonespec.PlanPro, []zonespec.RateLimitRule{row})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "period 120 is not available on the pro plan")

	_, err = rules.RateLimits("enterprise", nil)
	assert.Error(t, err)

	// Free allows one rule; the empty block is a policy (an empty ruleset).
	empty, err := rules.RateLimits(zonespec.PlanFree, nil)
	require.NoError(t, err)
	assert.Empty(t, empty.Rules)
}

func TestTrustedClientsAndManagedWAF(t *testing.T) {
	c, err := rules.TrustedClients(&zonespec.TrustedClients{
		Zone: "example.com", Hosts: []string{"b.example.com", "a.example.com"}, Ranges: []string{"192.0.2.0/24"},
	})
	require.NoError(t, err)
	require.Len(t, c.Rules, 1)
	assert.Equal(t, rules.PhaseFirewallCustom, c.Phase)
	assert.Equal(t, `(http.host in {"a.example.com" "b.example.com"}) and (ip.src in {192.0.2.0/24})`, c.Rules[0].Expression)
	assert.Equal(t, "skip", c.Rules[0].Action)
	assert.True(t, c.Rules[0].Logging)
	assert.Empty(t, c.Rules[0].Params.Skip.Ruleset)
	assert.Equal(t, []string{"http_ratelimit", "http_request_sbfm"}, c.Rules[0].Params.Skip.Phases)
	assert.Equal(t, []string{"bic", "hot", "rateLimit", "securityLevel", "uaBlock"}, c.Rules[0].Params.Skip.Products)
	assert.Equal(t, "Trusted clients: skip bot and rate-limit checks", c.Rules[0].Description)

	none, err := rules.TrustedClients(nil)
	require.NoError(t, err)
	assert.Empty(t, none.Rules)

	_, err = rules.TrustedClients(&zonespec.TrustedClients{Zone: "example.com", Hosts: []string{"a.example.com"}})
	assert.Error(t, err)

	w, err := rules.ManagedWAF(&zonespec.ManagedWAF{Zone: "example.com", Hosts: []string{"b.example.com", "a.example.com"}})
	require.NoError(t, err)
	assert.Equal(t, rules.PhaseFirewallManaged, w.Phase)
	assert.Equal(t, `http.host in {"a.example.com" "b.example.com"}`, w.Rules[0].Expression)
	assert.Equal(t, "execute", w.Rules[0].Action)
	assert.Equal(t, zonespec.CloudflareManagedRulesetID, w.Rules[0].Params.Execute.ID)

	whole, err := rules.ManagedWAF(&zonespec.ManagedWAF{WholeZone: true})
	require.NoError(t, err)
	assert.Equal(t, "true", whole.Rules[0].Expression)

	_, err = rules.ManagedWAF(&zonespec.ManagedWAF{})
	assert.Error(t, err)
}

func TestTrustedClientsDefaultSkipLeavesTheManagedWAF(t *testing.T) {
	base := zonespec.TrustedClients{Zone: "example.com", Hosts: []string{"a.example.com"}, Ranges: []string{"192.0.2.0/24"}}

	c, err := rules.TrustedClients(&base)
	require.NoError(t, err)

	skip := c.Rules[0].Params.Skip
	assert.NotContains(t, skip.Phases, "http_request_firewall_managed")
	assert.NotContains(t, skip.Products, "waf")
	assert.NotContains(t, skip.Products, "zoneLockdown")
	assert.Empty(t, skip.Ruleset)

	// The old behaviour is a named opt-in.
	legacy := base
	legacy.Skip = zonespec.LegacyTrustedSkip()
	c, err = rules.TrustedClients(&legacy)
	require.NoError(t, err)
	assert.Equal(t, "current", c.Rules[0].Params.Skip.Ruleset)
	assert.Equal(t, []string{"http_ratelimit", "http_request_firewall_managed", "http_request_sbfm"}, c.Rules[0].Params.Skip.Phases)
	assert.Equal(t, []string{"bic", "hot", "rateLimit", "securityLevel", "uaBlock", "waf", "zoneLockdown"}, c.Rules[0].Params.Skip.Products)

	// Only the managed phase added back.
	managed := base
	managed.Skip = append(zonespec.DefaultTrustedSkip(), "http_request_firewall_managed")
	c, err = rules.TrustedClients(&managed)
	require.NoError(t, err)
	assert.Contains(t, c.Rules[0].Params.Skip.Phases, "http_request_firewall_managed")

	for name, skip := range map[string][]string{"unknown": {"http_request_waf"}, "twice": {"bic", "bic"}} {
		bad := base
		bad.Skip = skip
		_, err = rules.TrustedClients(&bad)
		assert.Error(t, err, name)
	}
}

func TestSSLMode(t *testing.T) {
	c, err := rules.SSLMode("app.example.com", "strict", "ssl-app")
	require.NoError(t, err)
	assert.Equal(t, rules.PhaseConfigSettings, c.Phase)
	assert.Equal(t, rules.Rule{
		Ref: "ssl-app", Description: "app.example.com: SSL mode strict", Action: "set_config",
		Expression: `http.host eq "app.example.com"`, Params: rules.Params{Config: &rules.ConfigParams{SSL: "strict"}},
	}, c.Rules[0])

	for name, args := range map[string][3]string{
		"bad mode": {"app.example.com", "origin_pull", "r"}, "empty mode": {"app.example.com", "", "r"},
		"bad host": {"localhost", "full", "r"}, "no ref": {"app.example.com", "full", ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rules.SSLMode(args[0], args[1], args[2])
			assert.Error(t, err)
		})
	}
}

func TestRateLimitRefsAreSlugs(t *testing.T) {
	row := func(name string) zonespec.RateLimitRule {
		return zonespec.RateLimitRule{
			Name: name, Endpoints: []zonespec.Endpoint{{Host: "a.example.com", Path: "/x"}},
			Period: 10, Requests: 5, MitigationTimeout: 10, Action: "block", Characteristics: []string{"ip.src"},
		}
	}

	c, err := rules.RateLimits("pro", []zonespec.RateLimitRule{row("Browser telemetry ingest"), row("Machine_ingest!!")})
	require.NoError(t, err)
	assert.Equal(t, "ratelimit-browser-telemetry-ingest", c.Rules[0].Ref)
	assert.Equal(t, "ratelimit-machine-ingest", c.Rules[1].Ref)
	assert.Equal(t, "Browser telemetry ingest", c.Rules[0].Description, "the description keeps the row's name")

	_, err = rules.RateLimits("pro", []zonespec.RateLimitRule{row("Machine ingest"), row("machine-ingest")})
	require.ErrorContains(t, err, `"Machine ingest" and "machine-ingest" both make the rule ref "ratelimit-machine-ingest"`)

	_, err = rules.RateLimits("pro", []zonespec.RateLimitRule{row("!!!")})
	require.ErrorContains(t, err, "no letter or digit")
}

func TestRedirectsContribution(t *testing.T) {
	keep := true
	c, err := rules.Redirects(&zonespec.Redirects{
		Zone: "example.com",
		Entries: []zonespec.Redirect{
			{Ref: "z_last", From: "z.example.com", To: "https://example.com"},
			{
				Ref: "www_to_apex", From: "www.example.com", To: "https://example.com",
				Status: 308, PreserveQueryString: &keep, Disabled: true,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, rules.PhaseDynamicRedirect, c.Phase)
	assert.Equal(t, "redirects", c.Source)
	require.Len(t, c.Rules, 2, "entries are sorted by source host, not authoring order")

	first := c.Rules[0]
	assert.Equal(t, "www_to_apex", first.Ref)
	assert.Equal(t, "redirect", first.Action)
	assert.Equal(t, `http.host eq "www.example.com"`, first.Expression)
	assert.True(t, first.Disabled)
	require.NotNil(t, first.Params.Redirect)
	assert.Equal(t, `concat("https://example.com", http.request.uri.path)`, first.Params.Redirect.TargetURL)
	assert.Equal(t, 308, first.Params.Redirect.StatusCode)
	assert.True(t, first.Params.Redirect.PreserveQueryString)

	assert.Equal(t, "z_last", c.Rules[1].Ref)
	assert.Equal(t, 301, c.Rules[1].Params.Redirect.StatusCode, "301 is the default")
	assert.True(t, c.Rules[1].Params.Redirect.PreserveQueryString, "query preservation is the default")

	empty, err := rules.Redirects(nil)
	require.NoError(t, err)
	assert.Empty(t, empty.Rules)
	assert.Equal(t, rules.PhaseDynamicRedirect, empty.Phase)

	_, err = rules.Redirects(&zonespec.Redirects{
		Zone:    "example.com",
		Entries: []zonespec.Redirect{{Ref: "www_to_apex", From: "www.example.com", To: "http://example.com"}},
	})
	require.ErrorContains(t, err, "HTTPS")
}

func TestAssembleKeepsTheDynamicRedirectPhase(t *testing.T) {
	c, err := rules.Redirects(&zonespec.Redirects{
		Zone:    "example.com",
		Entries: []zonespec.Redirect{{Ref: "www_to_apex", From: "www.example.com", To: "https://example.com"}},
	})
	require.NoError(t, err)

	got, err := rules.Assemble([]rules.Contribution{c})
	require.NoError(t, err)
	assert.Contains(t, got, rules.PhaseDynamicRedirect)
	assert.NotContains(t, got, rules.PhaseCacheSettings, "no unrelated phase is invented")
	assert.Equal(t, "www_to_apex", got[rules.PhaseDynamicRedirect][0].Ref)
}

// TestRedirectTargetCoversEveryPath pins the emitted expression and checks
// its one literal/path shape for root, nested and percent-encoded request
// paths; query preservation is the action parameter, not a second concat.
func TestRedirectTargetCoversEveryPath(t *testing.T) {
	c, err := rules.Redirects(&zonespec.Redirects{
		Zone:    "example.com",
		Entries: []zonespec.Redirect{{Ref: "www_to_apex", From: "www.example.com", To: "https://example.com"}},
	})
	require.NoError(t, err)

	r := c.Rules[0].Params.Redirect
	require.NotNil(t, r)
	assert.Equal(t, `concat("https://example.com", http.request.uri.path)`, r.TargetURL)
	assert.True(t, r.PreserveQueryString)
	assert.NotContains(t, r.TargetURL, "http.request.uri.query",
		"the query is kept by preserveQueryString, so it is not concatenated twice")

	// The expression is concat("<origin>", http.request.uri.path): evaluating
	// it for a path appends that path unchanged, including its encoding.
	const prefix, suffix = `concat("`, `", http.request.uri.path)`
	origin := strings.TrimSuffix(strings.TrimPrefix(r.TargetURL, prefix), suffix)

	for _, path := range []string{"/", "/a/b", "/a%20b", "/a%2Fb"} {
		assert.Equal(t, "https://example.com"+path, origin+path)
	}
}
