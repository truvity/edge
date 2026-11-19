package edge

import (
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/tunnel"
	"github.com/truvity/edge/v4/cloudflare/zone"
)

func baseArgs(sink TokenSink) Args {
	return Args{
		Accounts: []Account{
			{Name: "platform", AccountID: "a1", Token: "token-platform"},
			{Name: "partner", AccountID: "a2", Token: "token-partner"},
		},
		Zones: []Zone{
			{Name: "example.test", Account: "platform", Args: zone.Args{ZoneID: "z1", SSL: "full"}},
			{Name: "partner.test", Account: "partner", Args: zone.Args{ZoneID: "z2", SSL: "full"}},
		},
		Tunnels: []Tunnel{
			{Name: "alpha", Cluster: "alpha", Account: "platform", Zone: "example.test", Hosts: []string{"a.example.test"}, DNSDomain: "cluster.example"},
			{Name: "alpha-partner", Cluster: "alpha", Account: "partner", Zone: "partner.test", Hosts: []string{"a.partner.test"}},
		},
		Origin: Origin{Service: "gateway-internal", Namespace: "gateways"},
		Sink:   sink,
	}
}

type sunk struct {
	mu    sync.Mutex
	names []string
}

func (s *sunk) sink(_ *pulumi.Context, t Tunnel, _ pulumi.StringOutput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.names = append(s.names, t.Name)

	return nil
}

func run(t *testing.T, args Args) (*mocks, error) {
	t.Helper()

	args.Logger = slog.New(slog.DiscardHandler)
	m := &mocks{}
	err := pulumi.RunErr(func(ctx *pulumi.Context) error { return Deploy(ctx, args) },
		pulumi.WithMocks("example", "edge", m))

	return m, err
}

func TestDeployRegistersOneProviderPerAccountAndOneTunnelPerEntry(t *testing.T) {
	s := &sunk{}
	m, err := run(t, baseArgs(s.sink))
	require.NoError(t, err)

	assert.Equal(t, []string{"provider-partner", "provider-platform"}, m.names("pulumi:providers:cloudflare"))
	assert.Equal(t, []string{"alpha", "alpha-partner"}, m.names("truvity:cloudflare:Tunnel"))
	assert.Equal(t, []string{"tunnel-secret-alpha", "tunnel-secret-alpha-partner"}, m.names("random:index/randomBytes:RandomBytes"))
	assert.Equal(t, []string{"alpha", "alpha-partner"}, s.names, "every tunnel token reaches the sink")

	tun := "cloudflare:index/zeroTrustTunnelCloudflared:ZeroTrustTunnelCloudflared"
	assert.Equal(t, "a2", m.input(tun, "tunnel-alpha-partner", "accountId"))

	record := "cloudflare:index/dnsRecord:DnsRecord"
	assert.Equal(t, "z2", m.input(record, "cname-alpha-partner-a-partner-test", "zoneId"))
	assert.Equal(t, "z1", m.input(record, "cname-alpha-a-example-test", "zoneId"))
}

func TestTunnelArgsOriginAndCAPool(t *testing.T) {
	args := baseArgs(nil)

	got := TunnelArgs(args, "a1", "z1", args.Tunnels[0])
	require.Len(t, got.Ingress, 1)
	assert.Equal(t, "https://gateway-internal.gateways.svc.cluster.example:443", got.Ingress[0].Service)
	assert.Equal(t, DefaultCAPoolPath, got.Ingress[0].CAPool)
	assert.Equal(t, "a.example.test", got.Ingress[0].OriginServerName)

	// No DNS domain: the convention.
	got = TunnelArgs(args, "a2", "z2", args.Tunnels[1])
	assert.Equal(t, "https://gateway-internal.gateways.svc.cluster.alpha:443", got.Ingress[0].Service)
}

