package zonestack_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/zonestack"
)

// owned returns the fixture registry with example.com owned by the platform
// and example.org by the company "example", both in the company's account.
func owned(t *testing.T) zonestack.Args {
	t.Helper()

	a := args(t, "example")
	for name, owner := range map[string]string{"example.com": config.OwnerPlatform, "example.org": "example"} {
		z := a.Config.Zones[name]
		z.Owner = owner
		a.Config.Zones[name] = z
	}

	return a
}

// zoneKeys is the keys of the registered zone components.
func zoneKeys(t *testing.T, a zonestack.Args) []string {
	t.Helper()

	r, err := run(t, a)
	require.NoError(t, err)

	var out []string

	for _, res := range r.Resources() {
		if res.Type == "truvity:cloudflare:Zone" {
			out = append(out, res.Name)
		}
	}

	return out
}

func TestOwnersFilterWhichZonesAreDeployed(t *testing.T) {
	a := owned(t)
	assert.Equal(t, []string{"example-com", "example-org"}, zoneKeys(t, a), "no Owners: every zone, as before")

	a.Owners = []string{"example"}
	assert.Equal(t, []string{"example-org"}, zoneKeys(t, a), "a company stack registers no platform zone")

	a.Owners = []string{config.OwnerPlatform}
	assert.Equal(t, []string{"example-com"}, zoneKeys(t, a), "a platform stack registers no company zone")

	a.Owners = []string{config.OwnerPlatform, "example"}
	assert.Equal(t, []string{"example-com", "example-org"}, zoneKeys(t, a))
}

// A zone keeps its resource names whichever stack carries it: the handover
// between stacks is a state move, not a replacement.
func TestAZonesResourcesAreTheSameInEveryFilteredStack(t *testing.T) {
	forZone := func(a zonestack.Args, slug string) map[string]any {
		r, err := run(t, a)
		require.NoError(t, err)

		out := map[string]any{}

		for _, res := range r.Resources() {
			if res.Name == slug || res.Parent == "truvity:cloudflare:Zone::"+slug {
				out[res.Key()+" parent="+res.Parent] = res.Inputs
			}
		}

		return out
	}

	unfiltered := forZone(owned(t), "example-org")
	require.NotEmpty(t, unfiltered)

	company := owned(t)
	company.Owners = []string{"example"}
	assert.Equal(t, unfiltered, forZone(company, "example-org"))

	both := owned(t)
	both.Owners = []string{config.OwnerPlatform, "example"}
	assert.Equal(t, unfiltered, forZone(both, "example-org"))
}

func TestOwnersRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*zonestack.Args)
		want   string
	}{
		"an unowned zone": {func(a *zonestack.Args) {
			z := a.Config.Zones["example.org"]
			z.Owner = ""
			a.Config.Zones["example.org"] = z
			a.Owners = []string{"example"}
		}, "zone example.org has no owner"},
		"an empty owner":   {func(a *zonestack.Args) { a.Owners = []string{""} }, "Owners"},
		"a malformed name": {func(a *zonestack.Args) { a.Owners = []string{"Not A Company"} }, "Owners"},
		"a repeat":         {func(a *zonestack.Args) { a.Owners = []string{"example", "example"} }, "more than once"},
	} {
		a := owned(t)
		tc.mutate(&a)

		_, err := run(t, a)
		require.ErrorContains(t, err, tc.want, name)
	}
}
