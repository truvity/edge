package zone

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	cfrules "github.com/truvity/edge/v4/cloudflare/rules"
)

func redirects(entries ...Redirect) *Redirects {
	return &Redirects{Zone: "example.com", Entries: entries}
}

func redirectArgs() Args {
	return Args{
		ZoneID: "zone-1",
		Redirects: redirects(Redirect{
			Ref: "www_to_apex", From: "www.example.com", To: "https://example.com",
		}),
	}
}

// TestRedirectRuleMapping pins the Cloudflare shape: the dynamic redirect
// phase, the exact-host match, and the redirect action parameters.
func TestRedirectRuleMapping(t *testing.T) {
	m := run(t, redirectArgs())

	set, ok := m.res[rulesetType+"/redirect-rules-example"]
	require.True(t, ok, "the redirect ruleset was not registered")
	assert.Equal(t, "zone-1", set["zoneId"].StringValue())
	assert.Equal(t, "zone", set["kind"].StringValue())
	assert.Equal(t, "http_request_dynamic_redirect", set["phase"].StringValue())
	assert.Equal(t, "default", set["name"].StringValue())

	list := set["rules"].ArrayValue()
	require.Len(t, list, 1)

	rule := list[0].ObjectValue()
	assert.Equal(t, "www_to_apex", rule["ref"].StringValue(), "the stable ref is on the provider's rule")
	assert.True(t, rule["enabled"].BoolValue())
	assert.Equal(t, "redirect", rule["action"].StringValue())
	assert.Equal(t, `http.host eq "www.example.com"`, rule["expression"].StringValue(),
		"http.host matches both HTTP and HTTPS, on every path")

	fromValue := rule["actionParameters"].ObjectValue()["fromValue"].ObjectValue()
	assert.Equal(t, 301.0, fromValue["statusCode"].NumberValue())
	assert.True(t, fromValue["preserveQueryString"].BoolValue())
	assert.Equal(t,
		`concat("https://example.com", http.request.uri.path)`,
		fromValue["targetUrl"].ObjectValue()["expression"].StringValue())
}

func TestRedirectStatusAndQueryOptions(t *testing.T) {
	keep := false
	m := run(t, Args{
		ZoneID: "zone-1",
		Redirects: redirects(Redirect{
			Ref: "www_to_apex", From: "www.example.com", To: "https://example.com",
			Status: 302, PreserveQueryString: &keep,
		}),
	})

	rule := m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()[0].ObjectValue()
	params := rule["actionParameters"].ObjectValue()["fromValue"].ObjectValue()
	assert.Equal(t, 302.0, params["statusCode"].NumberValue())
	assert.False(t, params["preserveQueryString"].BoolValue())

	m = run(t, redirectArgs())
	rule = m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()[0].ObjectValue()
	params = rule["actionParameters"].ObjectValue()["fromValue"].ObjectValue()
	assert.Equal(t, 301.0, params["statusCode"].NumberValue(), "301 is the default")
	assert.True(t, params["preserveQueryString"].BoolValue(), "query preservation is the default")
}

// TestRedirectDisabledKeepsRef is the staged-rollback shape: the rule stays
// declared, under its ref, and is merely inert.
func TestRedirectDisabledKeepsRef(t *testing.T) {
	m := run(t, Args{
		ZoneID: "zone-1",
		Redirects: redirects(Redirect{
			Ref: "www_to_apex", From: "www.example.com", To: "https://example.com", Disabled: true,
		}),
	})

	rule := m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()[0].ObjectValue()
	assert.Equal(t, "www_to_apex", rule["ref"].StringValue())
	assert.False(t, rule["enabled"].BoolValue())
}

// TestRedirectEntriesSortBySource keeps the rendered rule order independent
// of the order the file lists them in.
func TestRedirectEntriesSortBySource(t *testing.T) {
	m := run(t, Args{
		ZoneID: "zone-1",
		Redirects: redirects(
			Redirect{Ref: "z", From: "z.example.com", To: "https://example.com"},
			Redirect{Ref: "a", From: "a.example.com", To: "https://example.com"},
		),
	})

	list := m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()
	require.Len(t, list, 2)
	assert.Equal(t, "a", list[0].ObjectValue()["ref"].StringValue())
	assert.Equal(t, "z", list[1].ObjectValue()["ref"].StringValue())
}

