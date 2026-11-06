package config_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/zonespec"
)

const scopeZone = "    managed_waf: {enabled: true, scope: zone}\n"

func TestScopeZoneRunsTheWAFOnTheWholeZoneMinusTheOptOuts(t *testing.T) {
	records := scopeZone + `    records:
      - {name: lb.example.com, type: CNAME, content: lb.example.net, proxied: true}
      - {name: raw.example.com, type: CNAME, content: lb.example.net, proxied: true, waf: false, waf_reason: raw uploads}
      - {name: '*.apps.example.com', type: CNAME, content: lb.example.net, proxied: true, waf: false, waf_reason: per-install names}
      - {name: '*.open.example.com', type: CNAME, content: lb.example.net, proxied: true}
`
	c, err := loadWith(t, [2]string{fixtureWAF, records}, [2]string{})
	require.NoError(t, err)

	z := c.Zones["example.com"]
	assert.Empty(t, z.WAFHosts, "a whole-zone rule lists no covered host")

	contribs, err := z.Contributions()
	require.NoError(t, err)

	// Exact opt-outs are one set; a wildcard opt-out (catalog or record) is a
	// suffix test. A proxied wildcard that did not opt out is simply covered.
	assert.Equal(t, `not (http.host in {"raw.example.com"} or ends_with(http.host, ".apps.example.com") `+
		`or ends_with(http.host, ".devel.example.com") or ends_with(http.host, ".eudi.devel.example.com"))`,
		wafExpression(t, contribs))
}

func TestScopeZoneWithNothingOptedOutIsTrue(t *testing.T) {
	// example.org declares no catalog name and no record to opt out.
	const tls13 = "    min_tls_version: \"1.3\"\n"

	c, err := loadWith(t, [2]string{tls13, tls13 + scopeZone}, [2]string{})
	require.NoError(t, err)

	contribs, err := c.Zones["example.org"].Contributions()
	require.NoError(t, err)
	assert.Equal(t, "true", wafExpression(t, contribs))
}

func TestScopeZoneRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ block, want string }{
		"with hosts":      {"    managed_waf: {enabled: true, scope: zone, hosts: [gemaal.example.com]}\n", "exclusive"},
		"with whole_zone": {"    managed_waf: {enabled: true, scope: zone, whole_zone: true}\n", "exclusive"},
		"without enabled": {"    managed_waf: {scope: zone}\n", "scope"},
		"hosts scope":     {"    managed_waf: {scope: hosts, whole_zone: true}\n", "scope"},
		"unknown scope":   {"    managed_waf: {enabled: true, scope: everything}\n", "scope"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := loadWith(t, [2]string{fixtureWAF, tc.block}, [2]string{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}

	t.Run("an opt-out without a reason", func(t *testing.T) {
		rec := scopeZone + "    records:\n      - {name: lb.example.com, type: CNAME, content: x.example.net, proxied: true, waf: false}\n"
		_, err := loadWith(t, [2]string{fixtureWAF, rec}, [2]string{})
		require.ErrorContains(t, err, "waf_reason")

		_, err = loadWith(t, [2]string{fixtureWAF, scopeZone}, [2]string{"gemaal: {domains: [gemaal.example.com], route_namespaces: [gemaal]}",
			"gemaal: {domains: [{host: gemaal.example.com, waf: off}], route_namespaces: [gemaal]}"})
		require.ErrorContains(t, err, "waf_reason")
	})
}

func TestScopeZoneRefusesAnOversizedExclusionList(t *testing.T) {
	var b strings.Builder

	b.WriteString(scopeZone + "    records:\n")

	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "      - {name: host-%04d-padding-padding.example.com, type: CNAME, content: x.example.net, proxied: true, waf: false, waf_reason: r}\n", i)
	}

	c, err := loadWith(t, [2]string{fixtureWAF, b.String()}, [2]string{})
	require.NoError(t, err)

	_, err = c.Zones["example.com"].Contributions()
	require.ErrorContains(t, err, "4096")
}

func TestScopeZoneIsOffByDefault(t *testing.T) {
	c := fixture(t)
	assert.Empty(t, c.Zones["example.com"].ManagedWAF.Scope)
	assert.NotEmpty(t, c.Zones["example.com"].WAFHosts)
}

func TestWAFExpressionWithExclusions(t *testing.T) {
	m := &zonespec.ManagedWAF{Zone: "example.com", WholeZone: true,
		ExcludeHosts: []string{"*.dev.example.com", "b.example.com", "a.example.com"}}
	require.NoError(t, (&zonespec.Args{ZoneID: "z", ManagedWAF: m}).Validate())
	assert.Equal(t, `not (http.host in {"a.example.com" "b.example.com"} or ends_with(http.host, ".dev.example.com"))`,
		zonespec.ManagedWAFExpression(m))

	assert.Equal(t, "true", zonespec.ManagedWAFExpression(&zonespec.ManagedWAF{WholeZone: true}))
}

func TestWAFExclusionsAreHeldToTheirRules(t *testing.T) {
	for name, m := range map[string]*zonespec.ManagedWAF{
		"without whole zone": {Zone: "example.com", Hosts: []string{"a.example.com"}, ExcludeHosts: []string{"b.example.com"}},
		"without a zone":     {WholeZone: true, ExcludeHosts: []string{"b.example.com"}},
		"outside the zone":   {Zone: "example.com", WholeZone: true, ExcludeHosts: []string{"b.example.org"}},
		"repeated":           {Zone: "example.com", WholeZone: true, ExcludeHosts: []string{"b.example.com", "b.example.com"}},
		"upper case":         {Zone: "example.com", WholeZone: true, ExcludeHosts: []string{"B.example.com"}},
	} {
		require.Error(t, (&zonespec.Args{ZoneID: "z", ManagedWAF: m}).Validate(), name)
	}
}
