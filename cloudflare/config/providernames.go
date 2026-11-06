package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
)

// ProviderNamesVersion is the schema version of the published provider names
// (ProviderNames.Version).
const ProviderNamesVersion = 1

type (
	// ProviderNames is a published fact: the hostnames the platform itself
	// writes, per zone, so that a company's program can refuse a row of its
	// own on one of them without holding the platform's catalog.
	//
	// It is versioned JSON. Version is the schema (ProviderNamesVersion);
	// Revision counts the platform's publications, at least 1, and is
	// quoted in the refusal so a reader can tell which publication a
	// refusal came from. The platform derives it (Config.ProviderNamesFor
	// plus the names no catalog holds, such as a status page's); the company
	// side only reads it (ParseProviderNames, Config.WithProviderNames). A
	// committed copy is never read back as the source of truth: the
	// published input is.
	ProviderNames struct {
		Version  int                     `json:"version"`
		Revision int                     `json:"revision"`
		Zones    map[string]ProviderZone `json:"zones"`
	}

	// ProviderZone is the provider-owned names of one zone: exact,
	// lower-case hostnames inside it. A wildcard is not a name here.
	ProviderZone struct {
		// Hosts is any other exact name the platform serves or writes.
		Hosts []string `json:"hosts,omitempty"`
		// Status is the CNAMEs of the platform's status pages, which no
		// catalog holds.
		Status []string `json:"status,omitempty"`
		// Tunnel is the CNAMEs the platform's tunnels write
		// (Config.ProviderNamesFor derives them).
		Tunnel []string `json:"tunnel,omitempty"`
	}
)

// Names is every provider-owned name of the zone, sorted and de-duplicated.
func (z ProviderZone) Names() []string {
	var out []string

	out = append(out, z.Hosts...)
	out = append(out, z.Status...)
	out = append(out, z.Tunnel...)

	slices.Sort(out)

	return slices.Compact(out)
}

// ParseProviderNames decodes and validates the published artifact: strict
// JSON (an unknown key fails), a known version, a revision, and in every zone
// only exact lower-case names inside that zone. The result has its lists
// sorted and de-duplicated.
func ParseProviderNames(data []byte) (*ProviderNames, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, errors.New("provider names: the input is empty")
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()

	var p ProviderNames
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("provider names: %w", err)
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("provider names: trailing data after the document")
	}

	if err := p.normalize(); err != nil {
		return nil, err
	}

	return &p, nil
}

// Marshal is the artifact as indented JSON with a trailing newline, lists
// sorted and de-duplicated, so the same facts are the same bytes.
func (p *ProviderNames) Marshal() ([]byte, error) {
	cp := ProviderNames{Version: p.Version, Revision: p.Revision, Zones: map[string]ProviderZone{}}
	for k, v := range p.Zones {
		cp.Zones[k] = v
	}

	if err := cp.normalize(); err != nil {
		return nil, err
	}

	out, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("provider names: %w", err)
	}

	return append(out, '\n'), nil
}

// normalize validates the artifact and sorts and de-duplicates its lists.
func (p *ProviderNames) normalize() error {
	if p.Version != ProviderNamesVersion {
		return fmt.Errorf("provider names: version %d is not supported (this library reads version %d)", p.Version, ProviderNamesVersion)
	}

	if p.Revision < 1 {
		return fmt.Errorf("provider names: revision must be at least 1, got %d", p.Revision)
	}

	for _, zone := range sortedKeys(p.Zones) {
		if zone == "" || zone != strings.ToLower(zone) || strings.ContainsAny(zone, `*/:" `) {
			return fmt.Errorf("provider names: zones[%q]: the key must be a lower-case zone name", zone)
		}

		z := p.Zones[zone]

		for list, hosts := range map[string]*[]string{"hosts": &z.Hosts, "status": &z.Status, "tunnel": &z.Tunnel} {
			for _, h := range *hosts {
				if err := checkProviderHost(zone, h); err != nil {
					return fmt.Errorf("provider names: zones[%s].%s: %w", zone, list, err)
				}
			}

			slices.Sort(*hosts)
			*hosts = slices.Compact(*hosts)
		}

		p.Zones[zone] = z
	}

	return nil
}

