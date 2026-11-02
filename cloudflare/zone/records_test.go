package zone

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dnsRecordType = "cloudflare:index/dnsRecord:DnsRecord"

// albRecords is the shape an estate moving a load balancer's names into a
// zone declares: proxied CNAMEs, one of them a wildcard two labels deep,
// beside a DNS-only CNAME to a CDN.
func albRecords() *Records {
	return &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "api.example.com", Type: "CNAME", Content: "lb-1.eu-west-1.elb.amazonaws.com", Proxied: true},
			{Name: "*.api.example.com", Type: "CNAME", Content: "lb-1.eu-west-1.elb.amazonaws.com", Proxied: true},
			{Name: "docs.example.com", Type: "CNAME", Content: "d111.cloudfront.net", TTL: 300, Comment: "CloudFront"},
		},
	}
}

func withRecords(mut func(r *Records)) Args {
	r := albRecords()
	mut(r)

	return Args{ZoneID: "z", Records: r}
}

func TestRecordChildNames(t *testing.T) {
	m := run(t, Args{ZoneID: "zone-1", Records: &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "*.api.example.com", Type: "CNAME", Content: "lb.example.net", Proxied: true},
			{Name: "example.com", Type: "A", Content: "192.0.2.1"},
			{Name: "example.com", Type: "A", Content: "192.0.2.2"},
			{Name: "v6.example.com", Type: "AAAA", Content: "2001:db8::1"},
		},
	}})

	for _, want := range []string{
		"record-example-star-api-example-com-cname",
		"record-example-example-com-a-192-0-2-1",
		"record-example-example-com-a-192-0-2-2",
		"record-example-v6-example-com-aaaa-2001-db8--1",
	} {
		assert.Contains(t, m.res, dnsRecordType+"/"+want, "child name is a contract consumers alias onto")
	}
}

func TestRecordInputs(t *testing.T) {
	m := run(t, Args{ZoneID: "zone-1", Records: albRecords()})

	api := m.res[dnsRecordType+"/record-example-api-example-com-cname"]
	require.NotNil(t, api)
	assert.Equal(t, "zone-1", api["zoneId"].StringValue())
	assert.Equal(t, "api.example.com", api["name"].StringValue())
	assert.Equal(t, "CNAME", api["type"].StringValue())
	assert.Equal(t, "lb-1.eu-west-1.elb.amazonaws.com", api["content"].StringValue())
	assert.True(t, api["proxied"].BoolValue())
	assert.Equal(t, 1.0, api["ttl"].NumberValue(), "a proxied record's TTL is automatic")
	assert.False(t, api.HasValue("comment"), "no comment declared, none sent")

	docs := m.res[dnsRecordType+"/record-example-docs-example-com-cname"]
	require.NotNil(t, docs)
	assert.False(t, docs["proxied"].BoolValue())
	assert.Equal(t, 300.0, docs["ttl"].NumberValue())
	assert.Equal(t, "CloudFront", docs["comment"].StringValue())
}

func TestUnsetTTLIsAutomatic(t *testing.T) {
	m := run(t, withRecords(func(r *Records) { r.Entries[2].TTL = 0 }))

	assert.Equal(t, 1.0, m.res[dnsRecordType+"/record-example-docs-example-com-cname"]["ttl"].NumberValue())
}

func TestNoRecordsManagesNoRecord(t *testing.T) {
	m := run(t, Args{ZoneID: "zone-1", SSL: "strict", Records: &Records{Zone: "example.com"}})

	for k := range m.res {
		assert.NotContains(t, k, dnsRecordType, "an empty records block touches no record")
	}
}

