package presets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ownedFixture is the fixture with owners: a.example and b.example belong to
// company acme, c.example to the platform, all in one account.
func ownedFixture() Registry {
	reg := fixture()

	for i := range reg.Zones {
		switch reg.Zones[i].Name {
		case "a.example", "b.example":
			reg.Zones[i].Owner = "acme"
		case "c.example":
			reg.Zones[i].Owner = "platform"
		}
	}

	return reg
}

func ownerRow() Row {
	return Row{
		Name: "umbrella-acme-zone", Kind: KindZone, Account: "umbrella", Owner: "acme",
		Zones: []string{"a.example", "b.example"}, WAFZones: []string{"a.example"},
	}
}

func TestAnOwnerRowCoversOnlyItsOwnersZones(t *testing.T) {
	table, err := Build(ownedFixture(), []Row{ownerRow()})
	require.NoError(t, err)
	assert.Equal(t, "acme", table[0].Owner)

	// The owner changes who may ask for the row, not what it grants.
	plain := ownerRow()
	plain.Owner = ""

	want, err := Build(fixture(), []Row{plain})
	require.NoError(t, err)

	assert.ElementsMatch(t, flat(t, want[0]), flat(t, table[0]))
}

func TestAnOwnerRowRefuses(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Row)
		want   string
	}{
		"a platform zone":   {func(r *Row) { r.Zones = []string{"a.example", "c.example"} }, `zone "c.example" is owned by "platform", not by "acme"`},
		"no zones listed":   {func(r *Row) { r.Zones = nil; r.WAFZones = nil }, "Owner needs Zones"},
		"a dns-only zone":   {func(r *Row) { r.DNSOnlyZones = []string{"c.example"} }, "DNSOnlyZones"},
		"a cloud connector": {func(r *Row) { r.CloudConnectorZones = []string{"a.example"} }, "CloudConnectorZones"},
		"a services kind":   {func(r *Row) { r.Kind = KindServices; r.WAFZones = nil }, "Owner is for the zone kind"},
		"a status kind":     {func(r *Row) { r.Kind = KindStatus; r.WAFZones = nil }, "Owner is for the zone kind"},
		"a dns kind":        {func(r *Row) { r.Kind = KindDNS; r.WAFZones = nil }, "Owner is for the zone kind"},
		"an insights kind":  {func(r *Row) { r.Kind = KindInsights; r.WAFZones = nil }, "Owner is for the zone kind"},
		"a malformed owner": {func(r *Row) { r.Owner = "Not A Company" }, "Owner"},
		"an unowned zone":   {func(r *Row) { r.Zones = []string{"a.example", "d.example"} }, `zone "d.example"`},
		"a different owner": {func(r *Row) { r.Owner = "globex" }, `zone "a.example" is owned by "acme", not by "globex"`},
	} {
		t.Run(name, func(t *testing.T) {
			reg := ownedFixture()
			reg.Zones = append(reg.Zones, Zone{Name: "d.example", ID: zoneC, Account: "umbrella"})

			row := ownerRow()
			tc.mutate(&row)

			err := Validate(reg, []Row{row})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestARowWithoutAnOwnerIsUnchanged(t *testing.T) {
	// Owners on the registry's zones alone change nothing for an ownerless row.
	row := Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella"}

	withOwners, err := Build(ownedFixture(), []Row{row})
	require.NoError(t, err)

	without, err := Build(fixture(), []Row{row})
	require.NoError(t, err)

	assert.Equal(t, without, withOwners)
}
