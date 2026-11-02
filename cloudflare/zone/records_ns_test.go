package zone

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nsRecords is one child-zone delegation with four distinct targets, the
// shape a parent zone declares for a child served elsewhere.
func nsRecords() *Records {
	return &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "team.example.com", Type: "NS", Content: "ns1.nameservers.example", TTL: 300},
			{Name: "team.example.com", Type: "NS", Content: "ns2.nameservers.example", TTL: 300},
			{Name: "team.example.com", Type: "NS", Content: "ns3.nameservers.example", TTL: 300},
			{Name: "team.example.com", Type: "NS", Content: "ns4.nameservers.example", TTL: 300},
		},
	}
}

func withNS(mutate func(*Records)) Args {
	r := nsRecords()
	mutate(r)

	return Args{ZoneID: "zone-1", Records: r}
}

// TestNSDelegationRendersOneRecordPerTarget pins the four individual records
// and the child names: an NS set is not one multi-value resource.
func TestNSDelegationRendersOneRecordPerTarget(t *testing.T) {
	m := run(t, Args{ZoneID: "zone-1", Records: nsRecords()})

	want := []string{
		"record-example-team-example-com-ns-ns1-nameservers-example",
		"record-example-team-example-com-ns-ns2-nameservers-example",
		"record-example-team-example-com-ns-ns3-nameservers-example",
		"record-example-team-example-com-ns-ns4-nameservers-example",
	}

	for _, name := range want {
		rec, ok := m.res[dnsRecordType+"/"+name]
		require.True(t, ok, name)

		assert.Equal(t, "zone-1", rec["zoneId"].StringValue())
		assert.Equal(t, "team.example.com", rec["name"].StringValue())
		assert.Equal(t, "NS", rec["type"].StringValue())
		assert.False(t, rec["proxied"].BoolValue(), "a delegation is DNS only")
		assert.Equal(t, 300.0, rec["ttl"].NumberValue())
		assert.Contains(t, rec["content"].StringValue(), "ns")
	}
}

// TestNSResourceNamesIgnoreInputOrder keeps the resource identities stable
// when a config file reorders the four targets.
func TestNSResourceNamesIgnoreInputOrder(t *testing.T) {
	names := func(entries []Record) []string {
		var out []string
		for k := range run(t, Args{ZoneID: "zone-1", Records: &Records{Zone: "example.com", Entries: entries}}).res {
			out = append(out, k)
		}

		sort.Strings(out)

		return out
	}

	regular := nsRecords().Entries
	reversed := []Record{regular[3], regular[2], regular[1], regular[0]}

	assert.Equal(t, names(regular), names(reversed))
}

func TestValidateNSDelegations(t *testing.T) {
	cases := []struct {
		name string
		args Args
		want string
	}{
		{"four targets ok", withNS(func(*Records) {}), ""},
		{"proxied", withNS(func(r *Records) { r.Entries[0].Proxied = true }), "DNS only"},
		{"waf", withNS(func(r *Records) { yes := true; r.Entries[0].WAF = &yes }), "managed WAF"},
		{"wildcard owner", withNS(func(r *Records) { r.Entries[0].Name = "*.example.com" }), "wildcard"},
		{"apex owner", withNS(func(r *Records) { r.Entries[0].Name = "example.com" }), "apex"},
		{"duplicate target", withNS(func(r *Records) { r.Entries[3] = r.Entries[0] }), "listed twice"},
		{"inconsistent ttl", withNS(func(r *Records) { r.Entries[3].TTL = 600 }), "share a ttl"},
		{"automatic ttl", withNS(func(r *Records) { r.Entries[0].TTL = 0 }), "explicit ttl"},
		{"upper-case name", withNS(func(r *Records) { r.Entries[0].Content = "NS1.nameservers.example" }), "nameserver hostname"},
		{"trailing dot", withNS(func(r *Records) { r.Entries[0].Content = "ns1.nameservers.example." }), "nameserver hostname"},
		{"address as target", withNS(func(r *Records) { r.Entries[0].Content = "192.0.2.1" }), "nameserver hostname"},
		{"beside an address", withNS(func(r *Records) {
			r.Entries = append(r.Entries, Record{Name: "team.example.com", Type: "A", Content: "192.0.2.1"})
		}), "share its name"},
		{"beside a CNAME", withNS(func(r *Records) {
			r.Entries = append(r.Entries, Record{Name: "team.example.com", Type: "CNAME", Content: "origin.example.net"})
		}), "share its name"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.args.Validate()
			if tc.want == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

// TestNSIsNotAWAFHost keeps a DNS-only delegation out of the derived WAF's
// vocabulary even if a caller inspects the record's coverage directly.
func TestNSIsNotAWAFHost(t *testing.T) {
	assert.False(t, nsRecords().Entries[0].WAFCovered())
}
