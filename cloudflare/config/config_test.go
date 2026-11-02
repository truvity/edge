package config_test

import (
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/catalog"
	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/edgespec"
	"github.com/truvity/edge/v4/cloudflare/rules"
)

// The fixture is shaped like an estate's real cloudflare.yaml and edge
// catalog, with placeholder accounts, zones and ids.
var fixtureProjects = []catalog.Project{{Name: "dms"}, {Name: "eudi"}, {Name: "url-shortener"}, {Name: "landing"}}

func fixtureCatalog(t *testing.T) *catalog.Config {
	t.Helper()

	cat, err := catalog.Load(os.DirFS("testdata"))
	require.NoError(t, err)

	return cat
}

func fixture(t *testing.T) *config.Config {
	t.Helper()

	c, err := config.Load(os.DirFS("testdata"), fixtureCatalog(t), fixtureProjects)
	require.NoError(t, err, "load the fixture")

	return c
}

// TestNoSDKImports guards this package's reason to exist: an estate's config
// layer must link no Pulumi or Cloudflare SDK. Every dependency outside the
// standard library is the YAML decoder or a package of this module that is
// itself SDK-free.
func TestNoSDKImports(t *testing.T) {
	const pkg = "github.com/truvity/edge/v4/cloudflare/config"

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	require.NoError(t, err, "go list -deps")

	allowed := []string{
		"go.yaml.in/yaml/v3",
		"github.com/truvity/edge/v4/catalog",
		"github.com/truvity/edge/v4/cloudflare/cfnames",
		"github.com/truvity/edge/v4/cloudflare/edgespec",
		"github.com/truvity/edge/v4/cloudflare/rules",
		"github.com/truvity/edge/v4/cloudflare/rulespec",
		"github.com/truvity/edge/v4/cloudflare/zonespec",
	}

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || line == pkg {
			continue
		}

		root, _, _ := strings.Cut(line, "/")
		if !strings.Contains(root, ".") {
			continue
		}

		assert.Contains(t, allowed, line, "cloudflare/config must depend on the standard library and the SDK-free packages listed, found %q", line)
	}
}

func TestLoadTheFixture(t *testing.T) {
	c := fixture(t)

	require.Contains(t, c.Accounts, "main")
	assert.Equal(t, "aaaa1111bbbb2222cccc3333dddd4444", c.Accounts["main"].AccountID)
	assert.True(t, c.Accounts["main"].LegacyNames)
	assert.Equal(t, "example", c.Accounts["main"].Company)
	assert.Equal(t, "cloudflare-root-main", c.Accounts["main"].BreakglassItem)

	require.Contains(t, c.Zones, "example.com")
	zone := c.Zones["example.com"]
	assert.Equal(t, "example.com", zone.Name, "the zone knows the name it is filed under")
	assert.Equal(t, "main", zone.Account)
	assert.Equal(t, "full", zone.SSLMode)
	assert.Equal(t, "1.2", zone.MinTLSVersion)
	assert.Equal(t, &config.TotalTLSCfg{Enabled: true, CertificateAuthority: "lets_encrypt"}, zone.TotalTLS)

	assert.Equal(t, []string{"devel", "kernel", "prod", "stage"}, c.Clusters())
	assert.Equal(t, "gateway-internal", c.Fleets.Internal.Service)
	assert.Equal(t, "1", c.Root.Edge.Rotation)
}

// TestLiveNamesArePreserved is the zero-diff contract of the registry:
// with one account holding the legacy names, every tunnel keeps the names it
// had before the registry existed. A change here renames a live tunnel,
// Application or Secret.
func TestLiveNamesArePreserved(t *testing.T) {
	c := fixture(t)

	// zones is every zone the tunnel's CNAMEs live in (Tunnel.InZones).
	type names struct{ name, release, secret, zones string }

	got := map[string]names{}
	others := map[string]names{}

	for _, tun := range c.Tunnels {
		n := names{tun.Name(), tun.Release(), tun.SecretName(), strings.Join(tun.InZones(), ",")}
		if tun.Account != "main" {
			others[tun.Cluster+"/"+tun.Account] = n

			continue
		}

		got[tun.Cluster] = n
	}

	// A second account's tunnels are suffixed and never rename the first's.
	assert.Equal(t, map[string]names{
		"devel/partner": {"devel-partner", "cloudflared-partner", "cloudflared-partner-tunnel-token", "example.net"},
		"stage/partner": {"stage-partner", "cloudflared-partner", "cloudflared-partner-tunnel-token", "example.net"},
		"prod/partner":  {"prod-partner", "cloudflared-partner", "cloudflared-partner-tunnel-token", "example.net"},
	}, others)

	assert.Equal(t, map[string]names{
		"kernel": {"kernel", "cloudflared", "cloudflared-tunnel-token", "example.com"},
		"devel":  {"devel", "cloudflared", "cloudflared-tunnel-token", "example.com"},
		"stage":  {"stage", "cloudflared", "cloudflared-tunnel-token", "example.com,example.org"},
		"prod":   {"prod", "cloudflared", "cloudflared-tunnel-token", "example.com"},
	}, got)
}

// TestTunnelHostsArePinned is the tunnels' ingress, literally. The lists
// are derived from the catalog, so a catalog edit that adds, drops or
// reorders a tunnel rule changes a live Cloudflare tunnel -- this test makes
// that change show up in review as an edit here, not only as a Pulumi diff
// at deploy time.
func TestTunnelHostsArePinned(t *testing.T) {
	c := fixture(t)

	want := map[string][]string{
		"kernel": {
			// The apex: first, because the catalog orders a cluster's groups
			// as it reads them and `portal` comes before `access`.
			"example.com",
			"access.example.com",
			"alert-ingress.example.com",
			"alertmanager.example.com",
			"argocd.example.com",
			"grafana.example.com",
			"headlamp.kernel.example.com",
			"mcp.example.com",
			"otlp.example.com",
		},
		"devel": {
			"dms.devel.example.com",
			"eudi.devel.example.com",
			"headlamp.devel.example.com",
			"old.devel.example.com",
			"otlp.devel.example.com",
			"url-shortener.devel.example.com",
			"gemaal.example.com",
			"*.eudi.devel.example.com",
			"*.devel.example.com",
		},
		"stage": {
			"dms.stage.example.com",
			"eudi.stage.example.com",
			"headlamp.stage.example.com",
			"url-shortener.stage.example.com",
			// The foreign host: an exact name of its own zone, last in
			// DNS canonical order.
			"ssi.example.org",
		},
		"prod": {
			"dms.prod.example.com",
			"eudi.prod.example.com",
			"headlamp.prod.example.com",
			"url-shortener.prod.example.com",
		},
		"devel-partner": {"landing.devel.example.net"},
		"stage-partner": {"landing.stage.example.net"},
		"prod-partner":  {"landing.prod.example.net"},
	}

	got := map[string][]string{}
	for _, tun := range c.Tunnels {
		got[tun.Name()] = tun.Hosts
	}

	assert.Equal(t, want, got)
}