func TestValidateRecords(t *testing.T) {
	cases := []struct {
		name string
		args Args
		want string
	}{
		{"records ok", Args{ZoneID: "z", Records: albRecords()}, ""},
		{"empty records manage nothing", Args{ZoneID: "z", Records: &Records{Zone: "example.com"}}, "nothing to apply"},
		{"no zone domain", withRecords(func(r *Records) { r.Zone = "" }), "records.zone"},
		{"name outside the zone", withRecords(func(r *Records) { r.Entries[0].Name = "api.example.org" }), "inside example.com"},
		{"suffix is not a parent", withRecords(func(r *Records) { r.Entries[0].Name = "api.notexample.com" }), "inside example.com"},
		{"upper-case name", withRecords(func(r *Records) { r.Entries[0].Name = "API.example.com" }), "lower-case hostname"},
		{"trailing dot", withRecords(func(r *Records) { r.Entries[0].Name = "api.example.com." }), "lower-case hostname"},
		{"wildcard in the middle", withRecords(func(r *Records) { r.Entries[0].Name = "api.*.example.com" }), "lower-case hostname"},
		{"apex wildcard ok", withRecords(func(r *Records) { r.Entries[0].Name = "*.example.com" }), ""},
		{"apex ok", withRecords(func(r *Records) { r.Entries[0].Name = "example.com" }), ""},
		{"unsupported type", withRecords(func(r *Records) { r.Entries[2].Type = "MX" }), "type must be one of"},
		{"CNAME with a trailing dot", withRecords(func(r *Records) { r.Entries[2].Content = "d111.cloudfront.net." }), "without a trailing dot"},
		{"CNAME that is a URL", withRecords(func(r *Records) { r.Entries[2].Content = "https://d111.cloudfront.net" }), "without a trailing dot"},
		{"A with an IPv6 address", withRecords(func(r *Records) {
			r.Entries[2] = Record{Name: "v4.example.com", Type: "A", Content: "2001:db8::1"}
		}), "not an IPv4 address"},
		{"AAAA with an IPv4 address", withRecords(func(r *Records) {
			r.Entries[2] = Record{Name: "v6.example.com", Type: "AAAA", Content: "192.0.2.1"}
		}), "not an IPv6 address"},
		{"proxied with a TTL", withRecords(func(r *Records) { r.Entries[0].TTL = 300 }), "leave ttl at 0"},
		{"TTL too short", withRecords(func(r *Records) { r.Entries[2].TTL = 30 }), "ttl 30 must be"},
		{"TTL too long", withRecords(func(r *Records) { r.Entries[2].TTL = 172800 }), "ttl 172800 must be"},
		{"listed twice", withRecords(func(r *Records) { r.Entries = append(r.Entries, r.Entries[0]) }), "listed twice"},
		{"CNAME beside an A", withRecords(func(r *Records) {
			r.Entries = append(r.Entries, Record{Name: "api.example.com", Type: "A", Content: "192.0.2.1"})
		}), "a CNAME cannot share its name"},
		{"A before a CNAME", withRecords(func(r *Records) {
			r.Entries = append([]Record{{Name: "api.example.com", Type: "A", Content: "192.0.2.1"}}, r.Entries...)
		}), "a CNAME cannot share its name"},
		{"two A records on one name ok", withRecords(func(r *Records) {
			r.Entries = append(r.Entries,
				Record{Name: "ip.example.com", Type: "A", Content: "192.0.2.1"},
				Record{Name: "ip.example.com", Type: "A", Content: "192.0.2.2"})
		}), ""},
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

// TestOnlyListedRecordsAreRegistered proves the inverse of unlisted-record
// survival: the zone component registers a record for each listed row and
// nothing else, so a record the file does not name is not in the program and
// cannot be changed by the zone stack.
func TestOnlyListedRecordsAreRegistered(t *testing.T) {
	m := run(t, Args{ZoneID: "zone-1", Records: &Records{
		Zone: "example.com",
		Entries: []Record{
			{Name: "listed.example.com", Type: "CNAME", Content: "origin.example.net", TTL: 300},
		},
	}})

	var got []string
	for key := range m.res {
		if strings.HasPrefix(key, dnsRecordType+"/") {
			got = append(got, key)
		}
	}

	assert.Equal(t, []string{dnsRecordType + "/record-example-listed-example-com-cname"}, got)
}
