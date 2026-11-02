package tokensplit

import (
	"log/slog"
	"slices"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testAccountID = "acct-1"
	testZoneID    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tokenType     = "cloudflare:index/accountToken:AccountToken"
)

func zones() []Zone {
	return []Zone{
		{Name: "example.test", ID: testZoneID, AccountID: testAccountID},
		{Name: "other.test", ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", AccountID: "acct-2"},
	}
}

func inputs(w *recordingWriter, buckets ...Bucket) Inputs {
	return Inputs{
		RootToken: "root-token-value",
		AccountID: testAccountID,
		Zones:     zones(),
		Buckets:   buckets,
		Rotation:  Rotations{Edge: "1", Status: "1", R2Admin: "1", R2Parent: "1"},
		WAFZone:   "example.test",
		Writer:    w,
		Namespace: "ns",
		KeyPrefix: "cloudflare",
	}
}

func TestDeployEdgeAndStatusScopeToTheAccountsOwnZones(t *testing.T) {
	m, w := deploy(t, inputs(&recordingWriter{}))
	_ = w

	assert.Equal(t, []string{"edge", "r2-admin", "status"}, m.names(tokenType))

	edge := m.policies("edge")
	require.Len(t, edge, 3)
	assertPolicy(t, edge[0], []string{PermCloudflareTunnelWrite}, "com.cloudflare.api.account."+testAccountID)
	assertPolicy(t, edge[1],
		[]string{PermDNSWrite, PermSSLAndCertificatesWrite, PermZoneSettingsWrite, PermCacheSettingsWrite, PermConfigSettingsWrite},
		"com.cloudflare.api.account.zone."+testZoneID)
	// Exactly one added permission, Zone WAF Write, on exactly the named zone.
	assertPolicy(t, edge[2], []string{"Zone WAF Write"}, "com.cloudflare.api.account.zone."+testZoneID)
	assert.Equal(t, "cloudflare-edge-1", m.input("edge", "name"))

	status := m.policies("status")
	require.Len(t, status, 2)
	assertPolicy(t, status[0], []string{PermCloudflareTunnelWrite}, "com.cloudflare.api.account."+testAccountID)
	assertPolicy(t, status[1], []string{PermDNSWrite}, "com.cloudflare.api.account.zone."+testZoneID)

	admin := m.policies("r2-admin")
	require.Len(t, admin, 1)
	assertPolicy(t, admin[0], []string{PermWorkersR2StorageWrite}, "com.cloudflare.api.account."+testAccountID)
}

func TestDeployRedirectZonesAreScoped(t *testing.T) {
	in := inputs(&recordingWriter{})
	in.WAFZone = ""
	in.RedirectZones = []string{"example.test"}

	m, _ := deploy(t, in)

	edge := m.policies("edge")
	require.Len(t, edge, 3, "tunnel, baseline zone grant, redirect grant")
	assertPolicy(t, edge[2], []string{PermDynamicRedirectsWrite},
		"com.cloudflare.api.account.zone."+testZoneID)

	status := m.policies("status")
	assert.Len(t, status, 2, "the status child never gets the redirect grant")
}

func TestRedirectZoneIDsRefusesBadNames(t *testing.T) {
	txt := Zone{Name: "txt.test", ID: "cccccccccccccccccccccccccccccccc", AccountID: testAccountID, SecurityTxtOnly: true}

	for name, tc := range map[string]struct {
		zones []Zone
		names []string
		want  string
	}{
		"ok":                {zones(), []string{"example.test"}, ""},
		"empty":             {zones(), nil, ""},
		"unknown":           {zones(), []string{"missing.test"}, "in none of the declared zones"},
		"other account":     {zones(), []string{"other.test"}, "not in the account"},
		"security.txt only": {append(zones(), txt), []string{"txt.test"}, "security.txt only"},
		"duplicate":         {zones(), []string{"example.test", "example.test"}, "listed twice"},
	} {
		t.Run(name, func(t *testing.T) {
			ids, err := RedirectZoneIDs(tc.zones, testAccountID, MatchingZoneIDs(tc.zones, testAccountID), tc.names)
			if tc.want == "" {
				require.NoError(t, err)
				if len(tc.names) > 0 {
					assert.Equal(t, []string{testZoneID}, ids)
				}

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestDeployWithoutAWAFZoneAddsNoWAFGrant(t *testing.T) {
	in := inputs(&recordingWriter{})
	in.WAFZone = ""

	m, _ := deploy(t, in)

	assert.Len(t, m.policies("edge"), 2)
}

func TestDeployR2ParentOnePerBucketAndPassesJurisdiction(t *testing.T) {
	w := &recordingWriter{}
	m, _ := deploy(t, inputs(w,
		Bucket{Name: "scratch", Bucket: "scratch-bucket"},
		Bucket{Name: "archive", Bucket: "archive-bucket", Jurisdiction: "eu"},
	))

	assert.Equal(t, []string{"edge", "r2-admin", "r2-parent-archive", "r2-parent-scratch", "status"}, m.names(tokenType))

	scratch := m.policies("r2-parent-scratch")
	require.Len(t, scratch, 1)
	assertPolicy(t, scratch[0], []string{PermWorkersR2StorageBucketItemWrite},
		"com.cloudflare.edge.r2.bucket."+testAccountID+"_default_scratch-bucket")

	archive := m.policies("r2-parent-archive")
	require.Len(t, archive, 1)
	assertPolicy(t, archive[0], []string{PermWorkersR2StorageBucketItemWrite},
		"com.cloudflare.edge.r2.bucket."+testAccountID+"_eu_archive-bucket")
	assert.Equal(t, "cloudflare-r2-parent-archive-1", m.input("r2-parent-archive", "name"))

	assert.Equal(t, map[string][]string{
		"cloudflare/edge":              {"account-id", "api-token"},
		"cloudflare/status":            {"account-id", "api-token"},
		"cloudflare/r2-admin":          {"account-id", "api-token"},
		"cloudflare/r2-parent-scratch": {"account-id", "api-token", "token-id"},
		"cloudflare/r2-parent-archive": {"account-id", "api-token", "token-id"},
	}, w.keys())
}

func TestDeployWithoutR2AdminMintsNoBucketAdmin(t *testing.T) {
	w := &recordingWriter{}
	in := inputs(w)
	in.WithoutR2Admin = true

	m, _ := deploy(t, in)

	assert.Equal(t, []string{"edge", "status"}, m.names(tokenType))
	assert.Equal(t, map[string][]string{
		"cloudflare/edge":   {"account-id", "api-token"},
		"cloudflare/status": {"account-id", "api-token"},
	}, w.keys())
}

func TestDeployRefusesAnAccountWithNoZones(t *testing.T) {
	in := inputs(&recordingWriter{})
	in.Zones = zones()[1:]

	err := run(t, in, &mocks{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no zone belongs to account "+testAccountID)
}

func TestNoPermissionNamesAccountAPITokens(t *testing.T) {
	for _, perm := range []string{
		PermCloudflareTunnelWrite, PermDNSWrite, PermSSLAndCertificatesWrite, PermZoneSettingsWrite, PermCacheSettingsWrite,
		PermConfigSettingsWrite,
		PermWorkersR2StorageWrite, PermWorkersR2StorageBucketItemWrite,
	} {
		assert.NotContains(t, perm, "Account API Tokens")
	}
}

func TestSoleAccount(t *testing.T) {
	got, err := SoleAccount([]string{"one"}, "edge")
	require.NoError(t, err)
	assert.Equal(t, "one", got)

	_, err = SoleAccount([]string{"one", "two"}, "edge")
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"edge"`)
}

func TestParseRootTokenRefusesWhitespaceNamingTheFieldNeverTheValue(t *testing.T) {
	src := Source{Where: "ns kv/path", Hint: "write it first"}

	for name, value := range map[string]string{
		"trailing newline": "abc123\n", "leading space": " abc123", "embedded space": "abc 123",
		"tab": "abc123\t", "carriage return": "abc123\r",
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := ParseRootToken(map[string]string{"api-token": value, "account-id": "a"}, src)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "ns kv/path")
			assert.Contains(t, err.Error(), `"api-token"`)
			assert.NotContains(t, err.Error(), value)
		})
	}

	token, account, err := ParseRootToken(map[string]string{"api-token": "abc123", "account-id": "a"}, src)
	require.NoError(t, err)
	assert.Equal(t, "abc123", token)
	assert.Equal(t, "a", account)

	_, _, err = ParseRootToken(nil, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "write it first")

	_, _, err = ParseRootToken(map[string]string{"api-token": "x"}, src)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing api-token or account-id")
}

func TestParseChildChecksTheAccount(t *testing.T) {
	src := Source{Where: "ns kv/path"}
	data := map[string]string{"api-token": "t", "account-id": "acct-1"}

	token, err := ParseChild(data, src, "platform", "acct-1")
	require.NoError(t, err)
	assert.Equal(t, "t", token)

	_, err = ParseChild(data, src, "platform", "acct-9")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not match")
}

type recordingWriter struct {
	mu   sync.Mutex
	puts map[string][]string
}

func (w *recordingWriter) Put(_ *pulumi.Context, namespace, key string, properties map[string]pulumi.StringInput, _ ...pulumi.ResourceOption) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.puts == nil {
		w.puts = map[string][]string{}
	}

	if namespace != "ns" {
		return assert.AnError
	}

	for p := range properties {
		w.puts[key] = append(w.puts[key], p)
	}

	slices.Sort(w.puts[key])

	return nil
}

func (w *recordingWriter) keys() map[string][]string { return w.puts }

func run(t *testing.T, in Inputs, m *mocks) error {
	t.Helper()

	return pulumi.RunErr(func(ctx *pulumi.Context) error {
		return Deploy(ctx, slog.New(slog.DiscardHandler), in)
	}, pulumi.WithMocks("example", "root", m))
}

func deploy(t *testing.T, in Inputs) (*mocks, *recordingWriter) {
	t.Helper()

	m := &mocks{}
	require.NoError(t, run(t, in, m))

	return m, in.Writer.(*recordingWriter)
}

func assertPolicy(t *testing.T, p resource.PropertyValue, groups []string, resourceKey string) {
	t.Helper()

	obj := p.ObjectValue()

	var gotGroups []string

	for _, g := range obj["permissionGroups"].ArrayValue() {
		gotGroups = append(gotGroups, g.ObjectValue()["id"].StringValue())
	}

	slices.Sort(gotGroups)
	wantIDs := make([]string, len(groups))

	for i, g := range groups {
		wantIDs[i] = "group-" + g
	}

	slices.Sort(wantIDs)
	assert.Equal(t, wantIDs, gotGroups)
	assert.Contains(t, obj["resources"].StringValue(), `"`+resourceKey+`":"*"`)
}

type (
	recorded struct {
		typ, name string
		inputs    resource.PropertyMap
	}

	mocks struct {
		mu        sync.Mutex
		resources []recorded
	}
)

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resources = append(m.resources, recorded{args.TypeToken, args.Name, args.Inputs})

	return args.Name + "-id", args.Inputs.Copy(), nil
}

func (m *mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token == "cloudflare:index/getAccountApiTokenPermissionGroupsList:getAccountApiTokenPermissionGroupsList" {
		name := args.Args["name"].StringValue()

		return resource.PropertyMap{
			"results": resource.NewArrayProperty([]resource.PropertyValue{
				resource.NewObjectProperty(resource.PropertyMap{
					"id":   resource.NewStringProperty("group-" + name),
					"name": resource.NewStringProperty(name),
				}),
			}),
		}, nil
	}

	return resource.PropertyMap{}, nil
}

func (m *mocks) names(typ string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	var out []string

	for _, r := range m.resources {
		if r.typ == typ {
			out = append(out, r.name)
		}
	}

	slices.Sort(out)

	return out
}

func (m *mocks) input(name, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.resources {
		if r.typ == tokenType && r.name == name {
			return r.inputs[resource.PropertyKey(key)].StringValue()
		}
	}

	return ""
}

func (m *mocks) policies(name string) []resource.PropertyValue {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, r := range m.resources {
		if r.typ == tokenType && r.name == name {
			return r.inputs["policies"].ArrayValue()
		}
	}

	return nil
}
