package account

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertionError(msg string) error { return errors.New(msg) }

const (
	accountTokenType     = "cloudflare:index/accountToken:AccountToken"
	permissionGroupToken = "cloudflare:index/getAccountApiTokenPermissionGroupsList:getAccountApiTokenPermissionGroupsList"
)

// childTokenMocks mirrors cloudflare/r2's own mocks: it records every registered
// resource so tests can assert on exact names and inputs, and answers the
// permission-group lookup invoke from a fixed name -> id table, refusing
// (rather than silently succeeding) an invoke with no explicit provider —
// the same regression cloudflare/r2 shipped in v2.2.0 and fixed in v2.3.0, now
// guarded here since the lookup itself lives in this package.
type childTokenMocks struct {
	mu      sync.Mutex
	res     map[string]resource.PropertyMap
	regRPCs map[string]*pulumirpc.RegisterResourceRequest
	groups  map[string][]string

	lastPermissionGroupProvider string
}

func newChildTokenMocks() *childTokenMocks {
	return &childTokenMocks{
		res:     map[string]resource.PropertyMap{},
		regRPCs: map[string]*pulumirpc.RegisterResourceRequest{},
		groups: map[string][]string{
			"Workers KV Storage Write":             {"group-kv-write-id"},
			"DNS Write":                            {"group-dns-write-id"},
			"Zone Settings Write":                  {"group-zone-settings-write-id"},
			"Workers R2 Storage Bucket Item Write": {"group-r2-write-id"},
			"Ambiguous Group":                      {"group-dup-1", "group-dup-2"},

			// The preset-only permission groups (account.EdgePolicies,
			// account.R2AdminPolicies): real Cloudflare API names, per
			// CL1a's own citation, not the dashboard's "Edit" label.
			"Cloudflare Tunnel Write":     {"group-tunnel-write-id"},
			"SSL and Certificates Write":  {"group-ssl-write-id"},
			"Cache Settings Write":        {"group-cache-settings-write-id"},
			"Config Settings Write":       {"group-config-settings-write-id"},
			"Dynamic URL Redirects Write": {"group-dynamic-url-redirects-write-id"},
			"Workers R2 Storage Write":    {"group-r2-admin-write-id"},
		},
	}
}

func (m *childTokenMocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	key := args.TypeToken + "/" + args.Name
	m.res[key] = args.Inputs
	m.regRPCs[key] = args.RegisterRPC

	out := args.Inputs.Copy()
	id := args.Name + "-id"

	if args.TypeToken == accountTokenType {
		id = "token-id-123"
		out["value"] = resource.NewStringProperty("tok-value-abc123")
	}

	return id, out, nil
}

func (m *childTokenMocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token != permissionGroupToken {
		return resource.PropertyMap{}, nil
	}

	m.mu.Lock()
	m.lastPermissionGroupProvider = args.Provider
	m.mu.Unlock()

	if args.Provider == "" {
		return resource.PropertyMap{}, assertionError("getAccountApiTokenPermissionGroupsList invoked with no explicit provider — " +
			"NewChildToken must thread acct's own provider into this invoke")
	}

	if !args.Args.HasValue("accountId") || args.Args["accountId"].StringValue() == "" {
		return resource.PropertyMap{}, assertionError("getAccountApiTokenPermissionGroupsList invoked with no accountId")
	}

	name := args.Args["name"].StringValue()

	ids, ok := m.groups[name]
	if !ok {
		return resource.PropertyMap{
			"results": resource.NewArrayProperty([]resource.PropertyValue{}),
		}, nil
	}

	results := make([]resource.PropertyValue, 0, len(ids))
	for _, id := range ids {
		results = append(results, resource.NewObjectProperty(resource.PropertyMap{
			"id":     resource.NewStringProperty(id),
			"name":   resource.NewStringProperty(name),
			"scopes": resource.NewArrayProperty([]resource.PropertyValue{}),
		}))
	}

	return resource.PropertyMap{
		"results": resource.NewArrayProperty(results),
	}, nil
}