func TestTunnelArgsOriginPort(t *testing.T) {
	args := baseArgs(nil)
	args.Origin.Port = 8443

	got := TunnelArgs(args, "a1", "z1", args.Tunnels[0])
	require.Len(t, got.Ingress, 1)
	assert.Equal(t, "https://gateway-internal.gateways.svc.cluster.example:8443", got.Ingress[0].Service)
	assert.Equal(t, "a.example.test", got.Ingress[0].OriginServerName, "the port does not change the origin SNI")

	inZones, err := TunnelArgsInZones(args, "a1", map[string]string{args.Tunnels[0].Zone: "z1"}, args.Tunnels[0])
	require.NoError(t, err)
	assert.Equal(t, got, inZones)

	// Zero is the default, so a caller that sets no port renders as before.
	args.Origin.Port = 0
	assert.Equal(t, TunnelArgs(baseArgs(nil), "a1", "z1", args.Tunnels[0]), TunnelArgs(args, "a1", "z1", args.Tunnels[0]))

	for _, bad := range []int{-1, 65536} {
		args.Origin.Port = bad

		_, err := TunnelArgsInZones(args, "a1", map[string]string{args.Tunnels[0].Zone: "z1"}, args.Tunnels[0])
		require.ErrorContains(t, err, "is not a TCP port")
	}
}

func TestDeployAppliesTheOptionHooks(t *testing.T) {
	args := baseArgs((&sunk{}).sink)

	var zones, tunnels, accounts []string

	args.AccountOptions = func(a Account) []pulumi.ResourceOption { accounts = append(accounts, a.Name); return nil }
	args.ZoneOptions = func(z Zone) []pulumi.ResourceOption { zones = append(zones, z.Name); return nil }
	args.TunnelOptions = func(t Tunnel) []pulumi.ResourceOption { tunnels = append(tunnels, t.Name); return nil }

	_, err := run(t, args)
	require.NoError(t, err)

	assert.Equal(t, []string{"partner", "platform"}, accounts)
	assert.Equal(t, []string{"example.test", "partner.test"}, zones)
	assert.Equal(t, []string{"alpha", "alpha-partner"}, tunnels)
}

func TestDeployRefusals(t *testing.T) {
	_, err := run(t, baseArgs(nil))
	require.ErrorContains(t, err, "TokenSink is required")

	args := baseArgs((&sunk{}).sink)
	args.Accounts[0].Token = ""
	_, err = run(t, args)
	require.ErrorContains(t, err, "no resolved API token")

	args = baseArgs((&sunk{}).sink)
	args.Tunnels[0].Zone = "missing.test"
	_, err = run(t, args)
	require.ErrorContains(t, err, `zone "missing.test" is not declared`)

	args = baseArgs((&sunk{}).sink)
	args.Zones[0].Account = "missing"
	_, err = run(t, args)
	require.ErrorContains(t, err, `account "missing" is not declared`)
}

type (
	recorded struct {
		typ, name string
		inputs    resource.PropertyMap
	}

	mocks struct {
		mu        sync.Mutex
		resources []recorded
	}
)

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resources = append(m.resources, recorded{args.TypeToken, args.Name, args.Inputs})

	return args.Name + "-id", args.Inputs.Copy(), nil
}

func (m *mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token == "cloudflare:index/getZeroTrustTunnelCloudflaredToken:getZeroTrustTunnelCloudflaredToken" {
		return resource.PropertyMap{"token": resource.NewStringProperty("tunnel-token")}, nil
	}

	return resource.PropertyMap{}, nil
}

func (m *mocks) names(typ string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []string

	for _, r := range m.resources {
		if r.typ == typ {
			out = append(out, r.name)
		}
	}

	slices.Sort(out)

	return out
}

func (m *mocks) input(typ, name, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.resources {
		if r.typ == typ && r.name == name {
			return r.inputs[resource.PropertyKey(key)].StringValue()
		}
	}

	return ""
}

// multiZoneArgs adds a second zone to the platform account and lets the
// alpha tunnel serve a name in it.
func multiZoneArgs(sink TokenSink) Args {
	args := baseArgs(sink)
	args.Zones = append(args.Zones, Zone{Name: "example.net", Account: "platform", Args: zone.Args{ZoneID: "z3", SSL: "full"}})
	args.Tunnels[0].Zones = []string{"example.net", "example.test"}
	args.Tunnels[0].Hosts = []string{"a.example.test", "api.example.net", "*.example.test"}

	return args
}

