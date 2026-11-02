package presets

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldFixture is fixture() with b.example managed for its security.txt only.
func heldFixture() Registry {
	reg := fixture()
	for i := range reg.Zones {
		if reg.Zones[i].Name == "b.example" {
			reg.Zones[i].SecurityTxtOnly = true
		}
	}

	return reg
}

func TestSecurityTxtOnlyZoneGetsZoneSettingsWriteAndNothingElse(t *testing.T) {
	for name, zones := range map[string][]string{"every zone of the account": nil, "named zones": {"a.example", "b.example"}} {
		t.Run(name, func(t *testing.T) {
			table, err := Build(heldFixture(), []Row{{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", Zones: zones}})
			require.NoError(t, err)

			got := flat(t, table[0])
			assert.Contains(t, got, "Zone Settings Write@zone."+zoneB)
			assert.Contains(t, got, "DNS Write,SSL and Certificates Write,Zone Settings Write,"+
				"Cache Settings Write,Config Settings Write@zone."+zoneA)

			// One policy on the held zone, and it is the last.
			var held int

			docs, err := table[0].Documents()
			require.NoError(t, err)

			for i, d := range docs {
				for k := range d.Resources {
					if k == "com.cloudflare.api.account.zone."+zoneB {
						held++

						assert.Equal(t, []string{"Zone Settings Write"}, d.Groups)
						assert.Equal(t, len(docs)-1, i, "after every other grant")
					}
				}
			}

			assert.Equal(t, 1, held)
		})
	}
}

func TestSecurityTxtOnlyZoneIsLeftOutOfDNSAndStatus(t *testing.T) {
	for _, kind := range []string{KindDNS, KindStatus} {
		table, err := Build(heldFixture(), []Row{{Name: "umbrella-" + kind, Kind: kind, Account: "umbrella"}})
		require.NoError(t, err)

		for _, s := range flat(t, table[0]) {
			assert.NotContains(t, s, zoneB, kind)
		}

		assert.Contains(t, flat(t, table[0])[len(flat(t, table[0]))-1], "DNS Write", kind)
	}

	_, err := Build(heldFixture(), []Row{{Name: "umbrella-dns", Kind: KindDNS, Account: "umbrella", Zones: []string{"b.example"}}})
	require.ErrorContains(t, err, "managed for its security.txt only")
}

func TestSecurityTxtOnlyZoneRefusesTheZoneAddOns(t *testing.T) {
	for name, mutate := range map[string]func(*Row){
		"waf":             func(r *Row) { r.WAFZones = []string{"b.example"} },
		"bot management":  func(r *Row) { r.BotManagementZones = []string{"b.example"} },
		"cloud connector": func(r *Row) { r.CloudConnectorZones = []string{"b.example"} },
		"dns only":        func(r *Row) { r.Zones = []string{"a.example"}; r.DNSOnlyZones = []string{"b.example"} },
	} {
		r := Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella"}
		mutate(&r)

		_, err := Build(heldFixture(), []Row{r})
		require.ErrorContains(t, err, "managed for its security.txt only", name)
	}
}

func TestAccountOfOnlySecurityTxtOnlyZonesCannotRunDNS(t *testing.T) {
	reg := Registry{Accounts: fixture().Accounts, Zones: []Zone{{Name: "b.example", ID: zoneB, Account: "umbrella", SecurityTxtOnly: true}}}

	_, err := Build(reg, []Row{{Name: "umbrella-dns", Kind: KindDNS, Account: "umbrella"}})
	require.ErrorContains(t, err, "every zone is managed for its security.txt only")

	table, err := Build(reg, []Row{{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella"}})
	require.NoError(t, err)
	assert.Equal(t, []string{"Zone Settings Write@zone." + zoneB}, flat(t, table[0]))
}

func TestInsightsKindsStillCoverASecurityTxtOnlyZone(t *testing.T) {
	table, err := Build(heldFixture(), []Row{{Name: "umbrella-insights", Kind: KindInsights, Account: "umbrella"}})
	require.NoError(t, err)

	var found bool

	for _, s := range flat(t, table[0]) {
		found = found || (len(s) > len(zoneB) && s[len(s)-len(zoneB):] == zoneB)
	}

	assert.True(t, found)
}