func withProvider(t *testing.T, ctx *pulumi.Context, accountID string) *Account {
	t.Helper()

	acct, err := New(ctx, "acct", Args{AccountID: accountID}, pulumi.String("token-"+accountID))
	require.NoError(t, err)

	return acct
}

func resolveString(t *testing.T, o pulumi.StringOutput) string {
	t.Helper()

	ch := make(chan string, 1)
	o.ApplyT(func(v string) string { ch <- v; return v })

	return <-ch
}

func zoneScopedConfig() ChildTokenConfig {
	return ChildTokenConfig{
		Name: "ci-zone-token",
		Policies: []ChildTokenPolicy{
			{
				PermissionGroups: []string{"DNS Write", "Zone Settings Write"},
				Scope:            ZoneScope{ZoneID: "0123456789abcdef0123456789abcdef"},
			},
		},
	}
}

func TestZoneScopedMultiGroupToken(t *testing.T) {
	m := newChildTokenMocks()

	var ct *ChildToken
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		var err error
		ct, err = NewChildToken(ctx, "ci-token", acct, zoneScopedConfig())

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	tok := m.res[accountTokenType+"/ci-token"]
	require.True(t, tok.HasValue("policies"))

	policies := tok["policies"].ArrayValue()
	require.Len(t, policies, 1)

	policy := policies[0].ObjectValue()
	assert.Equal(t, "allow", policy["effect"].StringValue())

	groups := policy["permissionGroups"].ArrayValue()
	require.Len(t, groups, 2, "both permission groups land in the one policy")
	assert.Equal(t, "group-dns-write-id", groups[0].ObjectValue()["id"].StringValue())
	assert.Equal(t, "group-zone-settings-write-id", groups[1].ObjectValue()["id"].StringValue())

	var resources map[string]string
	require.NoError(t, json.Unmarshal([]byte(policy["resources"].StringValue()), &resources))
	require.Len(t, resources, 1)
	assert.Equal(t, "*", resources["com.cloudflare.api.account.zone.0123456789abcdef0123456789abcdef"])

	assert.Equal(t, "example-account-id", tok["accountId"].StringValue())
	assert.Equal(t, "ci-zone-token", tok["name"].StringValue())

	require.NotNil(t, ct)
	assert.Equal(t, "token-id-123", resolveString(t, ct.ID))
}

