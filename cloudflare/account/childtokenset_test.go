package account

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChildTokenSetMintsOnePerKeyWithExactNames is the naming-preservation
// contract itself: a set with keys "edge", "r2-admin" and
// "r2-parent-example-bucket" must register the SAME three AccountToken
// resources, under the SAME three Pulumi names, that a caller writing
// three separate NewChildToken calls would have registered — nothing
// about grouping them into one map call may rename, retype or reparent
// any of them.
func TestChildTokenSetMintsOnePerKeyWithExactNames(t *testing.T) {
	m := newChildTokenMocks()

	var tokens map[string]*ChildToken
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		var err error
		tokens, err = NewChildTokenSet(ctx, "root", acct, map[string]ChildTokenConfig{
			"edge": {
				Name:     "cloudflare-edge",
				Policies: EdgePolicies("0123456789abcdef0123456789abcdef"),
			},
			"r2-admin": {
				Name:     "cloudflare-r2-admin",
				Policies: R2AdminPolicies(),
			},
			"r2-parent-example-bucket": {
				Name:     "cloudflare-r2-parent-example-bucket",
				Policies: R2BucketParentPolicies("", "example-bucket"),
			},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	for _, name := range []string{"edge", "r2-admin", "r2-parent-example-bucket"} {
		_, ok := m.res[accountTokenType+"/"+name]
		assert.True(t, ok, "child %q must be registered under exactly this Pulumi name", name)
	}

	require.Len(t, tokens, 3)
	assert.Equal(t, "cloudflare-edge", m.res[accountTokenType+"/edge"]["name"].StringValue())
	assert.Equal(t, "cloudflare-r2-admin", m.res[accountTokenType+"/r2-admin"]["name"].StringValue())
	assert.Equal(t, "cloudflare-r2-parent-example-bucket", m.res[accountTokenType+"/r2-parent-example-bucket"]["name"].StringValue())

	require.NotNil(t, tokens["edge"])
	require.NotNil(t, tokens["r2-admin"])
	require.NotNil(t, tokens["r2-parent-example-bucket"])
}

// TestChildTokenSetMintsInSortedKeyOrder proves NewChildTokenSet visits
// cfgs' keys in sorted order, regardless of the map's own (randomized) Go
// iteration order: it puts an invalid config under the alphabetically
// FIRST key ("a-broken") alongside a valid one under a key that sorts
// after it ("z-fine"). NewChildTokenSet stops at the first error, so a
// sorted walk fails on "a-broken" before "z-fine" is ever minted — an
// unsorted (map-order) walk would mint "z-fine" first whenever Go's
// randomized iteration happened to visit it before "a-broken", making
// this assertion flaky under an unsorted implementation and reliable
// under a sorted one.
func TestChildTokenSetMintsInSortedKeyOrder(t *testing.T) {
	for i := 0; i < 20; i++ {
		m := newChildTokenMocks()

		err := pulumi.RunErr(func(ctx *pulumi.Context) error {
			acct := withProvider(t, ctx, "example-account-id")

			_, err := NewChildTokenSet(ctx, "root", acct, map[string]ChildTokenConfig{
				"z-fine": {Name: "cloudflare-z-fine", Policies: R2AdminPolicies()},
				"a-broken": {
					Name: "cloudflare-a-broken",
					Policies: []ChildTokenPolicy{
						{PermissionGroups: []string{"Account API Tokens Write"}, Scope: WholeAccountScope{}},
					},
				},
			})

			return err
		}, pulumi.WithMocks("proj", "stack", m))
		require.Error(t, err)
		assert.Contains(t, err.Error(), `child "a-broken"`)

		_, zFineRegistered := m.res[accountTokenType+"/z-fine"]
		assert.False(t, zFineRegistered, "the sorted walk must fail on a-broken before z-fine is ever minted")
	}
}

func TestChildTokenSetEmptyMintsNothing(t *testing.T) {
	m := newChildTokenMocks()

	var tokens map[string]*ChildToken
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		var err error
		tokens, err = NewChildTokenSet(ctx, "root", acct, nil)

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)
	assert.NotNil(t, tokens)
	assert.Empty(t, tokens)

	for key := range m.res {
		assert.NotContains(t, key, accountTokenType, "an empty set must register no AccountToken — only acct's own provider may be present")
	}
}

// TestChildTokenSetSecretsAreMarkedSecret guards the same contract
// NewChildToken's own TestValueIsMarkedSecret does, once per entry of a
// set: a caller iterating the returned map must never see an unmarked
// secret regardless of which key it reads.
func TestChildTokenSetSecretsAreMarkedSecret(t *testing.T) {
	m := newChildTokenMocks()

	var tokens map[string]*ChildToken
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		var err error
		tokens, err = NewChildTokenSet(ctx, "root", acct, map[string]ChildTokenConfig{
			"edge": {Name: "cloudflare-edge", Policies: EdgePolicies("0123456789abcdef0123456789abcdef")},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	require.NotNil(t, tokens["edge"])
	assert.True(t, pulumi.IsSecret(tokens["edge"].Value), "Value must be secret")
	assert.False(t, pulumi.IsSecret(tokens["edge"].ID), "ID is not a secret")
}

// TestChildTokenSetRefusesForbiddenGroup proves the set inherits
// ChildTokenConfig.Validate's own refusal of Account API Tokens
// Read/Write with no extra code in NewChildTokenSet: the set does no
// validation of its own, so this is really a guard against a future
// change accidentally bypassing NewChildToken's checks for the
// set-of-many path.
func TestChildTokenSetRefusesForbiddenGroup(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		_, err := NewChildTokenSet(ctx, "root", acct, map[string]ChildTokenConfig{
			"rogue": {
				Name: "cloudflare-rogue",
				Policies: []ChildTokenPolicy{
					{PermissionGroups: []string{"Account API Tokens Write"}, Scope: WholeAccountScope{}},
				},
			},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must never be granted to a child token")
	assert.Contains(t, err.Error(), `child token set "root"`)
	assert.Contains(t, err.Error(), `child "rogue"`)
}

func TestEdgePoliciesShape(t *testing.T) {
	policies := EdgePolicies("0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210")
	require.Len(t, policies, 3, "one account-wide tunnel policy, plus one per zone")

	assert.Equal(t, []string{"Cloudflare Tunnel Write"}, policies[0].PermissionGroups)
	assert.Equal(t, WholeAccountScope{}, policies[0].Scope)

	for _, p := range policies[1:] {
		assert.Equal(t,
			[]string{"DNS Write", "SSL and Certificates Write", "Zone Settings Write", "Cache Settings Write", "Config Settings Write"},
			p.PermissionGroups)
	}
	assert.Equal(t, ZoneScope{ZoneID: "0123456789abcdef0123456789abcdef"}, policies[1].Scope)
	assert.Equal(t, ZoneScope{ZoneID: "fedcba9876543210fedcba9876543210"}, policies[2].Scope)
}

func TestRedirectPoliciesAreOptIn(t *testing.T) {
	const zoneA, zoneB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"

	for _, p := range append(ZonePolicies(zoneA), EdgePolicies(zoneA)...) {
		assert.NotContains(t, p.PermissionGroups, PermDynamicRedirectsWrite,
			"the baseline never grants the redirect group")
	}

	assert.Nil(t, RedirectPolicies(), "no zones: nothing to append")

	got := RedirectPolicies(zoneA, zoneB)
	require.Len(t, got, 2)
	assert.Equal(t, []string{PermDynamicRedirectsWrite}, got[0].PermissionGroups)
	assert.Equal(t, ZoneScope{ZoneID: zoneA}, got[0].Scope)
	assert.Equal(t, ZoneScope{ZoneID: zoneB}, got[1].Scope)

	with := EdgePoliciesWithRedirects([]string{zoneA}, zoneA, zoneB)
	base := EdgePolicies(zoneA, zoneB)
	require.Len(t, with, len(base)+1)
	assert.Equal(t, base, with[:len(base)])
	assert.Equal(t, []string{PermDynamicRedirectsWrite}, with[len(base)].PermissionGroups)
	assert.Equal(t, ZoneScope{ZoneID: zoneA}, with[len(base)].Scope)
}

func TestEdgePoliciesNoZones(t *testing.T) {
	policies := EdgePolicies()
	require.Len(t, policies, 1, "the account-wide tunnel policy is unconditional")
	assert.Equal(t, WholeAccountScope{}, policies[0].Scope)
}

func TestEdgePoliciesWithWAF(t *testing.T) {
	const zoneA, zoneB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"

	base := EdgePolicies(zoneA, zoneB)

	assert.Equal(t, base, EdgePoliciesWithWAF(nil, zoneA, zoneB), "no WAF zones: output is unchanged")
	assert.Equal(t, base, EdgePoliciesWithWAF([]string{}, zoneA, zoneB))

	got := EdgePoliciesWithWAF([]string{zoneA}, zoneA, zoneB)
	require.Len(t, got, len(base)+1, "exactly one extra policy")
	assert.Equal(t, base, got[:len(base)], "the edge grant itself is untouched")
	assert.Equal(t, []string{"Zone WAF Write"}, got[len(base)].PermissionGroups)
	assert.Equal(t, ZoneScope{ZoneID: zoneA}, got[len(base)].Scope)

	two := EdgePoliciesWithWAF([]string{zoneA, zoneB}, zoneA)
	require.Len(t, two, 4)
	assert.Equal(t, ZoneScope{ZoneID: zoneB}, two[3].Scope)
}

func TestBotManagementPolicies(t *testing.T) {
	const zoneA, zoneB = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"

	assert.Nil(t, BotManagementPolicies(), "no zones: nothing to append")

	got := BotManagementPolicies(zoneA, zoneB)
	require.Len(t, got, 2)
	assert.Equal(t, []string{"Bot Management Write"}, got[0].PermissionGroups)
	assert.Equal(t, ZoneScope{ZoneID: zoneA}, got[0].Scope)
	assert.Equal(t, ZoneScope{ZoneID: zoneB}, got[1].Scope)

	for _, p := range EdgePoliciesWithWAF([]string{zoneA}, zoneA) {
		assert.NotContains(t, p.PermissionGroups, PermBotManagementWrite, "the edge grant never includes it")
	}
}

func TestR2AdminPoliciesShape(t *testing.T) {
	policies := R2AdminPolicies()
	require.Len(t, policies, 1)
	assert.Equal(t, []string{"Workers R2 Storage Write"}, policies[0].PermissionGroups)
	assert.Equal(t, WholeAccountScope{}, policies[0].Scope)
}

func TestR2BucketParentPoliciesShape(t *testing.T) {
	policies := R2BucketParentPolicies("eu", "example-bucket")
	require.Len(t, policies, 1)
	assert.Equal(t, []string{"Workers R2 Storage Bucket Item Write"}, policies[0].PermissionGroups)
	assert.Equal(t, R2BucketScope{Jurisdiction: "eu", Bucket: "example-bucket"}, policies[0].Scope)
}

// TestPresetsMintThroughNewChildTokenSet is an end-to-end sanity check
// that every preset's Scope and PermissionGroups round-trip through an
// actual mint: a preset that built a scope this package's own Validate or
// resourceKey rejects would fail here, not just in a unit test of the
// preset function alone.
func TestPresetsMintThroughNewChildTokenSet(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		_, err := NewChildTokenSet(ctx, "root", acct, map[string]ChildTokenConfig{
			"edge":                     {Name: "cloudflare-edge", Policies: EdgePolicies("0123456789abcdef0123456789abcdef")},
			"r2-admin":                 {Name: "cloudflare-r2-admin", Policies: R2AdminPolicies()},
			"r2-parent-example-bucket": {Name: "cloudflare-r2-parent-example-bucket", Policies: R2BucketParentPolicies("", "example-bucket")},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)
}

func TestSecurityTxtPoliciesGrantOnlyZoneSettingsWrite(t *testing.T) {
	assert.Nil(t, SecurityTxtPolicies())

	got := SecurityTxtPolicies("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	require.Len(t, got, 2)

	for i, id := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"} {
		assert.Equal(t, []string{"Zone Settings Write"}, got[i].PermissionGroups)
		assert.Equal(t, ZoneScope{ZoneID: id}, got[i].Scope)
	}
}