func checkProviderHost(zone, host string) error {
	switch {
	case host != strings.ToLower(host):
		return fmt.Errorf("%q must be lower-case", host)
	case host == "" || strings.ContainsAny(host, `*/:" `):
		return fmt.Errorf("%q must be an exact hostname (no wildcard, scheme, path or port)", host)
	case host != zone && !strings.HasSuffix(host, "."+zone):
		return fmt.Errorf("%q is not inside zone %s", host, zone)
	}

	return nil
}

// WithProviderNames records the platform's published names and holds the
// registry to them: in a zone owned by a company (not the platform, and not
// unowned), a records row, an origin_cnames key, or a redirect source or
// target that is a provider-owned name is refused, quoting the revision. The
// platform's own zones and zones with no owner are not checked: the names are
// the platform's to write there.
//
// Load needs the platform's catalog and refuses to run without one; this is
// how a company, which has no catalog of the platform's, learns the same
// names. A config that carries names re-checks them on every Validate. A nil
// artifact is refused: absence of the input must not read as "nothing is
// provider-owned".
func (c *Config) WithProviderNames(p *ProviderNames) error {
	if p == nil {
		return errors.New("provider names: no artifact given")
	}

	cp := ProviderNames{Version: p.Version, Revision: p.Revision, Zones: map[string]ProviderZone{}}
	for k, v := range p.Zones {
		cp.Zones[k] = v
	}

	if err := cp.normalize(); err != nil {
		return err
	}

	prev := c.providerNames
	c.providerNames = &cp

	if err := c.validateProviderNames(); err != nil {
		c.providerNames = prev

		return err
	}

	return nil
}

// ProviderNamesFor lists the exact names of zone that the registry's tunnels
// write the CNAME of, sorted: the part of the platform's published names the
// catalog derives. A wildcard host and a dns_elsewhere host (someone else
// writes its record) are left out. Status pages and any other name are the
// platform's to add (ProviderZone.Status, ProviderZone.Hosts).
func (c *Config) ProviderNamesFor(zone string) ProviderZone {
	var out ProviderZone

	for i := range c.Tunnels {
		t := &c.Tunnels[i]

		for _, host := range t.Hosts {
			if strings.Contains(host, "*") || !t.OwnsRecord(zone, host) {
				continue
			}

			out.Tunnel = append(out.Tunnel, host)
		}
	}

	slices.Sort(out.Tunnel)
	out.Tunnel = slices.Compact(out.Tunnel)

	return out
}

// validateProviderNames refuses, in a company's zone, a row on a name the
// platform published as its own. It does nothing until WithProviderNames.
func (c *Config) validateProviderNames() error {
	if c.providerNames == nil {
		return nil
	}

	for _, zone := range sortedKeys(c.Zones) {
		z := c.Zones[zone]
		if z.Owner == "" || z.Owner == OwnerPlatform {
			continue
		}

		names := c.providerNames.Zones[zone].Names()
		if len(names) == 0 {
			continue
		}

		refuse := func(where, host string) error {
			if !slices.Contains(names, strings.ToLower(host)) {
				return nil
			}

			return fmt.Errorf("%s: zones[%s].%s: %s is provider-owned (published input rev %d); the platform writes it",
				FileName, zone, where, host, c.providerNames.Revision)
		}

		for _, r := range z.Records {
			if err := refuse("records["+r.Name+"]", r.Name); err != nil {
				return err
			}
		}

		for _, host := range sortedKeys(z.OriginCNAMEs) {
			if err := refuse("origin_cnames["+host+"]", host); err != nil {
				return err
			}
		}

		for _, r := range z.Redirects {
			if err := refuse("redirects["+r.Ref+"].from", r.From); err != nil {
				return err
			}

			if u, err := url.Parse(r.To); err == nil && u.Hostname() != "" {
				if err := refuse("redirects["+r.Ref+"].to", u.Hostname()); err != nil {
					return err
				}
			}
		}
	}

	return nil
}
