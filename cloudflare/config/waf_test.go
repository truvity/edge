package config_test

import (
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/catalog"
	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/rules"
)

// loadWith loads the fixture with the given replacements applied to its
// cloudflare.yaml and gateways.yaml.
func loadWith(t *testing.T, cf, gw [2]string) (*config.Config, error) {
	t.Helper()

	cfData, gwData := string(readFixture(t, config.FileName)), string(readFixture(t, catalog.FileName))
	if cf[0] != "" {
		cfData = replaceOnce(t, cfData, cf[0], cf[1])
	}

	if gw[0] != "" {
		gwData = replaceOnce(t, gwData, gw[0], gw[1])
	}

	fsys := fstest.MapFS{config.FileName: {Data: []byte(cfData)}, catalog.FileName: {Data: []byte(gwData)}}

	cat, err := catalog.Load(fsys)
	if err != nil {
		return nil, err
	}

	return config.Load(fsys, cat, fixtureProjects)
}

const fixtureWAF = "    managed_waf:\n      enabled: true\n"

func TestWAFHostsAreDerivedFromTheCatalog(t *testing.T) {
	z := fixture(t).Zones["example.com"]

	// A dns_elsewhere name is still proxied traffic; a platform console is
	// covered too; the opted-out wildcards are the only names missing, and
	// names of other zones never appear.
	assert.Contains(t, z.WAFHosts, "old.devel.example.com")
	assert.Contains(t, z.WAFHosts, "headlamp.kernel.example.com")
	assert.Contains(t, z.WAFHosts, "example.com")
	assert.NotContains(t, z.WAFHosts, "landing.devel.example.net")
	assert.NotContains(t, z.WAFHosts, "*.devel.example.com")
	assert.NotContains(t, z.WAFHosts, "*.globex.devel.example.com")
}

