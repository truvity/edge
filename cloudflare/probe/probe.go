// Package probe is a read-only check of a zone against what it declares: the
// TLS floor (SSL mode, minimum TLS version, Total TLS) and the provider-owned
// names (each must resolve to the record the platform writes).
//
// It exists because a zone's settings live in Cloudflare, where anyone with
// the dashboard can change them, and a program that only applies what it
// declares never learns that a floor was lowered by hand. The probe reads the
// live settings and reports the difference; it never writes, and so it needs
// only read rights. A caller that wants the floor back re-applies the stack
// that declares it.
//
//	d, _ := probe.DeclaredFrom(cfg, "example.com")
//	d.Names = probe.NamesFrom(providerHosts, targets)
//	report, err := probe.NewClient(token).Check(ctx, d)
//	if err == nil && !report.OK() { /* report.Drift */ }
//
// What the declaration leaves empty is not checked: an unset SSL mode, an
// unset minimum version, a nil TotalTLS and an empty Names are no request at
// all. An answer the API refuses (a token without the right, a missing zone)
// is an error, never drift: a probe that cannot read has not found the zone
// in order.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/internal/cfapi"
)

type (
	// Client reads a zone with one token. It issues GET requests only.
	Client struct {
		API cfapi.Client
	}

	// TotalTLS is the declared Total TLS setting.
	TotalTLS struct {
		Enabled bool
		// CertificateAuthority is compared when set: "lets_encrypt",
		// "google" and so on, as the registry writes it.
		CertificateAuthority string
	}

	// Name is a provider name and the record it must have.
	Name struct {
		Host string
		// Type is the record type; empty is CNAME.
		Type string
		// Target is the record's content. Empty: the record must exist,
		// whatever it points at.
		Target string
	}

	// Declared is what a zone is declared to have. Zone is the zone's name
	// (for the report), ZoneID what the API is asked about.
	Declared struct {
		Zone   string
		ZoneID string
		// SSLMode is "off", "flexible", "full" or "strict"; empty is not checked.
		SSLMode string
		// MinTLSVersion is "1.0" to "1.3"; empty is not checked.
		MinTLSVersion string
		// TotalTLS nil is not checked.
		TotalTLS *TotalTLS
		Names    []Name
	}

	// Finding is one difference between what is declared and what is live.
	Finding struct {
		// Check is "ssl", "min_tls_version", "total_tls" or "name".
		Check string
		// Subject is the name for a "name" finding, else empty.
		Subject string
		Want    string
		Got     string
	}

	// Report is the result of one Check.
	Report struct {
		Zone string
		// Checked is how many declared things were compared.
		Checked int
		Drift   []Finding
	}
)

// NewClient is a Client with the given token against the public API.
func NewClient(token string) *Client { return &Client{API: cfapi.Client{Token: token}} }

// OK reports whether everything checked matched.
func (r Report) OK() bool { return len(r.Drift) == 0 }

// String is one line per finding, for a log or a failing job.
func (f Finding) String() string {
	subject := f.Check
	if f.Subject != "" {
		subject += " " + f.Subject
	}

	return fmt.Sprintf("%s: want %s, got %s", subject, f.Want, f.Got)
}

// DeclaredFrom reads a zone's TLS floor from the registry: its SSL mode,
// minimum TLS version and Total TLS, each only when the registry sets it. The
// zone must be a known, fully managed zone with an id (not a security.txt-only
// zone, not zone_id_pending). Names are not part of the registry; the caller
// sets them (NamesFrom).
func DeclaredFrom(cfg *config.Config, zone string) (Declared, error) {
	z, ok := cfg.Zones[zone]

	switch {
	case !ok:
		return Declared{}, fmt.Errorf("probe: no zone %q in the registry", zone)
	case z.SecurityTxtOnly():
		return Declared{}, fmt.Errorf("probe: zone %q is managed for its security.txt only; it declares no TLS floor", zone)
	case z.ZoneIDPending || z.ZoneID == "":
		return Declared{}, fmt.Errorf("probe: zone %q has no zone id yet (zone_id_pending)", zone)
	}

	d := Declared{Zone: zone, ZoneID: z.ZoneID, SSLMode: z.SSLMode, MinTLSVersion: z.MinTLSVersion}

	if z.TotalTLS != nil {
		d.TotalTLS = &TotalTLS{Enabled: z.TotalTLS.Enabled, CertificateAuthority: z.TotalTLS.CertificateAuthority}
	}

	return d, nil
}