func TestDeployTunnelAcrossZones(t *testing.T) {
	m, err := run(t, multiZoneArgs((&sunk{}).sink))
	require.NoError(t, err)

	record := "cloudflare:index/dnsRecord:DnsRecord"
	assert.Equal(t, "z1", m.input(record, "cname-alpha-a-example-test", "zoneId"))
	assert.Equal(t, "z1", m.input(record, "cname-alpha-star-example-test", "zoneId"))
	assert.Equal(t, "z3", m.input(record, "cname-alpha-api-example-net", "zoneId"))
	assert.Equal(t, "z2", m.input(record, "cname-alpha-partner-a-partner-test", "zoneId"))

	// One tunnel per entry still, and no second ingress config.
	assert.Equal(t, []string{"alpha", "alpha-partner"}, m.names("truvity:cloudflare:Tunnel"))
	assert.Equal(t, []string{"ingress-alpha", "ingress-alpha-partner"},
		m.names("cloudflare:index/zeroTrustTunnelCloudflaredConfig:ZeroTrustTunnelCloudflaredConfig"))
}

func TestTunnelArgsInZones(t *testing.T) {
	args := multiZoneArgs(nil)
	ids := map[string]string{"example.test": "z1", "partner.test": "z2", "example.net": "z3"}

	got, err := TunnelArgsInZones(args, "a1", ids, args.Tunnels[0])
	require.NoError(t, err)
	assert.Empty(t, got.DNS.ZoneID)
	assert.Equal(t, []tunnel.DNSZone{{ZoneID: "z3", Name: "example.net"}, {ZoneID: "z1", Name: "example.test"}}, got.DNS.Zones)
	assert.Equal(t, args.Tunnels[0].Hosts, got.DNS.Names)

	var hosts []string
	for _, r := range got.Ingress {
		hosts = append(hosts, r.Hostname)
	}

	assert.Equal(t, args.Tunnels[0].Hosts, hosts, "ingress keeps the tunnel's host order")
	require.NoError(t, got.Validate())

	// One zone, given as Zone or as a one-entry Zones: exactly TunnelArgs.
	single := baseArgs(nil).Tunnels[0]
	want := TunnelArgs(args, "a1", "z1", single)

	got, err = TunnelArgsInZones(args, "a1", ids, single)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	single.Zones = []string{"example.test"}
	got, err = TunnelArgsInZones(args, "a1", ids, single)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	_, err = TunnelArgsInZones(args, "a1", map[string]string{"example.test": "z1"}, args.Tunnels[0])
	require.ErrorContains(t, err, `zone "example.net" is not declared`)
}

func TestDeployTunnelAcrossZonesRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Args)
		want   string
	}{
		"a zone of another account": {
			func(a *Args) { a.Tunnels[0].Zones = []string{"example.test", "partner.test"} },
			`zone "partner.test" belongs to account "partner", not the tunnel's account "platform"`,
		},
		"a host outside the tunnel's zones": {
			func(a *Args) { a.Tunnels[0].Hosts = append(a.Tunnels[0].Hosts, "api.example.org") },
			`"api.example.org" lies in none of the tunnel's zones`,
		},
		"zones dropped on the way": {
			func(a *Args) { a.Tunnels[0].Zones = nil },
			`"api.example.net" lies in none of the tunnel's zones (example.test)`,
		},
		"zone not among zones": {
			func(a *Args) { a.Tunnels[0].Zone = "partner.test" },
			`zone "partner.test" is not among the tunnel's zones`,
		},
		"an undeclared zone": {
			func(a *Args) { a.Tunnels[0].Zones = []string{"example.test", "missing.test"} },
			`zone "missing.test" is not declared`,
		},
		"the same name twice": {
			func(a *Args) { a.Tunnels[0].Hosts = append(a.Tunnels[0].Hosts, "API.example.net") },
			`is already dns.names[1]`,
		},
	} {
		args := multiZoneArgs((&sunk{}).sink)
		tc.mutate(&args)

		_, err := run(t, args)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", name, err, tc.want)
		}
	}
}

func TestTunnelArgsLeavesOutDNSElsewhere(t *testing.T) {
	args := baseArgs(nil)
	tun := args.Tunnels[0]
	tun.Hosts = []string{"a.example.test", "b.example.test"}
	tun.DNSElsewhere = []string{"a.example.test"}

	got := TunnelArgs(args, "a1", "z1", tun)
	require.NotNil(t, got.DNS)
	assert.Equal(t, []string{"b.example.test"}, got.DNS.Names)
	assert.Len(t, got.Ingress, 2, "the name keeps its ingress rule")

	tun.DNSElsewhere = tun.Hosts
	got = TunnelArgs(args, "a1", "z1", tun)
	assert.Nil(t, got.DNS, "no names, no DNS block")
	assert.Len(t, got.Ingress, 2)
	require.NoError(t, got.Validate())
}
