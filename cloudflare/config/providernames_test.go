package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/config"
)

const publishedNames = `{
  "version": 1,
  "revision": 7,
  "zones": {
    "partner.test": {
      "hosts": ["app.partner.test"],
      "status": ["status.partner.test"],
      "tunnel": ["console.partner.test"]
    }
  }
}`

// companyRegistry is the fixture with partner.test owned by company partner
// and the given extra keys on that zone.
func companyRegistry(t *testing.T, extra string) string {
	t.Helper()

	y := replaceOnce(t, registry, "partner.test: {account: partner, zone_id: z2,", "partner.test: {account: partner, owner: partner, zone_id: z2, "+extra)

	return y
}

func published(t *testing.T) *config.ProviderNames {
	t.Helper()

	p, err := config.ParseProviderNames([]byte(publishedNames))
	require.NoError(t, err)

	return p
}

func withNames(t *testing.T, extra string) error {
	t.Helper()

	c, err := config.Parse([]byte(companyRegistry(t, extra)))
	require.NoError(t, err)

	return c.WithProviderNames(published(t))
}

func TestACompanyZoneRefusesAProviderOwnedName(t *testing.T) {
	for name, tc := range map[string]struct{ extra, want string }{
		"a status host": {
			`records: [{name: status.partner.test, type: CNAME, content: pages.example.net}],`,
			"zones[partner.test].records[status.partner.test]: status.partner.test is provider-owned (published input rev 7); the platform writes it",
		},
		"a tunnel host": {
			`records: [{name: console.partner.test, type: A, content: 192.0.2.1}],`,
			"zones[partner.test].records[console.partner.test]: console.partner.test is provider-owned (published input rev 7)",
		},
		"an exact host": {
			`records: [{name: app.partner.test, type: A, content: 192.0.2.1}],`,
			"app.partner.test is provider-owned",
		},
		"an origin switch": {
			`origin_cnames: {console.partner.test: ""},`,
			"zones[partner.test].origin_cnames[console.partner.test]: console.partner.test is provider-owned (published input rev 7)",
		},
		"a redirect source": {
			`redirects: [{ref: r, from: status.partner.test, to: "https://partner.test"}],`,
			"zones[partner.test].redirects[r].from: status.partner.test is provider-owned (published input rev 7)",
		},
		"a redirect target": {
			`redirects: [{ref: r, from: www.partner.test, to: "https://app.partner.test"}],`,
			"zones[partner.test].redirects[r].to: app.partner.test is provider-owned (published input rev 7)",
		},
	} {
		err := withNames(t, tc.extra)
		require.ErrorContains(t, err, tc.want, name)
	}
}

func TestProviderNamesLeaveOtherNamesAlone(t *testing.T) {
	require.NoError(t, withNames(t, `records: [{name: www.partner.test, type: CNAME, content: pages.example.net}],`))
	require.NoError(t, withNames(t, ""))

	// A platform zone, and a zone that names no owner, are not a company's.
	c, err := config.Parse([]byte(replaceOnce(t, registry,
		"example.com: {account: platform, zone_id: z1,",
		"example.com: {account: platform, owner: platform, zone_id: z1, records: [{name: status.example.com, type: CNAME, content: pages.example.net}],")))
	require.NoError(t, err)

	p, err := config.ParseProviderNames([]byte(`{"version":1,"revision":2,"zones":{"example.com":{"status":["status.example.com"]}}}`))
	require.NoError(t, err)
	require.NoError(t, c.WithProviderNames(p))
}

func TestWithProviderNamesRefusesNil(t *testing.T) {
	c, err := config.Parse([]byte(registry))
	require.NoError(t, err)
	require.Error(t, c.WithProviderNames(nil))
}

func TestParseProviderNamesRefusals(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"an unknown version": {`{"version":2,"revision":1,"zones":{}}`, "version 2"},
		"no revision":        {`{"version":1,"zones":{}}`, "revision"},
		"an unknown key":     {`{"version":1,"revision":1,"zones":{},"extra":1}`, "unknown field"},
		"a wildcard":         {`{"version":1,"revision":1,"zones":{"a.test":{"hosts":["*.a.test"]}}}`, "exact"},
		"a host elsewhere":   {`{"version":1,"revision":1,"zones":{"a.test":{"hosts":["x.b.test"]}}}`, "inside zone a.test"},
		"an upper-case host": {`{"version":1,"revision":1,"zones":{"a.test":{"hosts":["X.a.test"]}}}`, "lower-case"},
		"empty":              {``, "empty"},
		"trailing data":      {`{"version":1,"revision":1,"zones":{}} {}`, "trailing"},
	} {
		_, err := config.ParseProviderNames([]byte(tc.in))
		require.ErrorContains(t, err, tc.want, name)
	}
}

func TestProviderNamesRoundTripAndAreSorted(t *testing.T) {
	p := &config.ProviderNames{Version: 1, Revision: 3, Zones: map[string]config.ProviderZone{
		"a.test": {Tunnel: []string{"z.a.test", "b.a.test", "b.a.test"}, Status: []string{"s.a.test"}},
	}}

	data, err := p.Marshal()
	require.NoError(t, err)

	back, err := config.ParseProviderNames(data)
	require.NoError(t, err)
	assert.Equal(t, []string{"b.a.test", "z.a.test"}, back.Zones["a.test"].Tunnel, "sorted and de-duplicated")
	assert.Equal(t, []string{"b.a.test", "s.a.test", "z.a.test"}, back.Zones["a.test"].Names())

	again, err := back.Marshal()
	require.NoError(t, err)
	assert.Equal(t, string(data), string(again))
}

func TestProviderNamesForListsTheTunnelHostsOfAZone(t *testing.T) {
	c := fixture(t)

	got := c.ProviderNamesFor("example.com")
	assert.Contains(t, got.Tunnel, "headlamp.kernel.example.com")
	assert.NotContains(t, got.Tunnel, "*.devel.example.com", "exact names only")
	assert.NotContains(t, got.Tunnel, "old.devel.example.com", "a dns_elsewhere name is written by someone else")
	assert.Empty(t, c.ProviderNamesFor("nowhere.test").Tunnel)

	for _, h := range got.Tunnel {
		assert.Equal(t, "example.com", c.ZoneOf(h))
	}
}