func TestAccountScopedToken(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, ChildTokenConfig{
			Name: "ci-account-token",
			Policies: []ChildTokenPolicy{
				{PermissionGroups: []string{"Workers KV Storage Write"}, Scope: WholeAccountScope{}},
			},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	tok := m.res[accountTokenType+"/ci-token"]
	policy := tok["policies"].ArrayValue()[0].ObjectValue()

	var resources map[string]string
	require.NoError(t, json.Unmarshal([]byte(policy["resources"].StringValue()), &resources))
	assert.Equal(t, map[string]string{"com.cloudflare.api.account.example-account-id": "*"}, resources)
}

func TestBucketScopedToken(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, ChildTokenConfig{
			Name: "ci-bucket-token",
			Policies: []ChildTokenPolicy{
				{
					PermissionGroups: []string{"Workers R2 Storage Bucket Item Write"},
					Scope:            R2BucketScope{Jurisdiction: "eu", Bucket: "example-bucket"},
				},
			},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	tok := m.res[accountTokenType+"/ci-token"]
	policy := tok["policies"].ArrayValue()[0].ObjectValue()

	var resources map[string]string
	require.NoError(t, json.Unmarshal([]byte(policy["resources"].StringValue()), &resources))
	assert.Equal(t, map[string]string{"com.cloudflare.edge.r2.bucket.example-account-id_eu_example-bucket": "*"}, resources)
}

func TestBucketScopeDefaultJurisdiction(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, ChildTokenConfig{
			Name: "ci-bucket-token",
			Policies: []ChildTokenPolicy{
				{PermissionGroups: []string{"Workers R2 Storage Bucket Item Write"}, Scope: R2BucketScope{Bucket: "example-bucket"}},
			},
		})

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	tok := m.res[accountTokenType+"/ci-token"]
	policy := tok["policies"].ArrayValue()[0].ObjectValue()

	var resources map[string]string
	require.NoError(t, json.Unmarshal([]byte(policy["resources"].StringValue()), &resources))
	assert.Equal(t, map[string]string{"com.cloudflare.edge.r2.bucket.example-account-id_default_example-bucket": "*"}, resources)
}

// TestRotationChangesNameAndIsReplaceOnChange mirrors cloudflare/r2's own
// rotation test: Rotation embeds into the Cloudflare-visible Name, and
// "name" is a ReplaceOnChanges property, so the one field Rotation
// controls is exactly the one Pulumi is told never to update quietly.
func TestRotationChangesNameAndIsReplaceOnChange(t *testing.T) {
	m1 := newChildTokenMocks()
	cfg1 := zoneScopedConfig()
	cfg1.Rotation = "2026-09-01"
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, cfg1)

		return err
	}, pulumi.WithMocks("proj", "stack", m1))
	require.NoError(t, err)

	m2 := newChildTokenMocks()
	cfg2 := zoneScopedConfig()
	cfg2.Rotation = "2026-10-01"
	err = pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, cfg2)

		return err
	}, pulumi.WithMocks("proj", "stack", m2))
	require.NoError(t, err)

	name1 := m1.res[accountTokenType+"/ci-token"]["name"].StringValue()
	name2 := m2.res[accountTokenType+"/ci-token"]["name"].StringValue()
	assert.NotEqual(t, name1, name2)
	assert.Contains(t, name1, "2026-09-01")
	assert.Contains(t, name2, "2026-10-01")

	rpc1 := m1.regRPCs[accountTokenType+"/ci-token"]
	require.NotNil(t, rpc1)
	assert.Contains(t, rpc1.GetReplaceOnChanges(), "name")

	rpc2 := m2.regRPCs[accountTokenType+"/ci-token"]
	require.NotNil(t, rpc2)
	assert.Contains(t, rpc2.GetReplaceOnChanges(), "name")
}

func TestExpiresOnIsPassedThrough(t *testing.T) {
	m := newChildTokenMocks()
	cfg := zoneScopedConfig()
	cfg.ExpiresOn = "2099-01-01T00:00:00Z"

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, cfg)

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	tok := m.res[accountTokenType+"/ci-token"]
	assert.Equal(t, "2099-01-01T00:00:00Z", tok["expiresOn"].StringValue())
}

func TestValueIsMarkedSecret(t *testing.T) {
	m := newChildTokenMocks()

	var ct *ChildToken
	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")

		var err error
		ct, err = NewChildToken(ctx, "ci-token", acct, zoneScopedConfig())

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	assert.True(t, pulumi.IsSecret(ct.Value), "Value must be secret")
	assert.False(t, pulumi.IsSecret(ct.ID), "ID is not a secret")
	assert.Equal(t, "tok-value-abc123", resolveString(t, ct.Value))
}

func TestPermissionGroupLookupCarriesTheExplicitProvider(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, zoneScopedConfig())

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.NoError(t, err)

	assert.NotEmpty(t, m.lastPermissionGroupProvider, "the invoke must carry acct's explicit provider")
}

// TestPermissionGroupLookupWithNoExplicitProviderFails is NewChildToken's
// own copy of cloudflare/r2's regression test for gitops#1700 / INF-r2-invoke: an
// acct built with no Provider at all must not silently resolve the
// permission-group lookup against a default provider.
func TestPermissionGroupLookupWithNoExplicitProviderFails(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := NewChildToken(ctx, "ci-token", &Account{AccountID: "example-account-id"}, zoneScopedConfig())

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no explicit provider")
}

func TestUnknownPermissionGroupNameFails(t *testing.T) {
	m := newChildTokenMocks()
	cfg := zoneScopedConfig()
	cfg.Policies[0].PermissionGroups = []string{"Does Not Exist"}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, cfg)

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestAmbiguousPermissionGroupNameFails(t *testing.T) {
	m := newChildTokenMocks()
	cfg := zoneScopedConfig()
	cfg.Policies[0].PermissionGroups = []string{"Ambiguous Group"}

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		acct := withProvider(t, ctx, "example-account-id")
		_, err := NewChildToken(ctx, "ci-token", acct, cfg)

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

func TestNewChildTokenRequiresAcct(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := NewChildToken(ctx, "ci-token", nil, zoneScopedConfig())

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "acct is required")
}