// NamesFrom turns provider-owned hostnames into the records to check, sorted
// and listed once, as CNAMEs. targets gives the content a host must have;
// a host without an entry only has to exist.
func NamesFrom(hosts []string, targets map[string]string) []Name {
	sorted := slices.Clone(hosts)
	slices.Sort(sorted)

	sorted = slices.Compact(sorted)

	out := make([]Name, 0, len(sorted))
	for _, h := range sorted {
		out = append(out, Name{Host: h, Type: "CNAME", Target: targets[h]})
	}

	return out
}

// Check reads the live settings and records the declaration covers and
// reports every difference. It only reads. An API error is returned as an
// error, with the token redacted, and no report.
func (c *Client) Check(ctx context.Context, d Declared) (Report, error) {
	r := Report{Zone: d.Zone}

	if d.SSLMode == "" && d.MinTLSVersion == "" && d.TotalTLS == nil && len(d.Names) == 0 {
		return r, nil
	}

	if d.ZoneID == "" {
		return Report{}, errors.New("probe: the declaration has no zone id to ask about")
	}

	zonePath := "/zones/" + url.PathEscape(d.ZoneID)

	for _, s := range []struct{ check, want string }{{"ssl", d.SSLMode}, {"min_tls_version", d.MinTLSVersion}} {
		if s.want == "" {
			continue
		}

		got, err := c.setting(ctx, zonePath, s.check)
		if err != nil {
			return Report{}, err
		}

		r.Checked++

		if got != s.want {
			r.Drift = append(r.Drift, Finding{Check: s.check, Want: s.want, Got: got})
		}
	}

	if d.TotalTLS != nil {
		f, err := c.totalTLS(ctx, zonePath, *d.TotalTLS)
		if err != nil {
			return Report{}, err
		}

		r.Checked++

		if f != nil {
			r.Drift = append(r.Drift, *f)
		}
	}

	for _, n := range d.Names {
		f, err := c.name(ctx, zonePath, n)
		if err != nil {
			return Report{}, err
		}

		r.Checked++

		if f != nil {
			r.Drift = append(r.Drift, *f)
		}
	}

	return r, nil
}

// get is the probe's one way to talk to the API: a GET.
func (c *Client) get(ctx context.Context, path string, query url.Values) (json.RawMessage, error) {
	env, err := c.API.Do(ctx, http.MethodGet, path, query, nil)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}

	return env.Result, nil
}

// setting is the value of a zone setting.
func (c *Client) setting(ctx context.Context, zonePath, id string) (string, error) {
	raw, err := c.get(ctx, zonePath+"/settings/"+id, nil)
	if err != nil {
		return "", err
	}

	var s struct {
		Value string `json:"value"`
	}

	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("probe: setting %s: unexpected answer", id)
	}

	return s.Value, nil
}

func (c *Client) totalTLS(ctx context.Context, zonePath string, want TotalTLS) (*Finding, error) {
	raw, err := c.get(ctx, zonePath+"/acm/total_tls", nil)
	if err != nil {
		return nil, err
	}

	var got struct {
		Enabled              bool   `json:"enabled"`
		CertificateAuthority string `json:"certificate_authority"`
	}

	if err := json.Unmarshal(raw, &got); err != nil {
		return nil, errors.New("probe: total_tls: unexpected answer")
	}

	state := func(on bool) string {
		if on {
			return "enabled"
		}

		return "disabled"
	}

	switch {
	case got.Enabled != want.Enabled:
		return &Finding{Check: "total_tls", Want: state(want.Enabled), Got: state(got.Enabled)}, nil
	case want.Enabled && want.CertificateAuthority != "" && got.CertificateAuthority != want.CertificateAuthority:
		return &Finding{Check: "total_tls", Want: want.CertificateAuthority, Got: got.CertificateAuthority}, nil
	}

	return nil, nil
}

func (c *Client) name(ctx context.Context, zonePath string, n Name) (*Finding, error) {
	typ := n.Type
	if typ == "" {
		typ = "CNAME"
	}

	q := url.Values{"name": {n.Host}, "type": {typ}}

	raw, err := c.get(ctx, zonePath+"/dns_records", q)
	if err != nil {
		return nil, err
	}

	var records []struct {
		Content string `json:"content"`
	}

	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("probe: dns_records %s: unexpected answer", n.Host)
	}

	want := typ
	if n.Target != "" {
		want += " " + n.Target
	}

	if len(records) == 0 {
		return &Finding{Check: "name", Subject: n.Host, Want: want, Got: "no record"}, nil
	}

	if n.Target == "" {
		return nil, nil
	}

	var got []string

	for _, rec := range records {
		if normalize(rec.Content) == normalize(n.Target) {
			return nil, nil
		}

		got = append(got, rec.Content)
	}

	return &Finding{Check: "name", Subject: n.Host, Want: want, Got: strings.Join(got, ", ")}, nil
}

// normalize compares record content without case or a trailing dot.
func normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}