// A host the catalog marks dns_elsewhere keeps its ingress rule and has no
// CNAME: the tunnel's DNSNames leave it out, and a record of the zone's own
// may name it.
func TestDNSElsewhereKeepsTheRuleAndDropsTheRecord(t *testing.T) {
	c := fixture(t)

	var devel config.Tunnel

	for _, tun := range c.TunnelsOn("devel") {
		if tun.Account == "main" {
			devel = tun
		}
	}

	assert.Contains(t, devel.Hosts, "old.devel.example.com")
	assert.Equal(t, []string{"old.devel.example.com"}, devel.DNSElsewhere)
	assert.NotContains(t, devel.DNSNames(), "old.devel.example.com")
	assert.Len(t, devel.DNSNames(), len(devel.Hosts)-1)
	assert.True(t, devel.CarriesIn("example.com", "old.devel.example.com"))
	assert.False(t, devel.OwnsRecord("example.com", "old.devel.example.com"))
	assert.True(t, devel.OwnsRecord("example.com", "dms.devel.example.com"))

	for _, tun := range c.Tunnels {
		if tun.Name() != "devel" {
			assert.Empty(t, tun.DNSElsewhere, tun.Name())
			assert.Equal(t, tun.Hosts, tun.DNSNames(), tun.Name())
		}
	}

	t.Run("a record may name it", func(t *testing.T) {
		withRecord := replaceOnce(t, string(readFixture(t, config.FileName)), "    min_tls_version: \"1.2\"\n    always_use_https: true\n\n    managed_waf",
			"    min_tls_version: \"1.2\"\n    always_use_https: true\n    records:"+
				"\n      - {name: old.devel.example.com, type: CNAME, content: legacy.example.net}\n\n    managed_waf")
		_, err := config.Load(fstest.MapFS{config.FileName: {Data: []byte(withRecord)}}, fixtureCatalog(t), fixtureProjects)
		require.NoError(t, err)

		onACarriedName := strings.Replace(withRecord, "name: old.devel.example.com", "name: dms.devel.example.com", 1)
		_, err = config.Load(fstest.MapFS{config.FileName: {Data: []byte(onACarriedName)}}, fixtureCatalog(t), fixtureProjects)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already carries this host")
	})

	t.Run("an origin switch may not", func(t *testing.T) {
		origin := replaceOnce(t, string(readFixture(t, config.FileName)), "      access.example.com: \"d-abcdef1234",
			"      old.devel.example.com: \"d-abcdef1234.execute-api.eu-central-1.amazonaws.com\"\n      access.example.com: \"d-abcdef1234")
		_, err := config.Load(fstest.MapFS{config.FileName: {Data: []byte(origin)}}, fixtureCatalog(t), fixtureProjects)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dns_elsewhere")
	})
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	return data
}

// Every catalog name on a tunnel exposure is its own rule, exactly -- which
// is what a listener_set group needs (cloudflared sends the matched rule's
// hostname as origin SNI, and a covering wildcard's would miss the group's
// listener). Private names are not the tunnel's.
func TestEveryTunnelCatalogNameIsAnExactRule(t *testing.T) {
	c := fixture(t)
	cat := fixtureCatalog(t)

	for exposure, e := range cat.Exposures {
		for cluster := range e.Clusters {
			var carried []string
			for _, tun := range c.TunnelsOn(cluster) {
				carried = append(carried, tun.Hosts...)
			}

			for _, group := range cat.Groups(exposure, cluster) {
				for _, host := range group.Hosts() {
					if e.Arrival == catalog.ArrivalCloudflareTunnel {
						assert.Contains(t, carried, host, "%s/%s/%s: %s is not a rule of a %s tunnel", exposure, cluster, group.Name, host, cluster)
					} else {
						assert.NotContains(t, carried, host, "%s/%s/%s: %s arrives by %s and must not be a tunnel rule", exposure, cluster, group.Name, host, e.Arrival)
					}
				}
			}
		}
	}
}

// registry is a cloudflare.yaml with two accounts: platform (legacy
// names) holding example.com, and partner holding partner.test and
// other.test.
const (
	registry = `accounts:
  platform:
    account_id: a1
    legacy_names: true
  partner:
    account_id: a2
zones:
  example.com: {account: platform, zone_id: z1, ssl_mode: full}
  partner.test: {account: partner, zone_id: z2, ssl_mode: full, total_tls: {enabled: true}}
  other.test: {account: partner, zone_id: z3, ssl_mode: full}
fleets:
  internal: {service: gateway-internal, namespace: envoy-gateway-system}
`
)

// catalog wraps groups (YAML, indented for a cluster's groups: map) on
// a public tunnel exposure listing accounts, on the named clusters.
func gateways(accounts string, clusters map[string]string) string {
	out := "version: 1\nexposures:\n  public:\n    class: internal\n    arrival: cloudflare-tunnel\n"
	if accounts != "" {
		out += "    cloudflare_accounts: " + accounts + "\n"
	}

	out += "    clusters:\n"
	for _, name := range []string{"kernel", "devel"} {
		if groups, ok := clusters[name]; ok {
			out += "      " + name + ":\n        groups:\n" + groups
		}
	}

	return out
}

func load(t *testing.T, cloudflareYAML, gatewaysYAML string) (*config.Config, error) {
	t.Helper()

	cat, err := catalog.Load(fstest.MapFS{catalog.FileName: {Data: []byte(gatewaysYAML)}})
	require.NoError(t, err)

	return config.Load(fstest.MapFS{config.FileName: {Data: []byte(cloudflareYAML)}}, cat, []catalog.Project{})
}

func TestLoadParsesTheRegistry(t *testing.T) {
	c, err := load(t, registry, gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	}))
	require.NoError(t, err)

	assert.Equal(t, []string{"partner", "platform"}, sortedKeys(c.Accounts))
	assert.False(t, c.Accounts["partner"].LegacyNames)
	assert.Equal(t, "partner", c.Zones["partner.test"].Account)
	assert.Equal(t, &config.TotalTLSCfg{Enabled: true}, c.Zones["partner.test"].TotalTLS)
	assert.Nil(t, c.Zones["example.com"].TotalTLS, "absent total_tls leaves the setting unmanaged")
}

// A second account on a cluster is a second tunnel with suffixed names:
// what adding one creates, next to the untouched legacy tunnel.
func TestSecondAccountIsASecondTunnel(t *testing.T) {
	c, err := load(t, registry, gateways("[platform, partner]", map[string]string{
		"kernel": "          a: {domains: [a.example.com, a.partner.test], route_namespaces: [a]}\n",
		"devel":  "          b: {domains: ['*.partner.test', b.partner.test], route_namespaces: [b]}\n",
	}))
	require.NoError(t, err)

	type tunnel struct {
		name, release, secret, zone string
		hosts                       []string
	}

	var got []tunnel
	for _, tun := range c.Tunnels {
		got = append(got, tunnel{tun.Name(), tun.Release(), tun.SecretName(), tun.Zone, tun.Hosts})
	}

	assert.Equal(t, []tunnel{
		{"devel-partner", "cloudflared-partner", "cloudflared-partner-tunnel-token", "partner.test",
			[]string{"b.partner.test", "*.partner.test"}},
		{"kernel-partner", "cloudflared-partner", "cloudflared-partner-tunnel-token", "partner.test",
			[]string{"a.partner.test"}},
		{"kernel", "cloudflared", "cloudflared-tunnel-token", "example.com",
			[]string{"a.example.com"}},
	}, got)
}

