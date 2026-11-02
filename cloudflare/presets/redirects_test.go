package presets

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const redirectGroup = "Dynamic URL Redirects Write"

// TestRedirectZonesAreOptIn is the default-rights regression: a zone kind
// that does not list RedirectZones keeps the existing baseline, with no
// redirect grant anywhere.
func TestRedirectZonesAreOptIn(t *testing.T) {
	table, err := Build(fixture(), []Row{{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella"}})
	require.NoError(t, err)

	for _, got := range flat(t, table[0]) {
		assert.NotContains(t, got, redirectGroup)
	}
}

// TestRedirectZonesScopeTheGrantToOneRowZone adds the grant only to the
// listed zone, even when the row covers several zones.
func TestRedirectZonesScopeTheGrantToOneRowZone(t *testing.T) {
	table, err := Build(fixture(), []Row{{
		Name: "umbrella-zone", Kind: KindZone, Account: "umbrella",
		Zones: []string{"a.example", "b.example"}, RedirectZones: []string{"a.example"},
	}})
	require.NoError(t, err)

	docs, err := table[0].Documents()
	require.NoError(t, err)

	var got []string
	for _, d := range docs {
		if slices.Contains(d.Groups, redirectGroup) {
			for key := range d.Resources {
				got = append(got, key)
			}
		}
	}

	require.Len(t, got, 1, "exactly one redirect policy")
	assert.Contains(t, got[0], zoneA)
	assert.NotContains(t, got[0], zoneB)

	// The same policy list is what break-glass mints.
	assert.Equal(t, table[0].Policies, table[0].Request().Policies)
}

func TestRedirectZoneValidation(t *testing.T) {
	for name, tc := range map[string]struct {
		reg  Registry
		row  Row
		want string
	}{
		"outside the row": {
			fixture(),
			Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", Zones: []string{"a.example"}, RedirectZones: []string{"b.example"}},
			"not among the zones of the row",
		},
		"outside the account": {
			fixture(),
			Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", RedirectZones: []string{"x.example"}},
			"is not a zone of account umbrella",
		},
		"wrong kind": {
			fixture(),
			Row{Name: "umbrella-status", Kind: KindStatus, Account: "umbrella", RedirectZones: []string{"a.example"}},
			"for the zone kind",
		},
		"duplicate": {
			fixture(),
			Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", RedirectZones: []string{"a.example", "a.example"}},
			"listed twice",
		},
		"security.txt only": {
			splitFixture(),
			Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", Zones: []string{"t.example"}, RedirectZones: []string{"t.example"}},
			"security.txt only",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(tc.reg, []Row{tc.row})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestRedirectPresetGolden pins the exact documents of a redirect-opted zone:
// the baseline policy, then one redirect policy on the same zone only.
func TestRedirectPresetGolden(t *testing.T) {
	table, err := Build(fixture(), []Row{{
		Name: "umbrella-zone", Kind: KindZone, Account: "umbrella",
		Zones: []string{"a.example"}, RedirectZones: []string{"a.example"},
	}})
	require.NoError(t, err)

	docs, err := table[0].Documents()
	require.NoError(t, err)

	key := "com.cloudflare.api.account.zone." + zoneA
	assert.Equal(t, []PolicyDocument{
		{
			Groups: []string{
				"DNS Write", "SSL and Certificates Write", "Zone Settings Write",
				"Cache Settings Write", "Config Settings Write",
			},
			Resources: map[string]string{key: "*"},
		},
		{Groups: []string{redirectGroup}, Resources: map[string]string{key: "*"}},
	}, docs)
}

// TestRedirectGrantSurvivesRuleRemoval is the teardown regression: the grant
// is derived from the explicit RedirectZones allowlist, not from any rule
// content in this package. Removing the last redirect rule does not mutate the
// row, so the delete permission remains until the operator removes the zone
// from RedirectZones in a later apply.
func TestRedirectGrantSurvivesRuleRemoval(t *testing.T) {
	row := Row{
		Name: "umbrella-zone", Kind: KindZone, Account: "umbrella",
		Zones: []string{"a.example"}, RedirectZones: []string{"a.example"},
	}

	table, err := Build(fixture(), []Row{row})
	require.NoError(t, err)

	docs, err := table[0].Documents()
	require.NoError(t, err)

	count := 0
	for _, d := range docs {
		if slices.Contains(d.Groups, redirectGroup) {
			count++
		}
	}

	assert.Equal(t, 1, count, "the explicit allowlist keeps the delete grant after the rule is gone")
}
