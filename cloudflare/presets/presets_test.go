package presets

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	acct1 = "0123456789abcdef0123456789abcdef"
	zoneA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	zoneB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	zoneC = "cccccccccccccccccccccccccccccccc"
)

func fixture() Registry {
	return Registry{
		Accounts: []Account{{Name: "umbrella", ID: acct1}},
		Zones: []Zone{
			{Name: "b.example", ID: zoneB, Account: "umbrella"},
			{Name: "a.example", ID: zoneA, Account: "umbrella"},
			{Name: "c.example", ID: zoneC, Account: "umbrella"},
		},
	}
}

func flat(t *testing.T, p Preset) []string {
	t.Helper()

	docs, err := p.Documents()
	require.NoError(t, err)

	var out []string

	for _, d := range docs {
		for k := range d.Resources {
			out = append(out, strings.Join(d.Groups, ",")+"@"+strings.TrimPrefix(k, "com.cloudflare.api.account."))
		}
	}

	return out
}

// The kinds a second estate (umbrella) needs: a zone preset on one zone with a
// WAF, a Cloud Connector rule and DNS on a further zone, a DNS-only preset, an
// R2 admin and the workload buckets, with a rotation.
func TestKindsOfASecondEstate(t *testing.T) {
	rows := []Row{
		{
			Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", Zones: []string{"a.example"}, WAFZones: []string{"a.example"},
			CloudConnectorZones: []string{"a.example"}, DNSOnlyZones: []string{"c.example"}, Grants: []string{"example:deploy"},
		},
		{Name: "umbrella-cert-dns", Kind: KindDNS, Account: "umbrella", Zones: []string{"a.example"}},
		{Name: "umbrella-r2-admin", Kind: KindR2Admin, Account: "umbrella"},
		{Name: "umbrella-oidc-writer", Kind: KindBucketWrite, Account: "umbrella", Bucket: "oidc"},
		{
			Name: "umbrella-gocache", Kind: KindBucketRW, Account: "umbrella", Bucket: "gocache", Jurisdiction: "eu",
			Store: StoreExternal, Lifetime: time.Hour, Rotation: 20 * time.Minute, Description: "The CI plane's cache",
		},
	}

	table, err := Build(fixture(), rows)
	require.NoError(t, err)

	zonePreset, err := Named(table, "umbrella-zone")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"DNS Write,SSL and Certificates Write,Zone Settings Write,Cache Settings Write,Config Settings Write@zone." + zoneA,
		"Zone WAF Write@zone." + zoneA,
		"Cloud Connector Write@zone." + zoneA,
		"DNS Write@zone." + zoneC,
	}, flat(t, zonePreset))

	dns, _ := Named(table, "umbrella-cert-dns")
	assert.Equal(t, []string{"DNS Write@zone." + zoneA}, flat(t, dns))

	oidc, _ := Named(table, "umbrella-oidc-writer")
	assert.True(t, oidc.R2)
	assert.Equal(t, []string{"Workers R2 Storage Bucket Item Write@com.cloudflare.edge.r2.bucket." + acct1 + "_default_oidc"}, flat(t, oidc))

	gocache, _ := Named(table, "umbrella-gocache")
	assert.True(t, gocache.R2)
	assert.Equal(t, StoreExternal, gocache.Store)
	assert.Equal(t, 20*time.Minute, gocache.Rotation)
	assert.Equal(t, "The CI plane's cache", gocache.Description)
	require.Len(t, flat(t, gocache), 1)
	assert.Contains(t, flat(t, gocache)[0], "Workers R2 Storage Bucket Item Read,Workers R2 Storage Bucket Item Write@")
	assert.Contains(t, flat(t, gocache)[0], "eu")

	// Every zone of the account, by name, when a row names none.
	all, err := Build(fixture(), []Row{{Name: "umbrella-status", Kind: KindStatus, Account: "umbrella"}})
	require.NoError(t, err)
	assert.Equal(t, []string{
		"Cloudflare Tunnel Write@" + acct1,
		"DNS Write@zone." + zoneA, "DNS Write@zone." + zoneB, "DNS Write@zone." + zoneC,
	}, flat(t, all[0]))
	assert.Equal(t, "Pulumi: tunnel and DNS of the umbrella account's status service", all[0].Description)
	assert.Equal(t, DefaultLifetime, all[0].Lifetime)

	assert.Equal(t, "umbrella", AccountOf(fixture(), "umbrella-edge"))
	assert.Empty(t, AccountOf(fixture(), "other-edge"))
}

