package zonespec

import (
	"fmt"
	"sort"
	"strings"
)

// CloudflareManagedRulesetID is the id of the Cloudflare Managed Ruleset,
// the same in every zone and account (developers.cloudflare.com/waf/
// managed-rules/reference/cloudflare-managed-ruleset/). ManagedWAF deploys
// it; the OWASP Core Ruleset is a different ruleset and is not deployed.
const CloudflareManagedRulesetID = "efb7b8c949ac4650a09736fc376e9aee"

// DefaultManagedWAFDescription describes the managed ruleset's execute rule
// when ManagedWAF.Description is empty.
const DefaultManagedWAFDescription = "Cloudflare Managed Ruleset"

// HSTS limits. maxHSTSMaxAge is the longest max-age Cloudflare offers (12
// months); preloadMinMaxAge is the shortest the browsers' preload list
// accepts (hstspreload.org, "Submission Requirements").
const (
	maxHSTSMaxAge    = 31536000
	preloadMinMaxAge = 31536000
)

type (
	// HSTS is the zone setting security_header: the Strict-Transport-
	// Security header Cloudflare adds to every HTTPS response of the zone.
	//
	// A browser that has seen the header refuses plain HTTP to the name
	// (and, with IncludeSubdomains, to every name below it) for MaxAge
	// seconds, and gives the user no way past a certificate error. That is
	// the point, and it is also why it is rolled out with a short MaxAge
	// first: a name that cannot serve HTTPS is unreachable for that long.
	//
	// Turning it off again is two steps: keep Enabled with MaxAge 0 (the
	// header then tells every browser to forget the policy) for at least
	// the old MaxAge, and only then set Enabled false.
	HSTS struct {
		// Enabled sends the header. False stores the other fields and
		// sends nothing.
		Enabled bool `json:"enabled" yaml:"enabled"`
		// MaxAge is the header's max-age, in seconds: 0 to 31536000
		// (12 months). 0 with Enabled true is the "forget it" header.
		MaxAge int `json:"maxAge" yaml:"maxAge"`
		// IncludeSubdomains extends the policy to every name below each
		// name that sends it.
		IncludeSubdomains bool `json:"includeSubdomains,omitempty" yaml:"includeSubdomains,omitempty"`
		// Nosniff also sends X-Content-Type-Options: nosniff.
		Nosniff bool `json:"nosniff,omitempty" yaml:"nosniff,omitempty"`
		// Preload adds the preload directive. Refused unless Enabled,
		// IncludeSubdomains, MaxAge of at least 31536000 and
		// Args.AlwaysUseHTTPS true: what the preload list requires. A
		// preloaded name is built into browsers, and leaving the list
		// takes months.
		Preload bool `json:"preload,omitempty" yaml:"preload,omitempty"`
	}

	// ManagedWAF deploys the Cloudflare Managed Ruleset: ONE execute rule
	// in the zone's http_request_firewall_managed entry point ruleset,
	// limited to the listed hosts. The ruleset's own default actions
	// apply; nothing here overrides a rule. The OWASP Core Ruleset is not
	// deployed.
	//
	// The host list is how it is rolled out: a few hosts first, then more,
	// then the zone. An empty list is refused rather than read as "the
	// whole zone"; covering the whole zone is WholeZone, said out loud.
	//
	// A zone that declares this block owns the whole phase: a managed
	// rule deployed in the dashboard is replaced, not merged.
	ManagedWAF struct {
		// Zone is the zone's domain ("example.com"). Required with
		// Hosts: every host must be the zone itself or a name inside it.
		Zone string `json:"zone,omitempty" yaml:"zone,omitempty"`
		// Hosts the ruleset runs on: an exact lower-case name
		// ("app.example.com") or one leading wildcard label
		// ("*.dev.example.com", every name below dev.example.com, at
		// any depth, but not dev.example.com itself).
		Hosts []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
		// WholeZone runs the ruleset on every request to the zone.
		// Exclusive with Hosts.
		WholeZone bool `json:"wholeZone,omitempty" yaml:"wholeZone,omitempty"`
		// ExcludeHosts are the names a whole-zone rule leaves out: an exact
		// lower-case name or one leading wildcard label ("*.dev.example.com",
		// every name below dev.example.com). Only with WholeZone, and then
		// Zone is required: every name must be the zone or inside it. The
		// expression is "not (<exclusions>)"; without any, "true".
		ExcludeHosts []string `json:"excludeHosts,omitempty" yaml:"excludeHosts,omitempty"`
		// Description of the rule. Empty uses "Cloudflare Managed
		// Ruleset".
		Description string `json:"description,omitempty" yaml:"description,omitempty"`
	}
)