func TestRedirectAdoptsTheExistingRuleset(t *testing.T) {
	args := redirectArgs()
	args.ImportRulesets = map[cfrules.Phase]string{cfrules.PhaseDynamicRedirect: "redirect-ruleset-id"}

	m := run(t, args)
	assert.Equal(t, map[string]string{
		rulesetType + "/redirect-rules-example": "zones/zone-1/redirect-ruleset-id",
	}, m.imported)
}

func TestRedirectAndOtherPhasesCoexist(t *testing.T) {
	args := redirectArgs()
	args.Cache = &Cache{Hosts: []string{"app.example.com"}}

	m := run(t, args)
	assert.Contains(t, m.res, rulesetType+"/cache-rules-example")
	assert.Contains(t, m.res, rulesetType+"/redirect-rules-example")

	cache := rules(t, m)
	require.Len(t, cache, 2)
	assert.Equal(t,
		`http.host in {"app.example.com"}`,
		cache[0]["expression"].StringValue(),
		"the cache phase keeps its own partition")
}

func TestRedirectValidation(t *testing.T) {
	ok := func(mutate func(*Redirects)) Args {
		r := redirects(Redirect{Ref: "www_to_apex", From: "www.example.com", To: "https://example.com"})
		mutate(r)

		return Args{ZoneID: "zone-1", Redirects: r}
	}

	cases := []struct {
		name string
		args Args
		want string
	}{
		{"ok", ok(func(*Redirects) {}), ""},
		{"source outside zone", ok(func(r *Redirects) { r.Entries[0].From = "www.example.net" }), "inside example.com"},
		{"source wildcard", ok(func(r *Redirects) { r.Entries[0].From = "*.example.com" }), "no wildcard"},
		{"source upper case", ok(func(r *Redirects) { r.Entries[0].From = "WWW.example.com" }), "exact hostname"},
		{"source path", ok(func(r *Redirects) { r.Entries[0].From = "www.example.com/path" }), "exact hostname"},
		{"http destination", ok(func(r *Redirects) { r.Entries[0].To = "http://example.com" }), "HTTPS"},
		{"destination path", ok(func(r *Redirects) { r.Entries[0].To = "https://example.com/base" }), "no path"},
		{"destination query", ok(func(r *Redirects) { r.Entries[0].To = "https://example.com?x=1" }), "no path"},
		{"destination fragment", ok(func(r *Redirects) { r.Entries[0].To = "https://example.com#x" }), "no path"},
		{"destination port", ok(func(r *Redirects) { r.Entries[0].To = "https://example.com:8443" }), "no path"},
		{"destination user", ok(func(r *Redirects) { r.Entries[0].To = "https://user@example.com" }), "no path"},
		{"destination upper case", ok(func(r *Redirects) { r.Entries[0].To = "https://EXAMPLE.com" }), "lower-case hostname"},
		{"bad status", ok(func(r *Redirects) { r.Entries[0].Status = 200 }), "not a redirect status"},
		{"duplicate ref", ok(func(r *Redirects) {
			r.Entries = append(r.Entries, Redirect{Ref: "www_to_apex", From: "other.example.com", To: "https://example.com"})
		}), "already used"},
		{"duplicate source", ok(func(r *Redirects) {
			r.Entries = append(r.Entries, Redirect{Ref: "other", From: "www.example.com", To: "https://example.com"})
		}), "one redirect per source host"},
		{"bad ref", ok(func(r *Redirects) { r.Entries[0].Ref = "WWW" }), "ref must be lowercase"},
		{"self loop", ok(func(r *Redirects) { r.Entries[0].To = "https://www.example.com" }), "redirect loop"},
		{"two-hop loop", ok(func(r *Redirects) {
			r.Entries = append(r.Entries, Redirect{Ref: "apex_to_www", From: "example.com", To: "https://www.example.com"})
		}), "redirect loop"},
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

// TestRedirectChainWithoutLoopIsAllowed distinguishes a canonical chain
// (www -> apex -> canonical) from a loop.
func TestRedirectChainWithoutLoopIsAllowed(t *testing.T) {
	args := Args{
		ZoneID: "zone-1",
		Redirects: redirects(
			Redirect{Ref: "www_to_apex", From: "www.example.com", To: "https://example.com"},
			Redirect{Ref: "apex_to_canonical", From: "example.com", To: "https://canonical.example.com"},
		),
	}
	require.NoError(t, args.Validate())

	m := run(t, args)
	assert.Len(t, m.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue(), 2)
}

// runWithDeps records each resource's explicit Pulumi dependencies, keyed by
// type and name. The mocks alone do not expose DependsOn.
func runWithDeps(t *testing.T, args Args) (*mocks, map[string][]string) {
	t.Helper()

	m := &mocks{res: map[string]resource.PropertyMap{}}
	deps := map[string][]string{}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		if err := ctx.RegisterStackTransformation(func(ta *pulumi.ResourceTransformationArgs) *pulumi.ResourceTransformationResult {
			opts, err := pulumi.NewResourceOptions(ta.Opts...)
			if err != nil || len(opts.DependsOn) == 0 {
				return nil
			}

			names := make([]string, 0, len(opts.DependsOn))
			for _, d := range opts.DependsOn {
				names = append(names, d.PulumiResourceName())
			}

			deps[ta.Type+"::"+ta.Name] = names

			return nil
		}); err != nil {
			return err
		}

		_, err := New(ctx, "example", args)

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	return m, deps
}

// TestProxiedRedirectSourceWaitsForTheRuleset is the explicit
// rule-before-proxy dependency: the DNS record's update depends on the
// redirect ruleset resource, not on declaration order.
func TestProxiedRedirectSourceWaitsForTheRuleset(t *testing.T) {
	args := redirectArgs()
	args.Records = &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "www.example.com", Type: "CNAME", Content: "legacy.example.net", Proxied: true},
			{Name: "docs.example.com", Type: "CNAME", Content: "docs.example.net", Proxied: true},
		},
	}

	_, deps := runWithDeps(t, args)

	assert.Equal(t, []string{"redirect-rules-example"},
		deps[dnsRecordType+"::record-example-www-example-com-cname"],
		"the www proxy update waits for the redirect ruleset")
	assert.Empty(t, deps[dnsRecordType+"::record-example-docs-example-com-cname"],
		"an unrelated proxied record does not wait")
}