func TestNewChildTokenRequiresAcctAccountID(t *testing.T) {
	m := newChildTokenMocks()

	err := pulumi.RunErr(func(ctx *pulumi.Context) error {
		_, err := NewChildToken(ctx, "ci-token", &Account{}, zoneScopedConfig())

		return err
	}, pulumi.WithMocks("proj", "stack", m))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "AccountID is required")
}

func TestValidR2BucketName(t *testing.T) {
	assert.True(t, ValidR2BucketName("example-bucket"))
	assert.False(t, ValidR2BucketName("ab"), "too short")
	assert.False(t, ValidR2BucketName("Example-Bucket"), "upper case")
	assert.False(t, ValidR2BucketName("-example"), "leading hyphen")
	assert.False(t, ValidR2BucketName("example-"), "trailing hyphen")
}

func TestValidR2Jurisdiction(t *testing.T) {
	for _, j := range []string{"", "default", "eu", "fedramp", "us"} {
		assert.True(t, ValidR2Jurisdiction(j), j)
	}
	assert.False(t, ValidR2Jurisdiction("mars"))
}

func TestChildTokenConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*ChildTokenConfig)
		want string
	}{
		{"no name", func(c *ChildTokenConfig) { c.Name = "" }, "name is required"},
		{"no policies", func(c *ChildTokenConfig) { c.Policies = nil }, "at least one policy"},
		{"no permission groups", func(c *ChildTokenConfig) { c.Policies[0].PermissionGroups = nil }, "at least one permission group"},
		{"empty permission group name", func(c *ChildTokenConfig) { c.Policies[0].PermissionGroups = []string{""} }, "must not be empty"},
		{
			"account api tokens read forbidden",
			func(c *ChildTokenConfig) { c.Policies[0].PermissionGroups = []string{"Account API Tokens Read"} },
			"must never be granted to a child token",
		},
		{
			"account api tokens write forbidden",
			func(c *ChildTokenConfig) { c.Policies[0].PermissionGroups = []string{"Account API Tokens Write"} },
			"must never be granted to a child token",
		},
		{"nil scope", func(c *ChildTokenConfig) { c.Policies[0].Scope = nil }, "scope is unset or unknown"},
		{"zone id not hex", func(c *ChildTokenConfig) { c.Policies[0].Scope = ZoneScope{ZoneID: "not-hex!"} }, "32-character lowercase hex"},
		{"zone id wrong length", func(c *ChildTokenConfig) { c.Policies[0].Scope = ZoneScope{ZoneID: "abc123"} }, "32-character lowercase hex"},
		{
			"bucket scope bad name",
			func(c *ChildTokenConfig) { c.Policies[0].Scope = R2BucketScope{Bucket: "Bad_Bucket"} },
			"lowercase letters",
		},
		{
			"bucket scope bad jurisdiction",
			func(c *ChildTokenConfig) {
				c.Policies[0].Scope = R2BucketScope{Bucket: "example-bucket", Jurisdiction: "mars"}
			},
			"jurisdiction",
		},
		{"expiresOn not RFC3339", func(c *ChildTokenConfig) { c.ExpiresOn = "yesterday" }, "not RFC3339"},
		{"expiresOn in the past", func(c *ChildTokenConfig) { c.ExpiresOn = "2000-01-01T00:00:00Z" }, "must be in the future"},
		{"rotation bad characters", func(c *ChildTokenConfig) { c.Rotation = "not a valid name!" }, "rotation"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := zoneScopedConfig()
			tc.mut(&cfg)
			err := cfg.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestChildTokenConfigValidateAcceptsAWellFormedConfig(t *testing.T) {
	cfg := zoneScopedConfig()
	cfg.ExpiresOn = "2099-01-01T00:00:00Z"
	cfg.Rotation = "rev-1"
	require.NoError(t, cfg.Validate())
}
