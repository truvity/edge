package zonespec_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/zonespec"
)

// TestNoNonStdlibImports guards this package's reason to exist: a caller
// must be able to import zonespec alone, with no Pulumi or Cloudflare SDK
// riding along. See cloudflare/cfnames' test of the same name.
func TestNoNonStdlibImports(t *testing.T) {
	const pkg = "github.com/truvity/edge/v4/cloudflare/zonespec"

	out, err := exec.Command("go", "list", "-deps", pkg).Output()
	require.NoError(t, err, "go list -deps")

	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" || line == pkg {
			continue
		}

		// cloudflare/rulespec is this module's own standard-library-only leaf.
		if line == "github.com/truvity/edge/v4/cloudflare/rulespec" {
			continue
		}

		root, _, _ := strings.Cut(line, "/")
		assert.NotContains(t, root, ".", "cloudflare/zonespec must depend on the standard library only, found %q", line)
	}
}

// The full validation matrix is exercised through cloudflare/zone's aliases; this
// is the smoke test that the package stands on its own.
func TestValidate(t *testing.T) {
	require.NoError(t, (&zonespec.Args{ZoneID: "z", SSL: "strict"}).Validate())
	require.Error(t, (&zonespec.Args{ZoneID: "z", SSL: "bogus"}).Validate())
	require.Error(t, (&zonespec.Args{ZoneID: "z"}).Validate())
}

func TestDNSOnlyTypes(t *testing.T) {
	for typ, want := range map[string]bool{"NS": true, "TXT": true, "A": false, "AAAA": false, "CNAME": false} {
		assert.Equal(t, want, zonespec.DNSOnly(typ), typ)
	}
}

func TestPreservesPath(t *testing.T) {
	yes, no := true, false
	assert.True(t, (&zonespec.Redirect{}).PreservesPath(), "nil preserves the path")
	assert.True(t, (&zonespec.Redirect{PreservePath: &yes}).PreservesPath())
	assert.False(t, (&zonespec.Redirect{PreservePath: &no}).PreservesPath())
}

func TestExpressions(t *testing.T) {
	assert.Equal(t, `http.host in {"a.example.com"} or ends_with(http.host, ".example.com")`,
		zonespec.HostExpression([]string{"a.example.com", "*.example.com"}))

	tc := &zonespec.TrustedClients{Zone: "example.com", Hosts: []string{"b.example.com", "a.example.com"}, Ranges: []string{"192.0.2.0/24"}}
	assert.Equal(t, `(http.host in {"a.example.com" "b.example.com"}) and (ip.src in {192.0.2.0/24})`, zonespec.TrustedExpression(tc))
	assert.True(t, tc.Managed())

	var none *zonespec.TrustedClients
	assert.False(t, none.Managed())
}

func TestBotManagementStandsAlone(t *testing.T) {
	on := true
	require.NoError(t, (&zonespec.Args{ZoneID: "z", BotManagement: &zonespec.BotManagement{AIBots: zonespec.AIBotsBlock}}).Validate())
	require.NoError(t, (&zonespec.Args{ZoneID: "z", BotManagement: &zonespec.BotManagement{ManagedRobotsTxt: &on}}).Validate())
	require.Error(t, (&zonespec.Args{ZoneID: "z", BotManagement: &zonespec.BotManagement{AIBots: "on"}}).Validate())
	require.Error(t, (&zonespec.Args{ZoneID: "z", BotManagement: &zonespec.BotManagement{}}).Validate())
}

func TestHardeningStandsAlone(t *testing.T) {
	on := true
	require.NoError(t, (&zonespec.Args{ZoneID: "z", AlwaysUseHTTPS: &on}).Validate())
	require.NoError(t, (&zonespec.Args{ZoneID: "z", HSTS: &zonespec.HSTS{Enabled: true, MaxAge: 300}}).Validate())
	require.Error(t, (&zonespec.Args{ZoneID: "z", ManagedWAF: &zonespec.ManagedWAF{}}).Validate(),
		"an empty host list is refused, never read as the whole zone")

	assert.Equal(t, "true", zonespec.ManagedWAFExpression(&zonespec.ManagedWAF{WholeZone: true}))
	assert.Equal(t, `http.host in {"a.example.com" "b.example.com"}`,
		zonespec.ManagedWAFExpression(&zonespec.ManagedWAF{Zone: "example.com", Hosts: []string{"b.example.com", "a.example.com"}}))

	var none *zonespec.ManagedWAF
	assert.False(t, none.Managed())
}

func TestManagedWAFExpressionLengthIsGuarded(t *testing.T) {
	var hosts []string
	for i := 0; i < 400; i++ {
		hosts = append(hosts, fmt.Sprintf("host-%04d.example.com", i))
	}

	err := (&zonespec.Args{ZoneID: "z", ManagedWAF: &zonespec.ManagedWAF{Zone: "example.com", Hosts: hosts}}).Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "over Cloudflare's 4096 limit")

	few := hosts[:20]
	require.NoError(t, (&zonespec.Args{ZoneID: "z", ManagedWAF: &zonespec.ManagedWAF{Zone: "example.com", Hosts: few}}).Validate())
}

func TestRecordWAFCoverage(t *testing.T) {
	off := false
	on := true

	assert.True(t, zonespec.Record{Proxied: true}.WAFCovered(), "proxied defaults to covered")
	assert.True(t, zonespec.Record{Proxied: true, WAF: &on}.WAFCovered())
	assert.False(t, zonespec.Record{Proxied: true, WAF: &off}.WAFCovered())
	assert.False(t, zonespec.Record{}.WAFCovered(), "a DNS-only record is not behind the WAF")
}