// TestDNSOnlyRedirectSourceStagesTheRule is the first rollback stage: the
// redirect is declared, the source DNS is still DNS-only, and no dependency
// is needed because the rule is inert until the proxy is enabled.
func TestDNSOnlyRedirectSourceStagesTheRule(t *testing.T) {
	args := redirectArgs()
	args.Records = &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "www.example.com", Type: "CNAME", Content: "legacy.example.net", TTL: 300},
		},
	}

	m, deps := runWithDeps(t, args)

	assert.Empty(t, deps[dnsRecordType+"::record-example-www-example-com-cname"])
	assert.False(t, m.res[dnsRecordType+"/record-example-www-example-com-cname"]["proxied"].BoolValue())
	assert.Contains(t, m.res, rulesetType+"/redirect-rules-example", "the inert rule is still declared")
}

// TestRedirectRollbackKeepsIdentities keeps the DNS record and ruleset child
// names stable across the two rollback stages, and keeps the rule ref while
// it is disabled.
func TestRedirectRollbackKeepsIdentities(t *testing.T) {
	proxied := redirectArgs()
	proxied.Records = &Records{Zone: "example.com", Entries: []Record{
		{Name: "www.example.com", Type: "CNAME", Content: "legacy.example.net", Proxied: true},
	}}

	before, _ := runWithDeps(t, proxied)

	rolledBack := redirectArgs()
	rolledBack.Redirects.Entries[0].Disabled = true
	rolledBack.Records = &Records{Zone: "example.com", Entries: []Record{
		{Name: "www.example.com", Type: "CNAME", Content: "legacy.example.net", TTL: 300},
	}}

	after, _ := runWithDeps(t, rolledBack)

	for _, key := range []string{
		dnsRecordType + "/record-example-www-example-com-cname",
		rulesetType + "/redirect-rules-example",
	} {
		assert.Contains(t, before.res, key)
		assert.Contains(t, after.res, key, "the identity survives the rollback")
	}

	rule := after.res[rulesetType+"/redirect-rules-example"]["rules"].ArrayValue()[0].ObjectValue()
	assert.Equal(t, "www_to_apex", rule["ref"].StringValue())
	assert.False(t, rule["enabled"].BoolValue())
}
