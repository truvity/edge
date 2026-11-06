package probe_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/probe"
)

const token = "tok-secret"

// live is what the fake API answers for one zone.
type live struct {
	ssl, minTLS string
	totalTLS    string // the result object, JSON
	records     map[string][]map[string]string
}

// serve is a fake Cloudflare API for zone "zid". It records every request
// and fails the test on anything but a GET.
func serve(t *testing.T, l live) (*probe.Client, *[]string) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()

		assert.Equal(t, http.MethodGet, r.Method, "the probe only reads")
		assert.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))

		write := func(result any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": result})
		}

		switch r.URL.Path {
		case "/zones/zid/settings/ssl":
			write(map[string]any{"id": "ssl", "value": l.ssl})
		case "/zones/zid/settings/min_tls_version":
			write(map[string]any{"id": "min_tls_version", "value": l.minTLS})
		case "/zones/zid/acm/total_tls":
			var v any
			require.NoError(t, json.Unmarshal([]byte(l.totalTLS), &v))
			write(v)
		case "/zones/zid/dns_records":
			write(l.records[r.URL.Query().Get("name")])
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":7003,"message":"no route"}],"result":null}`))
		}
	}))
	t.Cleanup(srv.Close)

	c := probe.NewClient(token)
	c.API.Base = srv.URL

	return c, &seen
}

func good() live {
	return live{
		ssl: "strict", minTLS: "1.2",
		totalTLS: `{"enabled":true,"certificate_authority":"lets_encrypt"}`,
		records: map[string][]map[string]string{
			"console.example.org": {{"type": "CNAME", "name": "console.example.org", "content": "abc.cfargotunnel.com"}},
			"status.example.org":  {{"type": "CNAME", "name": "status.example.org", "content": "pages.example.net"}},
		},
	}
}

func declared() probe.Declared {
	return probe.Declared{
		Zone: "example.org", ZoneID: "zid", SSLMode: "strict", MinTLSVersion: "1.2",
		TotalTLS: &probe.TotalTLS{Enabled: true, CertificateAuthority: "lets_encrypt"},
		Names: []probe.Name{
			{Host: "console.example.org", Type: "CNAME", Target: "abc.cfargotunnel.com"},
			{Host: "status.example.org", Type: "CNAME"},
		},
	}
}

func TestAZoneThatMatchesItsDeclarationIsOK(t *testing.T) {
	c, seen := serve(t, good())

	r, err := c.Check(context.Background(), declared())
	require.NoError(t, err)
	assert.True(t, r.OK(), "%v", r.Drift)
	assert.Equal(t, 5, r.Checked, "ssl, min tls, total tls and two names")
	assert.NotEmpty(t, *seen)
}

func TestDriftIsReportedPerCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*live)
		check  string
		want   string
		got    string
	}{
		"ssl mode":      {func(l *live) { l.ssl = "full" }, "ssl", "strict", "full"},
		"min tls":       {func(l *live) { l.minTLS = "1.0" }, "min_tls_version", "1.2", "1.0"},
		"total tls off": {func(l *live) { l.totalTLS = `{"enabled":false}` }, "total_tls", "enabled", "disabled"},
		"total tls ca":  {func(l *live) { l.totalTLS = `{"enabled":true,"certificate_authority":"google"}` }, "total_tls", "lets_encrypt", "google"},
		"missing name":  {func(l *live) { delete(l.records, "console.example.org") }, "name", "CNAME abc.cfargotunnel.com", "no record"},
		"wrong target": {func(l *live) {
			l.records["console.example.org"][0]["content"] = "other.cfargotunnel.com"
		}, "name", "CNAME abc.cfargotunnel.com", "other.cfargotunnel.com"},
	} {
		t.Run(name, func(t *testing.T) {
			l := good()
			tc.mutate(&l)
			c, _ := serve(t, l)

			r, err := c.Check(context.Background(), declared())
			require.NoError(t, err)
			require.False(t, r.OK())
			require.Len(t, r.Drift, 1)
			assert.Equal(t, tc.check, r.Drift[0].Check)
			assert.Equal(t, tc.want, r.Drift[0].Want)
			assert.Contains(t, r.Drift[0].Got, tc.got)
		})
	}
}

