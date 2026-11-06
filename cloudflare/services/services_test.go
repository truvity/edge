package services_test

import (
	"fmt"
	"os"
	"sort"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/catalog"
	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/internal/stacktest"
	"github.com/truvity/edge/v4/cloudflare/services"
)

var projects = []catalog.Project{{Name: "dms"}, {Name: "eudi"}, {Name: "url-shortener"}, {Name: "landing"}}

func fixture(t *testing.T) *config.Config {
	t.Helper()

	cat, err := catalog.Load(os.DirFS("../config/testdata"))
	require.NoError(t, err)

	cfg, err := config.Load(os.DirFS("../config/testdata"), cat, projects)
	require.NoError(t, err)

	return cfg
}

type sunk struct{ names []string }

func (s *sunk) sink(_ *pulumi.Context, t services.Tunnel, _ pulumi.StringOutput) error {
	s.names = append(s.names, t.Name())

	return nil
}

func args(t *testing.T, company string, s *sunk) services.Args {
	t.Helper()

	return services.Args{
		Config: fixture(t), Company: company, Sink: s.sink,
		Tokens: map[string]string{"main": "token-main", "partner": "token-partner"},
	}
}

func run(t *testing.T, a services.Args) (*stacktest.Recorder, error) {
	t.Helper()

	return stacktest.Run(func(ctx *pulumi.Context) error { return services.Deploy(ctx, a) })
}

const (
	tunnelType = "truvity:cloudflare:Tunnel::"
	record     = "cloudflare:index/dnsRecord:DnsRecord::"
	ingress    = "cloudflare:index/zeroTrustTunnelCloudflaredConfig:ZeroTrustTunnelCloudflaredConfig::"
)

func TestOneTunnelPerClusterAndAccountOfTheCompany(t *testing.T) {
	s := &sunk{}
	r, err := run(t, args(t, "example", s))
	require.NoError(t, err)

	got := r.ByKey()
	for _, name := range []string{"kernel", "devel", "stage", "prod"} {
		assert.Contains(t, got, tunnelType+name)
	}

	assert.NotContains(t, got, tunnelType+"devel-partner", "a tunnel of another company's account")
	assert.NotContains(t, got, "pulumi:providers:cloudflare::provider-partner")

	sort.Strings(s.names)
	assert.Equal(t, []string{"devel", "kernel", "prod", "stage"}, s.names, "every token reaches the sink")

	// The services stack manages no zone setting, ruleset or zone record.
	for _, res := range r.Resources() {
		assert.NotContains(t, res.Type, "ruleset")
		assert.NotContains(t, res.Type, "zoneSetting")
		assert.NotContains(t, res.Type, "totalTls")
	}

	r, err = run(t, args(t, "partner-co", &sunk{}))
	require.NoError(t, err)
	assert.Contains(t, r.ByKey(), tunnelType+"devel-partner")
	assert.Contains(t, r.ByKey(), "pulumi:providers:cloudflare::provider-partner")
}

func TestDNSElsewhereKeepsTheIngressRuleAndDropsTheCNAME(t *testing.T) {
	r, err := run(t, args(t, "example", &sunk{}))
	require.NoError(t, err)

	got := r.ByKey()
	assert.Contains(t, fmt.Sprint(got[ingress+"ingress-devel"].Inputs), "old.devel.example.com")
	assert.NotContains(t, got, record+"cname-devel-old-devel-example-com")
	assert.Contains(t, got, record+"cname-devel-dms-devel-example-com")
}

func TestOriginCNAMERewritesTheRecordContent(t *testing.T) {
	r, err := run(t, args(t, "example", &sunk{}))
	require.NoError(t, err)

	got := r.ByKey()
	assert.Equal(t, "d-abcdef1234.execute-api.eu-central-1.amazonaws.com", got[record+"cname-kernel-access-example-com"].Inputs["content"])
	assert.Equal(t, "tunnel-kernel-id.cfargotunnel.com", got[record+"cname-kernel-grafana-example-com"].Inputs["content"])
}

func TestClustersAndOrigin(t *testing.T) {
	a := args(t, "example", &sunk{})
	a.Clusters = []services.Cluster{{Name: "devel", DNSDomain: "k8s.internal"}}

	r, err := run(t, a)
	require.NoError(t, err)

	got := r.ByKey()
	assert.Contains(t, fmt.Sprint(got[ingress+"ingress-devel"].Inputs), "https://gateway-internal.envoy-gateway-system.svc.k8s.internal:443")
	assert.Contains(t, fmt.Sprint(got[ingress+"ingress-prod"].Inputs), "svc.cluster.prod:443", "an unlisted cluster has the default domain")

	a.Origin = services.Origin{Service: "other", Namespace: "ns"}
	r, err = run(t, a)
	require.NoError(t, err)
	assert.Contains(t, fmt.Sprint(r.ByKey()[ingress+"ingress-devel"].Inputs), "https://other.ns.svc.k8s.internal:443")
}

func TestTunnelOptionsAndAccountOptions(t *testing.T) {
	a := args(t, "example", &sunk{})

	var tunnels, accounts []string

	a.TunnelOptions = func(t services.Tunnel) []pulumi.ResourceOption {
		tunnels = append(tunnels, t.Name())

		return nil
	}
	a.AccountOptions = func(account string) []pulumi.ResourceOption {
		accounts = append(accounts, account)

		return nil
	}

	_, err := run(t, a)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"kernel", "devel", "stage", "prod"}, tunnels)
	assert.Equal(t, []string{"main"}, accounts)
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*services.Args)
		want   string
	}{
		"no sink":   {func(a *services.Args) { a.Sink = nil }, "TokenSink is required"},
		"no config": {func(a *services.Args) { a.Config = nil }, "Config is required"},
		"unknown":   {func(a *services.Args) { a.Company = "nobody" }, `company "nobody" has no account`},
		"no token":  {func(a *services.Args) { a.Tokens = nil }, "no resolved API token"},
	} {
		a := args(t, "example", &sunk{})
		tc.mutate(&a)

		_, err := run(t, a)
		require.ErrorContains(t, err, tc.want, name)
	}
}

// The services stack has no owner filter: the records of a company's zones
// stay in its services stack whoever owns the zone.
func TestZoneOwnersDoNotMoveTheCNAMEs(t *testing.T) {
	before, err := run(t, args(t, "example", &sunk{}))
	require.NoError(t, err)

	a := args(t, "example", &sunk{})
	for name, owner := range map[string]string{"example.com": config.OwnerPlatform, "example.org": "example"} {
		z := a.Config.Zones[name]
		z.Owner = owner
		a.Config.Zones[name] = z
	}

	after, err := run(t, a)
	require.NoError(t, err)

	assert.Equal(t, before.ByKey(), after.ByKey())
}
