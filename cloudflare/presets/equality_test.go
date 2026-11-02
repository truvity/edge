package presets

import (
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The registry and rows in testdata are synthetic (two accounts, four zones,
// eight rows, one WAF zone). golden.json is the table they build, dumped as the
// documents Cloudflare receives: it pins every policy, group, scope, order,
// name, description, grant and lifetime a kind produces, so a change to any of
// them is a visible diff. Regenerate it with `go test ./cloudflare/presets -update`
// and read the diff: a consumer's prototypes change with it.

var update = flag.Bool("update", false, "rewrite testdata/golden.json from the rows")

type testRegistry struct {
	Accounts []struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"accounts"`
	Zones []struct {
		Name    string `json:"name"`
		ID      string `json:"id"`
		Account string `json:"account"`
	} `json:"zones"`
}

type testRow struct {
	Name          string   `json:"name"`
	Kind          string   `json:"kind"`
	Account       string   `json:"account"`
	WAFZones      []string `json:"waf_zones"`
	RedirectZones []string `json:"redirect_zones"`
	Grants        []string `json:"grants"`
}

type goldenPreset struct {
	Name        string           `json:"name"`
	Account     string           `json:"account"`
	Kind        string           `json:"kind"`
	Description string           `json:"description"`
	Lifetime    string           `json:"lifetime"`
	Store       string           `json:"store"`
	Grants      []string         `json:"grants"`
	Policies    []PolicyDocument `json:"policies"`
}

func readJSON(t *testing.T, name string, into any) []byte {
	t.Helper()

	data, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, into))

	return data
}

func inputs(t *testing.T) (Registry, []Row) {
	t.Helper()

	var (
		tr   testRegistry
		trow []testRow
	)

	readJSON(t, "registry.json", &tr)
	readJSON(t, "rows.json", &trow)

	var reg Registry
	for _, a := range tr.Accounts {
		reg.Accounts = append(reg.Accounts, Account{Name: a.Name, ID: a.ID})
	}

	for _, z := range tr.Zones {
		reg.Zones = append(reg.Zones, Zone{Name: z.Name, ID: z.ID, Account: z.Account})
	}

	var rows []Row
	for _, r := range trow {
		rows = append(rows, Row{Name: r.Name, Kind: r.Kind, Account: r.Account, WAFZones: r.WAFZones, RedirectZones: r.RedirectZones, Grants: r.Grants})
	}

	return reg, rows
}

func dump(t *testing.T, table []Preset) []byte {
	t.Helper()

	out := make([]goldenPreset, 0, len(table))

	for i := range table {
		p := &table[i]

		docs, err := p.Documents()
		require.NoError(t, err)

		out = append(out, goldenPreset{
			Name: p.Name, Account: p.Account, Kind: p.Kind, Description: p.Description,
			Lifetime: p.Lifetime.String(), Store: string(p.Store), Grants: p.Grants, Policies: docs,
		})
	}

	b, err := json.MarshalIndent(out, "", "  ")
	require.NoError(t, err)

	return append(b, '\n')
}

// The six presets of the two accounts: the policy documents and every name,
// byte for byte as the golden.
func TestGolden(t *testing.T) {
	reg, rows := inputs(t)

	table, err := Build(reg, rows)
	require.NoError(t, err)
	require.Len(t, table, 8)

	if *update {
		require.NoError(t, os.WriteFile("testdata/golden.json", dump(t, table), 0o644))
	}

	var golden []goldenPreset

	want := readJSON(t, "golden.json", &golden)
	require.Len(t, golden, 8)

	assert.Equal(t, string(want), string(dump(t, table)))

	// BuildReady is the same when no account has a pending zone.
	ready, err := BuildReady(reg, rows)
	require.NoError(t, err)
	assert.Equal(t, string(want), string(dump(t, ready)))

	for _, p := range table {
		assert.Equal(t, time.Hour, p.Lifetime, p.Name)
		assert.Equal(t, StoreNone, p.Store, p.Name)
		assert.False(t, p.R2, p.Name)
	}
}

// StandardRows plus the account's grant gives the same table.
func TestStandardRowsEqualTheRows(t *testing.T) {
	reg, rows := inputs(t)

	var std []Row

	for _, a := range []struct {
		name string
		waf  []string
	}{{"globex", nil}, {"acme", []string{"example.com"}}} {
		std = append(std, StandardRows(a.name, []string{"example:deploy"}, a.waf, nil)...)
	}

	want, err := Build(reg, rows)
	require.NoError(t, err)

	got, err := Build(reg, std)
	require.NoError(t, err)

	assert.Equal(t, string(dump(t, want)), string(dump(t, got)))
}

// An account with a zone that has no id yet contributes no preset to the
// table, and its own presets are refused naming the zone.
func TestPendingZone(t *testing.T) {
	reg, rows := inputs(t)

	for i := range reg.Zones {
		if reg.Zones[i].Name == "example.dev" {
			reg.Zones[i].ID = ""
		}
	}

	ready, err := BuildReady(reg, rows)
	require.NoError(t, err)
	require.Len(t, ready, 4)

	for _, p := range ready {
		assert.Equal(t, "acme", p.Account)
	}

	_, err = ForAccount(reg, rows, "globex")
	require.ErrorContains(t, err, "zones[example.dev] of account globex has no zone_id")

	_, err = Build(reg, rows)
	require.ErrorContains(t, err, "zone_id_pending")

	own, err := ForAccount(reg, rows, "acme")
	require.NoError(t, err)
	assert.Len(t, own, 4)

	_, err = ForAccount(reg, rows, "nobody")
	require.ErrorContains(t, err, `no account "nobody"`)
}