func TestValidateRefusesEveryProblem(t *testing.T) {
	base := func(mutate func(*Row)) []Row {
		r := Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella"}
		mutate(&r)

		return []Row{r}
	}

	for name, tc := range map[string]struct {
		rows []Row
		want string
	}{
		"bad name":         {base(func(r *Row) { r.Name = "Acme_Edge" }), "lowercase letters"},
		"unknown kind":     {base(func(r *Row) { r.Kind = "admin" }), `kind "admin"`},
		"unknown account":  {base(func(r *Row) { r.Account = "nobody" }), `no account "nobody"`},
		"unknown zone":     {base(func(r *Row) { r.Zones = []string{"x.example"} }), `zone "x.example" is not a zone of account umbrella`},
		"zone twice":       {base(func(r *Row) { r.Zones = []string{"a.example", "a.example"} }), "listed twice"},
		"zones on r2":      {base(func(r *Row) { r.Kind = KindR2Admin; r.Zones = []string{"a.example"} }), "Zones is for"},
		"waf outside":      {base(func(r *Row) { r.Zones = []string{"a.example"}; r.WAFZones = []string{"b.example"} }), "not among the zones"},
		"bot outside":      {base(func(r *Row) { r.Zones = []string{"a.example"}; r.BotManagementZones = []string{"b.example"} }), "not among the zones"},
		"bot on status":    {base(func(r *Row) { r.Kind = KindStatus; r.BotManagementZones = []string{"a.example"} }), "for the zone kind"},
		"waf on status":    {base(func(r *Row) { r.Kind = KindStatus; r.WAFZones = []string{"a.example"} }), "for the zone kind"},
		"dns-only dup":     {base(func(r *Row) { r.Zones = []string{"a.example"}; r.DNSOnlyZones = []string{"a.example"} }), "already among"},
		"no bucket":        {base(func(r *Row) { r.Kind = KindBucketRW }), "not an R2 bucket name"},
		"bucket on edge":   {base(func(r *Row) { r.Bucket = "x" }), "Bucket and Jurisdiction are for"},
		"bad grant":        {base(func(r *Row) { r.Grants = []string{"two words"} }), "not a policy group name"},
		"rotation on none": {base(func(r *Row) { r.Rotation = time.Minute }), "sluis refuses a rotation"},
		"external no rot":  {base(func(r *Row) { r.Store = StoreExternal }), "Rotation 0s must be positive"},
		"rotation > life":  {base(func(r *Row) { r.Store = StoreExternal; r.Rotation = 2 * time.Hour }), "under Lifetime"},
		"bad store":        {base(func(r *Row) { r.Store = "ssm" }), `store "ssm"`},
		"twice":            {append(base(func(*Row) {}), base(func(*Row) {})...), "the name is used twice"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Build(fixture(), tc.rows)
			require.ErrorContains(t, err, tc.want)
		})
	}

	// Registry problems.
	reg := fixture()
	reg.Accounts[0].ID = "short"
	reg.Zones = append(reg.Zones, Zone{Name: "a.example", Account: "umbrella"}, Zone{Name: "z", Account: "ghost"})

	err := Validate(reg, nil)
	require.ErrorContains(t, err, "not a 32-character")
	require.ErrorContains(t, err, `zone "a.example" is declared twice`)
	require.ErrorContains(t, err, `no account "ghost"`)
}