func TestWAFOptOutRemovesTheNameAndNeedsAReason(t *testing.T) {
	c, err := loadWith(t, [2]string{}, [2]string{"gemaal: {domains: [gemaal.example.com], route_namespaces: [gemaal]}",
		"gemaal: {domains: [{host: gemaal.example.com, waf: off, waf_reason: raw uploads}], route_namespaces: [gemaal]}"})
	require.NoError(t, err)
	assert.NotContains(t, c.Zones["example.com"].WAFHosts, "gemaal.example.com")
	assert.Contains(t, c.Zones["example.com"].WAFHosts, "acme.devel.example.com")

	_, err = loadWith(t, [2]string{}, [2]string{"gemaal: {domains: [gemaal.example.com], route_namespaces: [gemaal]}",
		"gemaal: {domains: [{host: gemaal.example.com, waf: off}], route_namespaces: [gemaal]}"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "waf_reason")
}

func TestAProxiedWildcardWithoutAnOptOutFailsTheLoad(t *testing.T) {
	_, err := loadWith(t, [2]string{}, [2]string{
		"{host: '*.devel.example.com', waf: off, waf_reason: catch-all wildcard; exact names are covered by the groups that serve them}",
		"'*.devel.example.com'"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"*.devel.example.com"`)
	assert.Contains(t, err.Error(), "wildcard")
}

func TestManagedWAFHasOneSource(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"enabled with hosts":      {"    managed_waf: {enabled: true, hosts: [gemaal.example.com]}\n", "exclusive"},
		"enabled with whole zone": {"    managed_waf: {enabled: true, whole_zone: true}\n", "exclusive"},
		"hosts with a wildcard":   {"    managed_waf: {hosts: ['*.devel.example.com']}\n", "wildcard is refused"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWith(t, [2]string{fixtureWAF, tc.block}, [2]string{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// A consumer that writes the older exact host list, or whole_zone, loads and
// builds the rule from it exactly as before.
func TestManagedWAFWithoutEnabledIsUnchanged(t *testing.T) {
	c, err := loadWith(t, [2]string{fixtureWAF, "    managed_waf: {hosts: [gemaal.example.com, access.example.com]}\n"}, [2]string{})
	require.NoError(t, err)

	z := c.Zones["example.com"]
	assert.Empty(t, z.WAFHosts, "nothing is derived without enabled")
	assert.Equal(t, []string{"gemaal.example.com", "access.example.com"}, z.ManagedWAF.Hosts)
	assert.Equal(t, []string{"gemaal.example.com", "access.example.com"}, z.ManagedWAFLibrary("example.com").Hosts)

	contribs, err := z.Contributions()
	require.NoError(t, err)

	assert.Equal(t, `http.host in {"access.example.com" "gemaal.example.com"}`, wafExpression(t, contribs))

	c, err = loadWith(t, [2]string{fixtureWAF, "    managed_waf: {whole_zone: true}\n"}, [2]string{})
	require.NoError(t, err)

	contribs, err = c.Zones["example.com"].Contributions()
	require.NoError(t, err)
	assert.Equal(t, "true", wafExpression(t, contribs))
}

func TestRecordsJoinTheDerivedWAFUnlessOptedOut(t *testing.T) {
	records := fixtureWAF + `    records:
      - {name: lb.example.com, type: CNAME, content: lb.example.net, proxied: true}
      - {name: raw.example.com, type: CNAME, content: lb.example.net, proxied: true, waf: false, waf_reason: raw uploads}
      - {name: dnsonly.example.com, type: CNAME, content: lb.example.net}
`
	c, err := loadWith(t, [2]string{fixtureWAF, records}, [2]string{})
	require.NoError(t, err)

	hosts := c.Zones["example.com"].WAFHosts
	assert.Contains(t, hosts, "lb.example.com")
	assert.NotContains(t, hosts, "raw.example.com")
	assert.NotContains(t, hosts, "dnsonly.example.com", "a DNS-only record is not behind Cloudflare")

	for name, tc := range map[string]struct{ record, want string }{
		"opt out without a reason": {"{name: lb.example.com, type: CNAME, content: x.example.net, proxied: true, waf: false}", "waf_reason"},
		"proxied wildcard":         {"{name: '*.apps.example.com', type: CNAME, content: x.example.net, proxied: true}", "proxied wildcard"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWith(t, [2]string{fixtureWAF, fixtureWAF + "    records:\n      - " + tc.record + "\n"}, [2]string{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	optedOut := "{name: '*.apps.example.com', type: CNAME, content: x.example.net, proxied: true, waf: false, waf_reason: wildcard}"
	_, err = loadWith(t, [2]string{fixtureWAF, fixtureWAF + "    records:\n      - " + optedOut + "\n"}, [2]string{})
	require.NoError(t, err, "a proxied wildcard record is fine once opted out")
}

func TestAddWAFHostsTakesExactNamesOfTheZone(t *testing.T) {
	c := fixture(t)
	before := len(c.Zones["example.com"].WAFHosts)

	require.NoError(t, c.AddWAFHosts("example.com", "status.example.com", "acme.devel.example.com"))

	hosts := c.Zones["example.com"].WAFHosts
	assert.Len(t, hosts, before+1, "a name already covered is not repeated")
	assert.Contains(t, hosts, "status.example.com")
	assert.True(t, strings.Compare(hosts[0], hosts[1]) < 0, "sorted")

	for _, bad := range []string{"*.example.com", "Status.example.com", "status.example.net", "https://status.example.com", "status.example.com.", ""} {
		err := c.AddWAFHosts("example.com", bad)
		require.Error(t, err, bad)
	}

	require.Error(t, c.AddWAFHosts("example.org", "a.example.org"), "example.org does not enable the managed WAF")
	require.Error(t, c.AddWAFHosts("nope.test", "a.nope.test"))

	contribs, err := c.Zones["example.com"].Contributions()
	require.NoError(t, err)
	assert.Contains(t, wafExpression(t, contribs), `"status.example.com"`)
}

// The rule a fixture zone builds is pinned literally: a catalog edit that
// changes what the WAF covers shows up here, in a diff a person reads.
func TestDerivedManagedWAFExpressionIsPinned(t *testing.T) {
	contribs, err := fixture(t).Zones["example.com"].Contributions()
	require.NoError(t, err)

	assert.Equal(t, `http.host in {"access.example.com" "acme.devel.example.com" "acme.prod.example.com" "acme.stage.example.com" `+
		`"alert-ingress.example.com" "alertmanager.example.com" "argocd.example.com" "example.com" "gemaal.example.com" `+
		`"globex.devel.example.com" "globex.prod.example.com" "globex.stage.example.com" "grafana.example.com" `+
		`"headlamp.devel.example.com" "headlamp.kernel.example.com" "headlamp.prod.example.com" "headlamp.stage.example.com" `+
		`"mcp.example.com" "old.devel.example.com" "otlp.devel.example.com" "otlp.example.com" "umbrella-corp.devel.example.com" `+
		`"umbrella-corp.prod.example.com" "umbrella-corp.stage.example.com"}`, wafExpression(t, contribs))
}

func TestAnExpressionOverCloudflaresLimitIsRefused(t *testing.T) {
	c := fixture(t)

	var many []string
	for i := 0; i < 300; i++ {
		many = append(many, "host-"+strings.Repeat("x", 5)+"-"+string(rune('a'+i%26))+strings.Repeat("y", i/26)+".example.com")
	}

	require.NoError(t, c.AddWAFHosts("example.com", many...))

	_, err := c.Zones["example.com"].Contributions()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "4096")
}

// wafExpression is the managed WAF rule's expression among the contributions.
func wafExpression(t *testing.T, contribs []rules.Contribution) string {
	t.Helper()

	for _, c := range contribs {
		if c.Source == "managed-waf" {
			require.Len(t, c.Rules, 1)

			return c.Rules[0].Expression
		}
	}

	t.Fatal("no managed-waf contribution")

	return ""
}

// TestNSDelegationIsNotAWAFHost keeps a DNS-only child-zone delegation out
// of a managed WAF's derived host list.
func TestNSDelegationIsNotAWAFHost(t *testing.T) {
	cf := replaceOnce(t, string(readFixture(t, config.FileName)),
		"  example.org:\n", "  example.org:\n    managed_waf: {enabled: true}\n")

	c, err := load(t, cf, string(readFixture(t, catalog.FileName)))
	require.NoError(t, err)

	hosts := c.Zones["example.org"].WAFHosts
	assert.NotContains(t, hosts, "team.example.org", "a delegation is never a WAF host")
	assert.Contains(t, hosts, "ssi.example.org", "a proxied tunnel name is")
}
