package config_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/config"
)

// withOwners gives the fixture's zones owners: example.com the platform's,
// the partner account's two zones company partner's.
func withOwners(t *testing.T) string {
	t.Helper()

	y := replaceOnce(t, registry, "example.com: {account: platform,", "example.com: {account: platform, owner: platform,")
	y = replaceOnce(t, y, "partner.test: {account: partner,", "partner.test: {account: partner, owner: partner,")

	return replaceOnce(t, y, "other.test: {account: partner,", "other.test: {account: partner, owner: partner,")
}

func TestZoneOwnerIsOptional(t *testing.T) {
	c, err := config.Parse([]byte(registry))
	require.NoError(t, err)

	for name, z := range c.Zones {
		assert.Empty(t, z.Owner, name)
	}

	assert.Empty(t, c.ZonesOwnedBy(config.OwnerPlatform))
	assert.Empty(t, c.ZonesOwnedBy(""), "a zone without an owner is nobody's")
}

func TestZoneOwnerParsesAndGroups(t *testing.T) {
	c, err := load(t, withOwners(t), goodGateways())
	require.NoError(t, err)

	assert.Equal(t, config.OwnerPlatform, c.Zones["example.com"].Owner)
	assert.Equal(t, []string{"example.com"}, c.ZonesOwnedBy(config.OwnerPlatform))
	assert.Equal(t, []string{"other.test", "partner.test"}, c.ZonesOwnedBy("partner"))
	assert.Empty(t, c.ZonesOwnedBy("nobody"))

	// The per-account view keeps the owner.
	partner, err := c.ForAccount("partner")
	require.NoError(t, err)
	assert.Equal(t, "partner", partner.Zones["partner.test"].Owner)
}

func TestZoneOwnerDoesNotChangeTheDerivation(t *testing.T) {
	owned, err := load(t, withOwners(t), goodGateways())
	require.NoError(t, err)

	base, err := load(t, registry, goodGateways())
	require.NoError(t, err)

	assert.Equal(t, base.Tunnels, owned.Tunnels)

	for name, z := range owned.Zones {
		want := base.Zones[name]
		want.Owner = z.Owner
		assert.Equal(t, want, z, name)
	}
}

func TestPlatformZoneMayLiveInAnyAccount(t *testing.T) {
	y := replaceOnce(t, registry, "other.test: {account: partner,", "other.test: {account: partner, owner: platform,")
	_, err := config.Parse([]byte(y))
	require.NoError(t, err)
}

func TestCompanyZoneFollowsTheAccountsCompanyLabel(t *testing.T) {
	// accounts.partner.company relabels the account: its zones are acme's.
	y := replaceOnce(t, registry, "    account_id: a2\n", "    account_id: a2\n    company: acme\n")
	y = replaceOnce(t, y, "partner.test: {account: partner,", "partner.test: {account: partner, owner: acme,")

	c, err := config.Parse([]byte(y))
	require.NoError(t, err)
	assert.Equal(t, []string{"partner.test"}, c.ZonesOwnedBy("acme"))
}

func TestZoneOwnerRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		zone, owner, want string
	}{
		"uppercase":         {"partner.test", "Partner", `"Partner" is neither "platform" nor a company label`},
		"leading digit":     {"partner.test", "1partner", `is neither "platform" nor a company label`},
		"space":             {"partner.test", "'part ner'", `is neither "platform" nor a company label`},
		"another company's": {"partner.test", "acme", `company "acme" owns the zone, but its account "partner" is company "partner"'s`},
		"platform account":  {"example.com", "partner", `company "partner" owns the zone, but its account "platform" is company "platform"'s`},
	} {
		t.Run(name, func(t *testing.T) {
			y := replaceOnce(t, registry, tc.zone+": {account: ", tc.zone+": {owner: "+tc.owner+", account: ")
			_, err := config.Parse([]byte(y))
			require.Error(t, err)
			assert.Contains(t, err.Error(), "zones["+tc.zone+"].owner")
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestSecurityTxtOnlyZoneMayCarryAnOwner(t *testing.T) {
	y := replaceOnce(t, withHeld(t), "held.example.net: {account: partner,", "held.example.net: {account: partner, owner: platform,")
	c, err := load(t, y, goodGateways())
	require.NoError(t, err)

	z := c.Zones["held.example.net"]
	assert.True(t, z.SecurityTxtOnly())
	assert.Equal(t, []string{"held.example.net"}, c.ZonesOwnedBy(config.OwnerPlatform))

	// The mode's own rule still holds for an owned zone.
	y = replaceOnce(t, y, "manage: security_txt,", "manage: security_txt, ssl_mode: full,")
	_, err = load(t, y, goodGateways())
	require.ErrorContains(t, err, "ssl_mode: refused with manage: security_txt")
}