// Everything is reported at once, not the first problem alone.
func TestValidateReportsAll(t *testing.T) {
	err := Validate(fixture(), []Row{{Name: "A", Kind: "x", Account: "umbrella"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lowercase letters")
	assert.Contains(t, err.Error(), `kind "x"`)
}

func TestRequestCarriesThePolicies(t *testing.T) {
	table, err := Build(fixture(), []Row{{Name: "umbrella-gocache", Kind: KindBucketRW, Account: "umbrella", Bucket: "gocache"}})
	require.NoError(t, err)

	req := table[0].Request()
	assert.Equal(t, "umbrella-gocache", req.Preset)
	assert.True(t, req.R2)
	assert.Equal(t, acct1, req.AccountID)
	assert.Equal(t, table[0].Policies, req.Policies)
}

// The insights kind is read-only: the account group, then the three zone
// groups on each zone (every zone of the account by name, or the row's own).
func TestInsightsIsReadOnly(t *testing.T) {
	table, err := Build(fixture(), []Row{
		{Name: "umbrella-insights", Kind: KindInsights, Account: "umbrella"},
		{Name: "umbrella-insights-one", Kind: KindInsights, Account: "umbrella", Zones: []string{"b.example"}},
	})
	require.NoError(t, err)

	const zoneGroups = "Zone Security Center Insights Read,Bot Management Read,Analytics Read,Zone WAF Read"

	assert.Equal(t, []string{
		"Account Security Center Insights Read@" + acct1,
		zoneGroups + "@zone." + zoneA, zoneGroups + "@zone." + zoneB, zoneGroups + "@zone." + zoneC,
	}, flat(t, table[0]))
	assert.Equal(t, []string{"Account Security Center Insights Read@" + acct1, zoneGroups + "@zone." + zoneB}, flat(t, table[1]))

	for _, p := range table {
		assert.False(t, p.R2)
		assert.Equal(t, DefaultLifetime, p.Lifetime)
		assert.Equal(t, StoreNone, p.Store)
		assert.NotEmpty(t, p.Description)
		assert.NotContains(t, p.Description, "stack")

		for _, pol := range p.Policies {
			for _, g := range pol.PermissionGroups {
				assert.NotRegexp(t, `Write|Edit`, g, "insights is read-only")
			}
		}
	}

	_, err = Build(fixture(), []Row{{Name: "umbrella-insights", Kind: KindInsights, Account: "umbrella", WAFZones: []string{"a.example"}}})
	require.ErrorContains(t, err, "for the zone kind")
}

// The insights-admin kind holds exactly the Security Center Insights groups,
// Write and Read, on the account and on each zone, and nothing else.
func TestInsightsAdminIsOnlyInsights(t *testing.T) {
	table, err := Build(fixture(), []Row{
		{Name: "umbrella-insights-admin", Kind: KindInsightsAdmin, Account: "umbrella"},
		{Name: "umbrella-insights-admin-one", Kind: KindInsightsAdmin, Account: "umbrella", Zones: []string{"b.example"}},
	})
	require.NoError(t, err)

	const (
		acctGroups = "Account Security Center Insights Write,Account Security Center Insights Read"
		zoneGroups = "Zone Security Center Insights Write,Zone Security Center Insights Read"
	)

	assert.Equal(t, []string{
		acctGroups + "@" + acct1,
		zoneGroups + "@zone." + zoneA, zoneGroups + "@zone." + zoneB, zoneGroups + "@zone." + zoneC,
	}, flat(t, table[0]))
	assert.Equal(t, []string{acctGroups + "@" + acct1, zoneGroups + "@zone." + zoneB}, flat(t, table[1]))

	for _, p := range table {
		assert.False(t, p.R2)
		assert.Equal(t, DefaultLifetime, p.Lifetime)
		assert.Equal(t, StoreNone, p.Store)
		assert.NotEmpty(t, p.Description)

		for _, pol := range p.Policies {
			for _, g := range pol.PermissionGroups {
				assert.Regexp(t, `^(Account|Zone) Security Center Insights (Read|Write)$`, g)
			}
		}
	}

	// Only insights-admin writes insights; insights stays read-only.
	ro, err := Build(fixture(), []Row{{Name: "umbrella-insights", Kind: KindInsights, Account: "umbrella"}})
	require.NoError(t, err)

	for _, pol := range ro[0].Policies {
		for _, g := range pol.PermissionGroups {
			assert.NotRegexp(t, `Write|Edit`, g)
		}
	}

	_, err = Build(fixture(), []Row{{Name: "umbrella-insights-admin", Kind: KindInsightsAdmin, Account: "umbrella", DNSOnlyZones: []string{"a.example"}}})
	require.ErrorContains(t, err, "for the zone kind")
}

// Bot Management Write is granted only on the zones a row lists, and a row
// that lists none builds exactly what it built before.
func TestBotManagementZonesGrant(t *testing.T) {
	row := Row{Name: "umbrella-zone", Kind: KindZone, Account: "umbrella", Zones: []string{"a.example"}}

	plain, err := Build(fixture(), []Row{row})
	require.NoError(t, err)

	row.BotManagementZones = []string{"a.example"}
	with, err := Build(fixture(), []Row{row})
	require.NoError(t, err)

	p0, p1 := plain[0].Policies, with[0].Policies
	require.Len(t, p1, len(p0)+1)
	assert.Equal(t, p0, p1[:len(p0)], "the rest of the grant is untouched")
	assert.Equal(t, []string{"Bot Management Write"}, p1[len(p0)].PermissionGroups)

	for _, p := range p0 {
		assert.NotContains(t, p.PermissionGroups, "Bot Management Write")
	}
}