// Managed reports whether the block deploys the ruleset: any non-nil
// block does (Validate refuses one that would match nothing).
func (m *ManagedWAF) Managed() bool { return m != nil }

// validate reports the first problem with an HSTS block.
func (h *HSTS) validate(zoneID string, alwaysUseHTTPS *bool) error {
	if h.MaxAge < 0 || h.MaxAge > maxHSTSMaxAge {
		return fmt.Errorf("zone %q: hsts.maxAge %d must be 0 to %d seconds", zoneID, h.MaxAge, maxHSTSMaxAge)
	}

	if h.Preload {
		https := alwaysUseHTTPS != nil && *alwaysUseHTTPS
		if !h.Enabled || !h.IncludeSubdomains || h.MaxAge < preloadMinMaxAge || !https {
			return fmt.Errorf("zone %q: hsts.preload needs enabled, includeSubdomains, maxAge of at least %d and alwaysUseHttps: true",
				zoneID, preloadMinMaxAge)
		}
	}

	return nil
}

// validate reports the first problem with a managed WAF block: a rule
// that would match nothing, or a host it would never meet.
func (m *ManagedWAF) validate(zoneID string) error {
	switch {
	case m.WholeZone && len(m.Hosts) > 0:
		return fmt.Errorf("zone %q: managedWaf: wholeZone and hosts are exclusive", zoneID)
	case m.WholeZone && len(m.ExcludeHosts) == 0:
		return nil
	case !m.WholeZone && len(m.ExcludeHosts) > 0:
		return fmt.Errorf("zone %q: managedWaf: excludeHosts are the names a wholeZone rule leaves out; "+
			"a host list already names what it covers", zoneID)
	case len(m.Hosts) == 0 && !m.WholeZone:
		return fmt.Errorf("zone %q: managedWaf: an empty host list would deploy a rule that matches nothing; "+
			"list hosts, set wholeZone: true, or omit the block", zoneID)
	}

	if m.Zone == "" || m.Zone != strings.ToLower(m.Zone) {
		return fmt.Errorf("zone %q: managedWaf.zone must be the zone's lower-case domain", zoneID)
	}

	if err := validateWAFNames(zoneID, "hosts", m.Zone, m.Hosts); err != nil {
		return err
	}

	if err := validateWAFNames(zoneID, "excludeHosts", m.Zone, m.ExcludeHosts); err != nil {
		return err
	}

	if n := len(ManagedWAFExpression(m)); n > MaxRuleExpression {
		return fmt.Errorf("zone %q: managedWaf: the rule expression is %d characters, over Cloudflare's %d limit; "+
			"fewer hosts, or opt names out of the WAF with a reason", zoneID, n, MaxRuleExpression)
	}

	return nil
}

// validateWAFNames holds a list of names (field is its key in the block) to
// the shape a managed WAF expression accepts: lower-case, inside zone, an
// exact name or one leading wildcard label, listed once.
func validateWAFNames(zoneID, field, zone string, names []string) error {
	seen := map[string]struct{}{}

	for i, host := range names {
		bare := strings.TrimPrefix(host, "*.")

		if host != strings.ToLower(host) || bare == "" || strings.ContainsAny(bare, `*/:"\ `) || strings.HasSuffix(host, ".") ||
			(bare != zone && !strings.HasSuffix(bare, "."+zone)) {
			return fmt.Errorf("zone %q: managedWaf.%s[%d] %q must be a lower-case hostname inside %s (a leading \"*.\" for a wildcard)",
				zoneID, field, i, host, zone)
		}

		if _, dup := seen[host]; dup {
			return fmt.Errorf("zone %q: managedWaf.%s[%d] %q is listed twice", zoneID, field, i, host)
		}

		seen[host] = struct{}{}
	}

	return nil
}

// MaxRuleExpression is Cloudflare's limit on a ruleset rule's expression.
const MaxRuleExpression = 4096

// ManagedWAFExpression is the filter the managed ruleset's execute rule
// runs on: "true" for the whole zone, "not (<exclusions>)" for the whole zone
// less its excluded names, else the hosts, sorted so authoring order never
// changes the plan.
func ManagedWAFExpression(m *ManagedWAF) string {
	if m.WholeZone {
		if len(m.ExcludeHosts) == 0 {
			return "true"
		}

		excluded := append([]string(nil), m.ExcludeHosts...)
		sort.Strings(excluded)

		return "not (" + HostExpression(excluded) + ")"
	}

	hosts := append([]string(nil), m.Hosts...)
	sort.Strings(hosts)

	return HostExpression(hosts)
}
