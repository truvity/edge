package zonestack_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/internal/stacktest"
	"github.com/truvity/edge/v4/cloudflare/rules"
	"github.com/truvity/edge/v4/cloudflare/zonestack"
)

func run(t *testing.T, args zonestack.Args) (*stacktest.Recorder, error) {
	t.Helper()

	return stacktest.Run(func(ctx *pulumi.Context) error { return zonestack.Deploy(ctx, args) })
}

func args(t *testing.T, company string) zonestack.Args {
	t.Helper()

	cfg := fixture(t)

	return zonestack.Args{Config: cfg, Company: company, Tokens: tokens(cfg)}
}

func TestADeployHoldsOneCompanyOnly(t *testing.T) {
	r, err := run(t, args(t, "partner-co"))
	require.NoError(t, err)

	got := r.ByKey()
	assert.Contains(t, got, "pulumi:providers:cloudflare::provider-partner")
	assert.Contains(t, got, "truvity:cloudflare:Zone::example-net")
	assert.NotContains(t, got, "pulumi:providers:cloudflare::provider-main")
	assert.NotContains(t, got, "truvity:cloudflare:Zone::example-com", "a zone of another company's account")

	for key := range got {
		assert.NotContains(t, key, "Tunnel", "the zone stack has no tunnel")
	}

	r, err = run(t, args(t, "example"))
	require.NoError(t, err)
	assert.Contains(t, r.ByKey(), "truvity:cloudflare:Zone::example-org")
	assert.Contains(t, r.ByKey(), "pulumi:providers:cloudflare::provider-aop-provider-example-com")
	assert.NotContains(t, r.ByKey(), "truvity:cloudflare:Zone::example-net")
}

func TestHooks(t *testing.T) {
	a := args(t, "example")

	var accounts, zones, contributed []string

	a.AccountOptions = func(account string) []pulumi.ResourceOption {
		accounts = append(accounts, account)

		return nil
	}
	a.ZoneOptions = func(zone string) []pulumi.ResourceOption {
		zones = append(zones, zone)

		return nil
	}
	a.Contributions = func(zone string) []rules.Contribution {
		contributed = append(contributed, zone)

		if zone != "example.org" {
			return nil
		}

		c, err := rules.SSLMode("ssi.example.org", "full", "ssl-ssi")
		require.NoError(t, err)

		return []rules.Contribution{c}
	}

	r, err := run(t, a)
	require.NoError(t, err)

	assert.Equal(t, []string{"main"}, accounts)
	assert.Equal(t, []string{"example.com", "example.org"}, zones)
	assert.Equal(t, zones, contributed)

	got := r.ByKey()["cloudflare:index/ruleset:Ruleset::ssl-mode-example-org"]
	assert.Equal(t, "truvity:cloudflare:Zone::example-org", got.Parent)
	assert.Contains(t, got.Inputs["rules"].([]any)[0], "ref")
}

func TestRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*zonestack.Args)
		want   string
	}{
		"no config": {func(a *zonestack.Args) { a.Config = nil }, "Config is required"},
		"unknown":   {func(a *zonestack.Args) { a.Company = "nobody" }, `company "nobody" has no account`},
		"a token":   {func(a *zonestack.Args) { a.Tokens = map[string]string{} }, "no resolved API token"},
		"a pending zone": {func(a *zonestack.Args) {
			z := a.Config.Zones["example.com"]
			z.ZoneIDPending = true
			a.Config.Zones["example.com"] = z
		}, "zone_id_pending"},
	} {
		a := args(t, "example")
		tc.mutate(&a)

		_, err := run(t, a)
		require.ErrorContains(t, err, tc.want, name)
	}
}

func TestZoneArgsRefusesABadBlock(t *testing.T) {
	z := config.Zone{Name: "example.com", ZoneID: "z", PublicCache: &config.PublicCacheCfg{Host: "not a host"}}

	_, err := zonestack.ZoneArgs("example.com", z)
	require.ErrorContains(t, err, "public_cache")

	z = config.Zone{ZoneID: "z", SSLModeRules: map[string]string{"a.example.com": "sideways"}}
	_, err = zonestack.ZoneArgs("example.com", z)
	require.ErrorContains(t, err, "ssl_mode_rules[a.example.com]")
}

func TestZoneArgsCarriesRedirectsAndTheirImport(t *testing.T) {
	z := config.Zone{
		Name: "example.com", ZoneID: "z", Redirects: []config.RedirectCfg{{
			Ref: "www_to_apex", From: "www.example.com", To: "https://example.com",
		}},
		RedirectRulesImportID: "feed0001feed0002feed0003feed0004",
	}

	args, err := zonestack.ZoneArgs("example.com", z)
	require.NoError(t, err)
	require.NotNil(t, args.Redirects)
	assert.Equal(t, "example.com", args.Redirects.Zone)
	assert.Equal(t, "www_to_apex", args.Redirects.Entries[0].Ref)
	assert.Equal(t, "feed0001feed0002feed0003feed0004",
		args.ImportRulesets[rules.PhaseDynamicRedirect])
}