func TestDomainMustLieInAZoneOfTheExposuresAccount(t *testing.T) {
	t.Run("in no zone", func(t *testing.T) {
		_, err := load(t, registry, gateways("[platform]", map[string]string{
			"kernel": "          a: {domains: [a.nowhere.test], route_namespaces: [a]}\n",
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"a.nowhere.test" lies in no zone`)
	})

	t.Run("zone of another account than the exposure's", func(t *testing.T) {
		_, err := load(t, registry, gateways("[platform]", map[string]string{
			"kernel": "          a: {domains: [a.partner.test], route_namespaces: [a]}\n",
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `"a.partner.test" is in zone partner.test of account "partner"`)
	})

	t.Run("wildcard resolves to the zone under its star", func(t *testing.T) {
		c, err := load(t, registry, gateways("[platform]", map[string]string{
			"kernel": "          a: {domains: ['*.example.com'], route_namespaces: [a]}\n",
		}))
		require.NoError(t, err)
		assert.Equal(t, "example.com", c.Tunnels[0].Zone)
	})

	t.Run("longest zone wins", func(t *testing.T) {
		// sub.example.com delegated to the partner account.
		nested := replaceOnce(t, registry, "  other.test:", "  sub.example.com: {account: partner, zone_id: z4, ssl_mode: full}\n  other.test:")

		_, err := load(t, nested, gateways("[platform]", map[string]string{
			"kernel": "          a: {domains: [a.sub.example.com], route_namespaces: [a]}\n",
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `in zone sub.example.com of account "partner"`)
	})

	t.Run("exposure lists no account", func(t *testing.T) {
		_, err := load(t, registry, gateways("", map[string]string{
			"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "lists no accounts")
	})

	t.Run("exposure lists an unknown account", func(t *testing.T) {
		_, err := load(t, registry, gateways("[platform, nobody]", map[string]string{
			"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
		}))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `no account "nobody"`)
	})
}

// A tunnel may serve names in several zones of its account (truvity/cloudflare
// v2.20.0 per-host zones): each host's CNAME goes in its own zone.
func TestTunnelSpanningTwoZonesCarriesBoth(t *testing.T) {
	c, err := load(t, registry, gateways("[partner]", map[string]string{
		"kernel": "          a: {domains: [a.partner.test, a.other.test], route_namespaces: [a]}\n",
	}))
	require.NoError(t, err)
	require.Len(t, c.Tunnels, 1)
	assert.Equal(t, []string{"other.test", "partner.test"}, c.Tunnels[0].Zones)
	assert.True(t, c.Tunnels[0].CarriesIn("other.test", "a.other.test"))
	assert.True(t, c.Tunnels[0].CarriesIn("partner.test", "a.partner.test"))
	assert.False(t, c.Tunnels[0].CarriesIn("partner.test", "a.other.test"))
}

func TestRecordsLoadAsTheLibrarysRecords(t *testing.T) {
	withRecords := replaceOnce(t, registry, "example.com: {account: platform, zone_id: z1, ssl_mode: full}",
		"example.com: {account: platform, zone_id: z1, ssl_mode: full, records: ["+
			"{name: lb.example.com, type: CNAME, content: lb-1.eu-west-1.elb.amazonaws.com, proxied: true}, "+
			"{name: docs.example.com, type: CNAME, content: d111.cloudfront.net, ttl: 300, comment: CloudFront}]}")
	c, err := load(t, withRecords, gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	}))
	require.NoError(t, err)

	recs := c.Zones["example.com"].RecordsLibrary("example.com")
	require.NotNil(t, recs)
	assert.Equal(t, "example.com", recs.Zone)
	require.Len(t, recs.Entries, 2)
	assert.Equal(t, "lb.example.com", recs.Entries[0].Name)
	assert.True(t, recs.Entries[0].Proxied)
	assert.Equal(t, 300, recs.Entries[1].TTL)
	assert.Equal(t, "CloudFront", recs.Entries[1].Comment)

	assert.Nil(t, c.Zones["partner.test"].RecordsLibrary("partner.test"), "a zone without records manages none")
}

func TestHostnameIsUniqueAcrossEveryTunnel(t *testing.T) {
	// The catalog allows one name on two clusters (a migration); two
	// tunnels cannot both own its CNAME.
	_, err := load(t, registry, gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
		"devel":  "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	}))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already declared")
}

func TestExactBeforeWildcardIsDerivedPerTunnel(t *testing.T) {
	c, err := load(t, registry, gateways("[platform, partner]", map[string]string{
		"kernel": "          w: {domains: ['*.example.com', '*.b.example.com'], route_namespaces: [w]}\n" +
			"          x: {domains: [x.b.example.com, '*.partner.test', x.partner.test], route_namespaces: [x]}\n",
	}))
	require.NoError(t, err)

	got := map[string][]string{}
	for _, tun := range c.Tunnels {
		got[tun.Name()] = tun.Hosts
	}

	assert.Equal(t, map[string][]string{
		"kernel":         {"x.b.example.com", "*.b.example.com", "*.example.com"},
		"kernel-partner": {"x.partner.test", "*.partner.test"},
	}, got)
}

func TestRegistryValidation(t *testing.T) {
	good := gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	})

	for name, tc := range map[string]struct{ old, new, zone, want string }{
		"zone names an unknown account": {
			old:  "  example.com: {account: platform,",
			new:  "  example.com: {account: nobody,",
			want: `zones[example.com].account: no account "nobody"`,
		},
		"zone without zone_id": {
			old:  "zone_id: z1, ",
			new:  "",
			want: "zones[example.com].zone_id is required",
		},
		"ssl_mode not encrypting": {
			old:  "zone_id: z1, ssl_mode: full",
			new:  "zone_id: z1, ssl_mode: flexible",
			want: `ssl_mode: "flexible" is not allowed (valid: full, strict)`,
		},
		"account without id": {
			old:  "account_id: a2",
			new:  "account_id: ''",
			want: "accounts[partner].account_id is required",
		},
		"two legacy_names accounts": {
			old:  "account_id: a2",
			new:  "account_id: a2\n    legacy_names: true",
			want: "all set legacy_names",
		},
		"account name not an object name": {
			old:  "  partner:\n",
			new:  "  Partner:\n",
			want: "accounts[Partner]",
		},
		"account company not a label": {
			old:  "account_id: a2",
			new:  "account_id: a2\n    company: Not A Label",
			want: "accounts[partner].company",
		},
		"trusted host outside the zone": {
			zone: "trusted_clients: {hosts: [a.other.test], ranges: [10.0.0.0/24]}",
			want: "inside example.com",
		},
		"trusted range with host bits": {
			zone: "trusted_clients: {hosts: [a.example.com], ranges: [10.0.0.1/24]}",
			want: "no host bits set",
		},
		"trusted range is everything": {
			zone: "trusted_clients: {hosts: [a.example.com], ranges: [0.0.0.0/0]}",
			want: "too broad",
		},
		"trusted skip unknown": {
			zone: "trusted_clients: {hosts: [a.example.com], ranges: [10.0.0.0/24], skip: [http_request_waf]}",
			want: "is not a phase",
		},
		"trusted clients without ranges": {
			zone: "trusted_clients: {hosts: [a.example.com]}",
			want: "non-empty",
		},
		"rate limit host outside the zone": {
			zone: "rate_limits: {plan: pro, rules: [{name: r, endpoints: [{host: a.other.test, path: /x}], " +
				"period: 10, requests: 5, mitigation_timeout: 10, action: block}]}",
			want: "must be the zone example.com or a name inside it",
		},
		"rate limit period the plan lacks": {
			zone: "rate_limits: {plan: pro, rules: [{name: r, endpoints: [{host: a.example.com, path: /x}], " +
				"period: 30, requests: 5, mitigation_timeout: 10, action: block}]}",
			want: "period 30 is not available on the pro plan",
		},
		"rate limit method on pro": {
			zone: "rate_limits: {plan: pro, rules: [{name: r, endpoints: [{host: a.example.com, path: /x, methods: [POST]}], " +
				"period: 10, requests: 5, mitigation_timeout: 10, action: block}]}",
			want: "methods needs the business plan",
		},
		"rate limit plan missing": {
			zone: "rate_limits: {rules: [{name: r, endpoints: [{host: a.example.com, path: /x}], period: 10, requests: 5, mitigation_timeout: 10, action: block}]}",
			want: "rateLimits.plan",
		},
		"rate limit unknown field": {
			zone: "rate_limits: {plan: pro, rules: [{name: r, endpoints: [{host: a.example.com, path: /x}], " +
				"period: 10, burst: 3, requests: 5, mitigation_timeout: 10, action: block}]}",
			want: "field burst not found",
		},
		"origin key outside the zone": {
			zone: "origin_cnames: {a.other.test: ''}",
			want: "lowercase exact hostname inside zone example.com",
		},
		"origin is not an api gateway": {
			zone: "origin_cnames: {a.example.com: TODO-domain-target}",
			want: "not an API Gateway regional domain name",
		},
		"origin host on no tunnel": {
			zone: "origin_cnames: {nope.example.com: ''}",
			want: "no tunnel carries this host",
		},
		"record on a tunnel host": {
			zone: "records: [{name: a.example.com, type: CNAME, content: lb.example.net}]",
			want: "records[a.example.com]: tunnel kernel already carries this host",
		},
		"record outside the zone": {
			zone: "records: [{name: b.other.test, type: CNAME, content: lb.example.net}]",
			want: "zones[example.com].records: zone \"example.com\": records.entries[0]",
		},
		"record type not proxiable": {
			zone: "records: [{name: b.example.com, type: MX, content: mx.example.net}]",
			want: "type must be one of",
		},
		"proxied record with a ttl": {
			zone: "records: [{name: b.example.com, type: CNAME, content: lb.example.net, proxied: true, ttl: 300}]",
			want: "leave ttl at 0",
		},
		"ssl rule host outside the zone": {
			zone: "ssl_mode_rules: {a.other.test: strict}",
			want: "ssl_mode_rules[a.other.test]: the key must be a lowercase exact hostname inside zone example.com",
		},
		"ssl rule mode not encrypting": {
			zone: "ssl_mode_rules: {a.example.com: flexible}",
			want: `"flexible" is not allowed (valid: full, strict)`,
		},
		"config import without rules": {
			zone: "config_rules_import_id: 65d99d74d5cf427dba3a06cb487dd2f0",
			want: "set without ssl_mode_rules",
		},
		"config import not a ruleset id": {
			zone: "ssl_mode_rules: {a.example.com: strict}, config_rules_import_id: default",
			want: "is not a ruleset id",
		},
		"ref for a host without a rule": {
			zone: "ssl_mode_rules: {a.example.com: strict}, ssl_mode_rule_refs: {b.example.com: 008b1e0703ef47e59f058306af99887a}",
			want: "no such host in ssl_mode_rules",
		},
		"ref not hex": {
			zone: "ssl_mode_rules: {a.example.com: strict}, ssl_mode_rule_refs: {a.example.com: nope}",
			want: "is not a rule ref",
		},
		"public cache without cache": {
			zone: "public_cache: {host: a.example.com}",
			want: "set without cache",
		},
		"public cache host outside": {
			zone: "cache: {}, public_cache: {host: a.other.test}",
			want: "inside zone example.com",
		},
		"public cache host empty": {
			zone: "cache: {}, public_cache: {ttl: 5m}",
			want: "inside zone example.com",
		},
		"public cache ttl not a duration": {
			zone: "cache: {}, public_cache: {host: a.example.com, ttl: soon}",
			want: "public_cache.ttl",
		},
		"public cache ttl too long": {
			zone: "cache: {}, public_cache: {host: a.example.com, ttl: 2h}",
			want: "between 1s and 1h0m0s",
		},
		"public cache ttl fractional": {
			zone: "cache: {}, public_cache: {host: a.example.com, ttl: 1500ms}",
			want: "whole seconds",
		},
		"public cache unknown field": {
			zone: "cache: {}, public_cache: {host: a.example.com, paths: [/x]}",
			want: "field paths not found",
		},
		"unknown field": {
			old:  "legacy_names: true",
			new:  "legacy_names: true\n    tunnel_name: x",
			want: "field tunnel_name not found",
		},
		"hsts preload": {
			zone: "always_use_https: true, hsts: {enabled: true, max_age: 31536000, include_subdomains: true, preload: true}",
			want: "no preload",
		},
		"hsts without https": {
			zone: "hsts: {enabled: true, max_age: 300}",
			want: "needs always_use_https: true",
		},
		"hsts with https off": {
			zone: "always_use_https: false, hsts: {enabled: true, max_age: 300}",
			want: "needs always_use_https: true",
		},
		"hsts max age too long": {
			zone: "always_use_https: true, hsts: {enabled: true, max_age: 63072000}",
			want: "hsts.maxAge",
		},
		"bot management unknown ai_bots": {
			zone: "bot_management: {ai_bots: challenge}",
			want: "botManagement.aiBots",
		},
		"bot management empty": {
			zone: "bot_management: {}",
			want: "sets nothing",
		},
		"bot management unknown field": {
			zone: "bot_management: {ai_bots: block, fight_mode: true}",
			want: "field fight_mode not found",
		},
		"managed waf empty": {
			zone: "managed_waf: {hosts: []}",
			want: "empty host list",
		},
		"managed waf host outside zone": {
			zone: "managed_waf: {hosts: [a.other.test]}",
			want: "inside example.com",
		},
		"managed waf hosts and whole zone": {
			zone: "managed_waf: {hosts: [a.example.com], whole_zone: true}",
			want: "exclusive",
		},
		"security.txt without contact": {
			zone: "security_txt: {contact: [], expires: '2027-10-13T00:00:00Z'}",
			want: "at least one contact",
		},
		"security.txt contact not a uri": {
			zone: "security_txt: {contact: [security@example.com], expires: '2027-10-13T00:00:00Z'}",
			want: "mailto: address or an https:// URL",
		},
		"security.txt expires not rfc3339": {
			zone: "security_txt: {contact: ['mailto:a@example.com'], expires: 2027-10-13}",
			want: "RFC 3339",
		},
	} {
		t.Run(name, func(t *testing.T) {
			old, replacement := tc.old, tc.new
			if tc.zone != "" {
				old, replacement = "zone_id: z1, ssl_mode: full", "zone_id: z1, ssl_mode: full, "+tc.zone
			}

			_, err := load(t, replaceOnce(t, registry, old, replacement), good)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// validConfig returns a minimal config that passes Validate; tests set
// the kernel tunnel's hosts (in ingress order) to probe the routing
// invariants.
func validConfig(hosts ...string) *config.Config {
	return &config.Config{
		Accounts: map[string]config.Account{
			"platform": {AccountID: "account-id", LegacyNames: true},
		},
		Zones: map[string]config.Zone{
			"example.com": {Account: "platform", ZoneID: "zone-id", SSLMode: "full"},
		},
		Fleets: config.FleetConfig{
			Internal: config.FleetEntry{Service: "gateway-internal", Namespace: "envoy-gateway-system"},
		},
		Tunnels: []config.Tunnel{
			{Cluster: "kernel", Account: "platform", LegacyNames: true, Zone: "example.com", Hosts: hosts},
		},
	}
}

func TestValidate_DomainRouting(t *testing.T) {
	t.Run("exact hosts before wildcard is valid", func(t *testing.T) {
		c := validConfig("argocd.example.com", "*.example.com")
		assert.NoError(t, c.Validate())
	})

	t.Run("deeper wildcard before broader wildcard is valid", func(t *testing.T) {
		c := validConfig("*.kernel.example.com", "*.example.com")
		assert.NoError(t, c.Validate())
	})

	t.Run("deeper wildcard shadowed by broader wildcard", func(t *testing.T) {
		// cloudflared's `*` spans dots: *.example.com already catches
		// every a.kernel.example.com.
		c := validConfig("*.example.com", "*.kernel.example.com")
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shadowed")
		assert.Contains(t, err.Error(), "*.kernel.example.com")
	})

	t.Run("exact host shadowed by earlier wildcard", func(t *testing.T) {
		c := validConfig("*.example.com", "argocd.example.com")
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shadowed")
		assert.Contains(t, err.Error(), "argocd.example.com")
	})

	t.Run("deeper exact host shadowed by earlier wildcard", func(t *testing.T) {
		c := validConfig("*.example.com", "a.eudi.devel.example.com")
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shadowed")
	})

	t.Run("OrderHosts output always validates", func(t *testing.T) {
		c := validConfig(edgespec.OrderHosts([]string{
			"*.example.com", "*.devel.example.com", "a.eudi.devel.example.com", "*.eudi.devel.example.com", "argocd.example.com",
		})...)
		assert.NoError(t, c.Validate())
	})

	t.Run("no hosts", func(t *testing.T) {
		c := validConfig()
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no hostname")
	})

	t.Run("host outside the tunnel's zone", func(t *testing.T) {
		c := validConfig("argocd.example.net")
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not the tunnel's zone")
	})

	t.Run("duplicate host within tunnel", func(t *testing.T) {
		c := validConfig("argocd.example.com", "argocd.example.com", "*.example.com")
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already declared")
	})

	t.Run("duplicate host across tunnels", func(t *testing.T) {
		c := validConfig("argocd.example.com", "*.example.com")
		c.Tunnels = append(c.Tunnels, config.Tunnel{
			Cluster: "devel", Account: "platform", LegacyNames: true, Zone: "example.com",
			Hosts: []string{"argocd.example.com"},
		})
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already declared")
	})

	t.Run("case-insensitive duplicate", func(t *testing.T) {
		c := validConfig("ArgoCD.example.com", "argocd.example.com")
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already declared")
	})

	t.Run("tunnel in a zone of another account", func(t *testing.T) {
		c := validConfig("argocd.example.com")
		c.Accounts["partner"] = config.Account{AccountID: "p"}
		c.Tunnels[0].Account = "partner"
		c.Tunnels[0].LegacyNames = false
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot cross accounts")
	})

	t.Run("tunnel of an unknown account", func(t *testing.T) {
		c := validConfig("argocd.example.com")
		c.Tunnels[0].Account = "nobody"
		err := c.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no such account")
	})
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	slices.Sort(out)

	return out
}

func replaceOnce(t *testing.T, s, old, replacement string) string {
	t.Helper()
	require.Contains(t, s, old, "fixture has no %q", old)

	return strings.Replace(s, old, replacement, 1)
}

// TestCacheHostsArePinned is the zone's cache policy, literally. The list is
// derived from the edge catalog, so a catalog edit that makes a host cacheable
// -- or stops it being so -- shows up HERE, in a diff a person reads, rather
// than at the edge where nobody would look until a stale page was served.
//
// EVERY NAME BELOW IS A BUSINESS PROJECT'S. If a console ever appears in
// this list, that is the bug: a platform chart is somebody else's, on
// somebody else's release cycle, and nothing makes its bundle immutable.
func TestCacheHostsArePinned(t *testing.T) {
	t.Parallel()

	zone, ok := fixture(t).Zones["example.com"]
	require.True(t, ok)
	require.NotNil(t, zone.Cache, "the zone declares a cache policy")

	assert.Equal(t, []string{
		"dms.devel.example.com",
		"dms.prod.example.com",
		"dms.stage.example.com",
		"eudi.devel.example.com",
		"eudi.prod.example.com",
		"eudi.stage.example.com",
		"url-shortener.devel.example.com",
		"url-shortener.prod.example.com",
		"url-shortener.stage.example.com",
	}, zone.CacheHosts)

	// The consoles, named so the reason is not only in a comment.
	for _, console := range []string{
		"example.com",
		"access.example.com",
		"argocd.example.com",
		"gemaal.example.com",
		"headlamp.kernel.example.com",
		"headlamp.devel.example.com",
	} {
		assert.NotContains(t, zone.CacheHosts, console,
			"%s is a platform console: its chart is not ours to make immutable", console)
	}

	// A wildcard covers names other groups serve -- headlamp.devel is inside
	// *.devel -- and a suffix test cannot say "except those".
	for _, host := range zone.CacheHosts {
		assert.NotContains(t, host, "*", "a wildcard reached the cache list")
	}

	// A zone without a cache policy has no cache hosts, whatever it serves.
	assert.Empty(t, fixture(t).Zones["example.org"].CacheHosts)
}

func TestACacheZoneNeedsTheProjectList(t *testing.T) {
	_, err := config.Load(os.DirFS("testdata"), fixtureCatalog(t), nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pass the project list")
}

// TestTrustedClientsArePinned is the firewall exemption, literally: who is
// skipped, and where. A widened range or host shows up here, in a diff a
// person reads.
func TestTrustedClientsArePinned(t *testing.T) {
	t.Parallel()

	tc := fixture(t).Zones["example.com"].TrustedClients
	require.NotNil(t, tc)
	assert.Equal(t, []string{"mcp.example.com", "access.example.com"}, tc.Hosts)
	assert.Equal(t, []string{"192.0.2.0/24"}, tc.Ranges)
}

// TestRateLimitsArePinned is the zone's rate limiting policy, literally:
// which endpoints are protected and how hard. A removed or loosened rule shows
// up in a diff a person reads.
func TestRateLimitsArePinned(t *testing.T) {
	t.Parallel()

	rl := fixture(t).Zones["example.com"].RateLimits
	require.NotNil(t, rl)
	assert.Equal(t, "pro", rl.Plan)
	require.Len(t, rl.Rules, 2, "Pro holds two rules for the zone")

	rule := rl.Rules[0]
	assert.Equal(t, []config.EndpointCfg{
		{Host: "url-shortener.devel.example.com", Path: "/faro/collect"},
		{Host: "url-shortener.stage.example.com", Path: "/faro/collect"},
		{Host: "url-shortener.prod.example.com", Path: "/faro/collect"},
		{Host: "access.example.com", Path: "/login/recovery"},
	}, rule.Endpoints)
	assert.Equal(t, []string{"ip.src"}, rule.Characteristics)
	assert.Equal(t, 10, rule.Period)
	assert.Equal(t, 20, rule.Requests)
	assert.Equal(t, 10, rule.MitigationTimeout)
	assert.Equal(t, "block", rule.Action)

	// Rule 2, machine ingest.
	rule = rl.Rules[1]
	assert.Equal(t, []config.EndpointCfg{
		{Host: "otlp.devel.example.com", PathPrefix: "/v1/"},
		{Host: "otlp.example.com", PathPrefix: "/v1/"},
		{Host: "alert-ingress.example.com", Path: "/"},
		{Host: "access.example.com", Path: "/token"},
	}, rule.Endpoints)
	assert.Equal(t, 60, rule.Period)
	assert.Equal(t, 600, rule.Requests)
	assert.Equal(t, 60, rule.MitigationTimeout)
	assert.Equal(t, "block", rule.Action)
}

// TestAccessEdgeSwitchPointsAtTheOrigin pins the state of the origin switch:
// access.example.com leaves its tunnel for an API Gateway custom domain, and
// only that host is active.
func TestAccessEdgeSwitchPointsAtTheOrigin(t *testing.T) {
	t.Parallel()

	c := fixture(t)

	const target = "d-abcdef1234.execute-api.eu-central-1.amazonaws.com"

	zone := c.Zones["example.com"]
	assert.Equal(t, map[string]string{"access.example.com": target}, zone.OriginCNAMEs)
	assert.Equal(t, map[string]string{"access.example.com": target}, zone.ActiveOrigins())

	for name, z := range c.Zones {
		if name != "example.com" {
			assert.Empty(t, z.ActiveOrigins(), "zone %s: no other host leaves its tunnel", name)
		}
	}
}

// The zone hardening as declared: HTTPS redirect on both zones of the first
// account, HSTS not yet, security.txt on both, and the managed WAF's first
// step -- every name the devel tunnel carries in example.com, and no name of
// any other cluster. A new devel name outside *.devel.example.com fails here
// until it is added.
func TestZoneHardeningIsPinned(t *testing.T) {
	c := fixture(t)

	for _, name := range []string{"example.com", "example.org"} {
		z := c.Zones[name]
		require.NotNil(t, z.AlwaysUseHTTPS, name)
		assert.True(t, *z.AlwaysUseHTTPS, name)
		assert.Nil(t, z.HSTS, "%s: HSTS waits for the HTTPS redirect to run clean", name)
		require.NotNil(t, z.SecurityTxt, name)
		assert.Equal(t, []string{"mailto:security@example.com"}, z.SecurityTxt.Contact, name)

		if name != "example.com" {
			assert.Nil(t, z.ManagedWAF, "%s: the managed WAF is deferred here", name)
		}
	}

	zone := c.Zones["example.com"]
	require.NotNil(t, zone.ManagedWAF)
	assert.True(t, zone.ManagedWAF.Enabled)
	assert.Empty(t, zone.ManagedWAF.Hosts, "enabled derives the hosts; none are authored")

	// Every exact name a tunnel carries in the zone is covered, in every
	// cluster; the two opted-out wildcards are the only names left out.
	for _, tun := range c.Tunnels {
		for _, host := range tun.Hosts {
			if c.ZoneOf(host) != "example.com" {
				continue
			}

			if strings.HasPrefix(host, "*.") {
				assert.NotContains(t, zone.WAFHosts, host)

				continue
			}

			assert.Contains(t, zone.WAFHosts, host, "every exact proxied name is covered: %s", host)
		}
	}

	for _, h := range zone.WAFHosts {
		assert.NotContains(t, h, "*")
	}

	assert.True(t, sort.StringsAreSorted(zone.WAFHosts))
}

// TestPublicCacheIsPinned: the installation host's public documents are
// cached for five minutes, on top of the zone's cache policy.
func TestPublicCacheIsPinned(t *testing.T) {
	z := fixture(t).Zones["example.com"]
	require.NotNil(t, z.PublicCache)
	require.NotNil(t, z.Cache, "the rule extends the zone's cache ruleset")
	assert.Equal(t, "access.example.com", z.PublicCache.Host)
	assert.Equal(t, 5*time.Minute, z.PublicCache.EdgeTTL())
	assert.Zero(t, (*config.PublicCacheCfg)(nil).EdgeTTL())
}

// The fixture is the file an estate has, so its round trip is the contract
// with the estate: Parse reads exactly the file Load reads, strictly, and the
// registry it returns is the one Load validates.
func TestTheFixtureRoundTrips(t *testing.T) {
	data := readFixture(t, config.FileName)

	parsed, err := config.Parse(data)
	require.NoError(t, err)
	assert.Empty(t, parsed.Tunnels, "Parse derives nothing")

	loaded := fixture(t)
	assert.Equal(t, parsed.Accounts, loaded.Accounts)
	assert.Equal(t, parsed.Fleets, loaded.Fleets)
	assert.Equal(t, parsed.Root, loaded.Root)
	require.NoError(t, loaded.Validate())

	for name, z := range parsed.Zones {
		lz := loaded.Zones[name]
		lz.CacheHosts = nil
		lz.WAFHosts = nil
		assert.Equal(t, z, lz, name)
	}

	_, err = config.Parse(append(data, []byte("surprise: 1\n")...))
	require.Error(t, err, "an unknown top-level key fails")

	_, err = config.Parse(nil)
	require.Error(t, err, "an empty file fails")

	_, err = config.Load(fstest.MapFS{}, fixtureCatalog(t), fixtureProjects)
	require.Error(t, err, "a missing file fails")

	_, err = config.Load(os.DirFS("testdata"), nil, fixtureProjects)
	require.Error(t, err, "a missing catalog fails")
}

func TestStackNamesFollowTheAccount(t *testing.T) {
	c := fixture(t)

	legacy, ok := c.LegacyAccount()
	require.True(t, ok)
	assert.Equal(t, "main", legacy)

	assert.Equal(t, "cloudflare-edge", c.StackName(config.StackEdge, "main"))
	assert.Equal(t, "cloudflare-root-partner", c.StackName(config.StackRoot, "partner"))

	for stack, want := range map[string]string{
		"cloudflare-edge":         "main",
		"cloudflare-edge-partner": "partner",
	} {
		got, err := c.StackAccount(stack, config.StackEdge)
		require.NoError(t, err, stack)
		assert.Equal(t, want, got, stack)
	}

	for stack, want := range map[string]string{
		"cloudflare-r2-main":   "is the legacy_names account",
		"cloudflare-r2-nobody": `no account "nobody"`,
		"cloudflare-root":      "is not a r2 stack",
		"other":                "is not a r2 stack",
	} {
		_, err := c.StackAccount(stack, config.StackR2)
		require.Error(t, err, stack)
		assert.Contains(t, err.Error(), want, stack)
	}
}

func TestForAccountIsOneAccountsView(t *testing.T) {
	c := fixture(t)

	partner, err := c.ForAccount("partner")
	require.NoError(t, err)
	assert.Equal(t, []string{"example.net"}, sortedKeys(partner.Zones))
	assert.Equal(t, []string{"partner"}, sortedKeys(partner.Accounts))
	require.Len(t, partner.Tunnels, 3)

	for _, tun := range partner.Tunnels {
		assert.Equal(t, "partner", tun.Account)
	}

	main, err := c.ForAccount("main")
	require.NoError(t, err)
	assert.Equal(t, []string{"example.com", "example.org"}, sortedKeys(main.Zones))
	assert.Equal(t, c.Root, main.Root, "the legacy_names account's rotations are the top-level block")

	_, err = c.ForAccount("nobody")
	require.Error(t, err)

	pending := replaceOnce(t, string(readFixture(t, config.FileName)),
		`    zone_id: "3333aaaa4444bbbb5555cccc6666dddd"`, "    zone_id_pending: true")
	pc, err := config.Load(fstest.MapFS{config.FileName: {Data: []byte(pending)}}, fixtureCatalog(t), fixtureProjects)
	require.NoError(t, err, "a pending zone id loads: the account's stacks are what refuse")
	_, err = pc.ForAccount("partner")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "zone_id_pending")

	_, err = pc.ForAccount("main")
	require.NoError(t, err, "another account's stacks never see the pending zone")
}

func TestRootRotationsDefaultAndOverride(t *testing.T) {
	data := replaceOnce(t, string(readFixture(t, config.FileName)), "  partner:\n    account_id:",
		"  partner:\n    root: {edge: {rotation: \"2\"}}\n    account_id:")
	c, err := config.Parse([]byte(data))
	require.NoError(t, err)

	assert.Equal(t, "2", c.RootFor("partner").Edge.Rotation)
	assert.Equal(t, "1", c.RootFor("partner").Status.Rotation, "a child left out takes the default")
	assert.Equal(t, c.Root, c.RootFor("main"))

	bare := replaceOnce(t, string(readFixture(t, config.FileName)), "root:\n  edge:", "x-root:\n  edge:")
	_, err = config.Parse([]byte(bare))
	require.Error(t, err)

	c2, err := config.Parse([]byte(strings.SplitN(string(readFixture(t, config.FileName)), "\nr2: {}", 2)[0]))
	require.NoError(t, err)
	assert.Equal(t, "1", c2.Root.R2Parent.Rotation, "a file with no root block gets the default rotation")
}

func TestForeignHostsMustLieInADeclaredZone(t *testing.T) {
	c := fixture(t)

	require.NoError(t, c.ValidateForeignHosts(map[string][]string{"stage": {"ssi.example.org"}, "prod": nil}))
	assert.Equal(t, "example.org", c.ZoneOf("ssi.example.org"))

	err := c.ValidateForeignHosts(map[string][]string{"stage": {"ssi.example.invalid"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `foreign host "ssi.example.invalid" lies in no zone`)
	assert.Contains(t, err.Error(), "environment stage")

	assert.Error(t, c.ValidateForeignHosts(nil))
}

// A FOREIGN HOST IS AN EXACT HOST OF ITS ENVIRONMENT'S TUNNEL, ITS CNAME IN
// ITS OWN ZONE. The fixture's stage serves ssi.example.org (zone example.org,
// account main), beside the names in example.com. The per-host zones keep both
// on the one stage tunnel: the host is listed as itself, never as a wildcard
// of its zone, and its zone joins the tunnel's Zones so the CNAME lands there.
func TestForeignHostIsAnExactTunnelHostInItsZone(t *testing.T) {
	c := fixture(t)

	var stage *config.Tunnel

	for i := range c.Tunnels {
		if c.Tunnels[i].Cluster == "stage" && c.Tunnels[i].Account == "main" {
			stage = &c.Tunnels[i]
		}
	}

	require.NotNil(t, stage, "the fixture's stage tunnel of account main")
	assert.Equal(t, []string{"example.com", "example.org"}, stage.Zones)
	assert.Contains(t, stage.Hosts, "ssi.example.org")
	assert.True(t, stage.CarriesIn("example.org", "ssi.example.org"))
	assert.False(t, stage.CarriesIn("example.com", "ssi.example.org"))

	for _, host := range stage.Hosts {
		if c.ZoneOf(host) == "example.org" {
			assert.Equal(t, "ssi.example.org", host, "only the exact foreign host may name its zone")
		}
	}
}

// Contributions are the rules the zone's blocks ask for, built with the
// library's own builders: a caller assembles them and builds none by hand.
func TestContributionsAreTheBuildersOwn(t *testing.T) {
	c := fixture(t)

	got, err := c.Zones["example.com"].Contributions()
	require.NoError(t, err)

	var sources []string
	for _, contribution := range got {
		sources = append(sources, contribution.Source)
	}

	assert.Equal(t, []string{
		"public-paths:access.example.com",
		"ssl-mode:feed0001feed0002feed0003feed0004",
		"rate-limits",
		"trusted-clients",
		"managed-waf",
	}, sources)

	// The rate limit contribution is exactly the builder's on the file's rows.
	wantLimits, err := rules.RateLimits("pro", c.Zones["example.com"].RateLimits.Library().Rules)
	require.NoError(t, err)
	assert.Equal(t, wantLimits, got[2])

	wantPublic, err := rules.PublicPathsCache("access.example.com", rules.PublicPathsArgs{TTL: 5 * time.Minute})
	require.NoError(t, err)
	assert.Equal(t, wantPublic.Contribution(), got[0])

	wantSSL, err := rules.SSLMode("access.example.com", "strict", "feed0001feed0002feed0003feed0004")
	require.NoError(t, err)
	assert.Equal(t, wantSSL, got[1])

	// They assemble: one ordered ruleset per phase, no conflicting writer.
	assembled, err := rules.Assemble(got)
	require.NoError(t, err)
	assert.Len(t, assembled[rules.PhaseRateLimit], 2)
	assert.Len(t, assembled[rules.PhaseFirewallCustom], 1)
	assert.Len(t, assembled[rules.PhaseFirewallManaged], 1)
	assert.Len(t, assembled[rules.PhaseConfigSettings], 1)

	// A zone with none of the blocks contributes nothing.
	none, err := c.Zones["example.net"].Contributions()
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestSSLModeRuleRefDefaultsToTheHost(t *testing.T) {
	z := config.Zone{Name: "example.com", SSLModeRules: map[string]string{"a.example.com": "full", "b.example.com": "strict"},
		SSLModeRuleRefs: map[string]string{"b.example.com": "feed0001feed0002feed0003feed0004"}}

	assert.Equal(t, "ssl-mode-a.example.com", z.SSLModeRef("a.example.com"))
	assert.Equal(t, "feed0001feed0002feed0003feed0004", z.SSLModeRef("b.example.com"))

	got, err := z.Contributions()
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "ssl-mode:ssl-mode-a.example.com", got[0].Source)
}

func TestLibraryConvertersAreNilSafe(t *testing.T) {
	var (
		rl *config.RateLimitsCfg
		h  *config.HSTSCfg
		bm *config.BotManagementCfg
		w  *config.ManagedWAFCfg
		tc *config.TrustedClientsCfg
	)

	assert.Nil(t, rl.Library())
	assert.Nil(t, h.Library())
	assert.Nil(t, bm.Library())
	assert.Nil(t, w.Library("example.com"))
	assert.Nil(t, tc.Library("example.com"))
}

func TestCompanyGroupsAccounts(t *testing.T) {
	c := fixture(t)

	assert.Equal(t, "example", c.CompanyOf("main"))
	assert.Equal(t, "partner-co", c.CompanyOf("partner"))
	assert.Equal(t, []string{"main"}, c.AccountsOf("example"))
	assert.Equal(t, []string{"partner"}, c.AccountsOf("partner-co"))
	assert.Empty(t, c.AccountsOf("nobody"))

	// An account that sets no company is its own.
	a := c.Accounts["partner"]
	a.Company = ""
	c.Accounts["partner"] = a
	assert.Equal(t, "partner", c.CompanyOf("partner"))
	assert.Equal(t, []string{"partner"}, c.AccountsOf("partner"))
}

// TestNSRecordsLoadAsDelegations is the config path of a child-zone
// delegation: four target records for one owner, DNS-only, explicit TTL.
func TestNSRecordsLoadAsDelegations(t *testing.T) {
	delegation := "  example.com: {account: platform, zone_id: z1, ssl_mode: full, records: [" +
		"{name: team.example.com, type: NS, content: ns1.nameservers.example, ttl: 300}, " +
		"{name: team.example.com, type: NS, content: ns2.nameservers.example, ttl: 300}, " +
		"{name: team.example.com, type: NS, content: ns3.nameservers.example, ttl: 300}, " +
		"{name: team.example.com, type: NS, content: ns4.nameservers.example, ttl: 300}]}"
	withNS := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}", delegation)

	c, err := load(t, withNS, gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	}))
	require.NoError(t, err)

	recs := c.Zones["example.com"].RecordsLibrary("example.com")
	require.Len(t, recs.Entries, 4)

	for i, r := range recs.Entries {
		assert.Equal(t, "team.example.com", r.Name)
		assert.Equal(t, "NS", r.Type)
		assert.Equal(t, 300, r.TTL)
		assert.False(t, r.Proxied, "a delegation is DNS only")
		assert.Nil(t, r.WAF, "a delegation is never in the managed WAF")
		assert.Contains(t, r.Content, "ns"+string(rune('1'+i))+".")
	}
}

// TestNSDelegationRefusesTunnelHostsAtOrBelow: the child zone cut would hide
// a tunnel record exactly at the delegation or anywhere under it.
func TestNSDelegationRefusesTunnelHostsAtOrBelow(t *testing.T) {
	delegation := "  example.com: {account: platform, zone_id: z1, ssl_mode: full, records: " +
		"[{name: team.example.com, type: NS, content: ns1.nameservers.example, ttl: 300}]}"
	withNS := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}", delegation)

	for name, tc := range map[string]struct {
		host string
		want string
	}{
		"exact":             {"team.example.com", "at or below this NS delegation"},
		"descendant":        {"app.team.example.com", "at or below this NS delegation"},
		"wildcard child":    {"'*.team.example.com'", "at or below this NS delegation"},
		"another host":      {"other.example.com", ""},
		"parent of the cut": {"example.com", ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, withNS, gateways("[platform]", map[string]string{
				"kernel": "          a: {domains: [" + tc.host + "], route_namespaces: [a]}\n",
			}))
			if tc.want == "" {
				require.NoError(t, err)
				require.NotNil(t, c)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestRedirectsLoadAsTheLibrarysRedirects is the config path of a
// canonical-host redirect.
func TestRedirectsLoadAsTheLibrarysRedirects(t *testing.T) {
	block := "  example.com:\n" +
		"    account: platform\n    zone_id: z1\n    ssl_mode: full\n" +
		"    redirects:\n" +
		"      - {ref: www_to_apex, from: www.example.com, to: https://example.com, status: 301, preserve_query_string: true}\n" +
		"    redirect_rules_import_id: feed0001feed0002feed0003feed0004\n"
	withRedirect := replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}", block)

	c, err := load(t, withRedirect, gateways("[platform]", map[string]string{
		"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
	}))
	require.NoError(t, err)

	r := c.Zones["example.com"].RedirectsLibrary("example.com")
	require.NotNil(t, r)
	assert.Equal(t, "example.com", r.Zone)
	require.Len(t, r.Entries, 1)
	assert.Equal(t, "www_to_apex", r.Entries[0].Ref)
	assert.Equal(t, "www.example.com", r.Entries[0].From)
	assert.Equal(t, "https://example.com", r.Entries[0].To)
	assert.Equal(t, 301, r.Entries[0].Status)
	require.NotNil(t, r.Entries[0].PreserveQueryString)
	assert.True(t, *r.Entries[0].PreserveQueryString)
	assert.Equal(t, "feed0001feed0002feed0003feed0004", c.Zones["example.com"].RedirectRulesImportID)
}

func TestRedirectAndNSRefusalsAtLoad(t *testing.T) {
	bad := func(block string) string {
		return replaceOnce(t, registry, "  example.com: {account: platform, zone_id: z1, ssl_mode: full}", block)
	}

	for name, tc := range map[string]struct {
		block string
		want  string
	}{
		"redirect http target": {
			block: bad("  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n" +
				"    redirects: [{ref: www_to_apex, from: www.example.com, to: http://example.com}]\n"),
			want: "HTTPS",
		},
		"redirect loop": {
			block: bad("  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n" +
				"    redirects:\n" +
				"      - {ref: www_to_apex, from: www.example.com, to: https://example.com}\n" +
				"      - {ref: apex_to_www, from: example.com, to: https://www.example.com}\n"),
			want: "redirect loop",
		},
		"NS WAF reason": {
			block: bad("  example.com: {account: platform, zone_id: z1, ssl_mode: full, records: " +
				"[{name: team.example.com, type: NS, content: ns1.nameservers.example, ttl: 300, waf_reason: nope}]}"),
			want: "no WAF options",
		},
		"redirect import without redirects": {
			block: bad("  example.com: {account: platform, zone_id: z1, ssl_mode: full, " +
				"redirect_rules_import_id: feed0001feed0002feed0003feed0004}"),
			want: "nothing would adopt",
		},
		"redirect import bad id": {
			block: bad("  example.com:\n    account: platform\n    zone_id: z1\n    ssl_mode: full\n" +
				"    redirects: [{ref: www_to_apex, from: www.example.com, to: https://example.com}]\n" +
				"    redirect_rules_import_id: not-an-id\n"),
			want: "not a ruleset id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(t, tc.block, gateways("[platform]", map[string]string{
				"kernel": "          a: {domains: [a.example.com], route_namespaces: [a]}\n",
			}))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}