func TestATargetIsComparedWithoutCaseOrTrailingDot(t *testing.T) {
	l := good()
	l.records["console.example.org"][0]["content"] = "ABC.cfargotunnel.com."
	c, _ := serve(t, l)

	r, err := c.Check(context.Background(), declared())
	require.NoError(t, err)
	assert.True(t, r.OK(), "%v", r.Drift)
}

func TestWhatIsNotDeclaredIsNotChecked(t *testing.T) {
	c, seen := serve(t, live{ssl: "off"})

	r, err := c.Check(context.Background(), probe.Declared{Zone: "example.org", ZoneID: "zid"})
	require.NoError(t, err)
	assert.True(t, r.OK())
	assert.Zero(t, r.Checked)
	assert.Empty(t, *seen, "no declaration, no request")
}

func TestAnAPIFailureIsAnErrorNotDrift(t *testing.T) {
	c, _ := serve(t, good())

	d := declared()
	d.ZoneID = "other"

	_, err := c.Check(context.Background(), d)
	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
}

func TestCheckNeedsAZoneID(t *testing.T) {
	c, _ := serve(t, good())

	_, err := c.Check(context.Background(), probe.Declared{Zone: "example.org", SSLMode: "strict"})
	require.ErrorContains(t, err, "zone id")
}

const registry = `accounts:
  main: {account_id: a1}
zones:
  example.org: {account: main, zone_id: zid, ssl_mode: strict, min_tls_version: "1.2", total_tls: {enabled: true, certificate_authority: lets_encrypt}}
  plain.example: {account: main, zone_id: z2, ssl_mode: full}
  txt.example: {account: main, zone_id: z3, manage: security_txt, security_txt: {contact: ["mailto:security@example.com"], expires: "2027-01-01T00:00:00Z"}}
  later.example: {account: main, zone_id_pending: true, ssl_mode: full}
fleets:
  internal: {service: gateway-internal, namespace: envoy-gateway-system}
`

func TestDeclaredFromReadsTheRegistry(t *testing.T) {
	cfg, err := config.Parse([]byte(registry))
	require.NoError(t, err)

	d, err := probe.DeclaredFrom(cfg, "example.org")
	require.NoError(t, err)
	assert.Equal(t, probe.Declared{
		Zone: "example.org", ZoneID: "zid", SSLMode: "strict", MinTLSVersion: "1.2",
		TotalTLS: &probe.TotalTLS{Enabled: true, CertificateAuthority: "lets_encrypt"},
	}, d)

	d, err = probe.DeclaredFrom(cfg, "plain.example")
	require.NoError(t, err)
	assert.Nil(t, d.TotalTLS, "a setting the registry leaves unmanaged is not probed")
	assert.Empty(t, d.MinTLSVersion)

	for name, zone := range map[string]string{"unknown": "nope.example", "security.txt only": "txt.example", "pending": "later.example"} {
		_, err := probe.DeclaredFrom(cfg, zone)
		require.Error(t, err, name)
	}
}

func TestNamesFromListsEveryProviderNameWithItsTarget(t *testing.T) {
	hosts := []string{"status.example.org", "app.example.org", "console.example.org", "app.example.org"}

	got := probe.NamesFrom(hosts, map[string]string{"console.example.org": "abc.cfargotunnel.com"})
	require.Len(t, got, 3, "a repeated host is listed once")

	byHost := map[string]probe.Name{}
	for _, n := range got {
		byHost[n.Host] = n
	}

	assert.Equal(t, probe.Name{Host: "console.example.org", Type: "CNAME", Target: "abc.cfargotunnel.com"}, byHost["console.example.org"])
	assert.Equal(t, probe.Name{Host: "status.example.org", Type: "CNAME"}, byHost["status.example.org"])
	assert.True(t, strings.HasSuffix(got[0].Host, ".example.org"))
	assert.Equal(t, []string{"app.example.org", "console.example.org", "status.example.org"},
		[]string{got[0].Host, got[1].Host, got[2].Host}, "sorted")
}
