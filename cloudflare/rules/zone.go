package rules

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/truvity/edge/v4/cloudflare/zonespec"
)

// validationZone stands in for the zone id in zonespec's messages: the rules
// package knows no zone.
const validationZone = "rules"

// RateLimits is the zone's whole http_ratelimit ruleset, one rule per row in
// the order given, checked against the plan by zonespec's validation (rule
// count, periods, mitigation timeouts, characteristics, actions). An error
// is zonespec's, unchanged.
func RateLimits(plan string, rows []zonespec.RateLimitRule) (Contribution, error) {
	limits := &zonespec.RateLimits{Plan: plan, Rules: rows}
	if err := (&zonespec.Args{ZoneID: validationZone, RateLimits: limits}).Validate(); err != nil {
		return Contribution{}, err
	}

	c := Contribution{Source: "rate-limits", Phase: PhaseRateLimit, Order: OrderHostsCache}
	byRef := map[string]string{}

	for i := range rows {
		r := &rows[i]

		slug := refSlug(r.Name)
		if slug == "" {
			return Contribution{}, fmt.Errorf("rules: rate limit %q has no letter or digit to name its rule by", r.Name)
		}

		ref := "ratelimit-" + slug
		if prev, dup := byRef[ref]; dup {
			return Contribution{}, fmt.Errorf("rules: rate limits %q and %q both make the rule ref %q: rename one", prev, r.Name, ref)
		}

		byRef[ref] = r.Name

		c.Rules = append(c.Rules, Rule{
			Ref:         ref,
			Description: r.Name,
			Action:      r.Action,
			Expression:  r.MatchExpression(),
			RateLimit: &RateLimit{
				Characteristics:    r.CharacteristicList(),
				Period:             r.Period,
				Requests:           r.Requests,
				MitigationTimeout:  r.MitigationTimeout,
				CountingExpression: r.CountingExpression,
			},
		})
	}

	return c, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// refSlug is a name as a rule ref fragment: lowercase, every run of other
// characters one "-", trimmed of "-".
func refSlug(name string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

// TrustedClients is the zone's one skip rule for known client address ranges
// on named hosts. It skips the bot and rate-limit checks (zonespec's
// DefaultTrustedSkip) unless t.Skip says otherwise; the managed rules and the
// rest of the custom rules still apply by default. An unmanaged block (nil, or no hosts and no ranges) is a
// contribution with no rules.
func TrustedClients(t *zonespec.TrustedClients) (Contribution, error) {
	c := Contribution{Source: "trusted-clients", Phase: PhaseFirewallCustom, Order: OrderHostsCache}
	if !t.Managed() {
		return c, nil
	}

	if err := (&zonespec.Args{ZoneID: validationZone, TrustedClients: t}).Validate(); err != nil {
		return Contribution{}, err
	}

	description := t.Description
	if description == "" {
		description = zonespec.DefaultTrustedDescription
		if len(t.Skip) > 0 {
			description = zonespec.LegacyTrustedDescription
		}
	}

	ruleset, phases, products, err := zonespec.TrustedSkipSplit(t.Skip)
	if err != nil {
		return Contribution{}, err
	}

	c.Rules = []Rule{{
		Ref:         "trusted-clients",
		Description: description,
		Action:      ActionSkip,
		Expression:  zonespec.TrustedExpression(t),
		Params: Params{Skip: &SkipParams{
			Ruleset:  ruleset,
			Phases:   phases,
			Products: products,
		}},
		Logging: true,
	}}

	return c, nil
}

// Redirects is the zone's canonical-host Single Redirects: one rule per
// declared source host in the http_request_dynamic_redirect phase. Each
// rule matches the exact host on HTTP and HTTPS and every path, and answers
// with the HTTPS destination plus the request's path; the query string is
// kept unless PreserveQueryString is false. Entries are sorted by source
// host, then ref, so the plan never depends on authoring order. An unmanaged
// block (nil or no entries) is a contribution with no rules.
func Redirects(r *zonespec.Redirects) (Contribution, error) {
	c := Contribution{Source: "redirects", Phase: PhaseDynamicRedirect, Order: OrderHostsCache}
	if !r.Managed() {
		return c, nil
	}

	if err := (&zonespec.Args{ZoneID: validationZone, Redirects: r}).Validate(); err != nil {
		return Contribution{}, err
	}

	entries := append([]zonespec.Redirect(nil), r.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].From != entries[j].From {
			return entries[i].From < entries[j].From
		}

		return entries[i].Ref < entries[j].Ref
	})

	for i := range entries {
		e := &entries[i]

		status := e.Status
		if status == 0 {
			status = 301
		}

		preserve := true
		if e.PreserveQueryString != nil {
			preserve = *e.PreserveQueryString
		}

		c.Rules = append(c.Rules, Rule{
			Ref:         e.Ref,
			Description: fmt.Sprintf("%s: redirect to %s", e.From, e.To),
			Action:      ActionRedirect,
			Expression:  fmt.Sprintf(`http.host eq "%s"`, e.From),
			Disabled:    e.Disabled,
			Params: Params{Redirect: &RedirectParams{
				TargetURL:           fmt.Sprintf("concat(%q, http.request.uri.path)", e.To),
				StatusCode:          status,
				PreserveQueryString: preserve,
			}},
		})
	}

	return c, nil
}

// ManagedWAF is the one rule that executes the Cloudflare Managed Ruleset,
// with its default actions, on the declared hosts. Nil is a contribution with
// no rules.
func ManagedWAF(m *zonespec.ManagedWAF) (Contribution, error) {
	c := Contribution{Source: "managed-waf", Phase: PhaseFirewallManaged, Order: OrderHostsCache}
	if !m.Managed() {
		return c, nil
	}

	if err := (&zonespec.Args{ZoneID: validationZone, ManagedWAF: m}).Validate(); err != nil {
		return Contribution{}, err
	}

	description := m.Description
	if description == "" {
		description = zonespec.DefaultManagedWAFDescription
	}

	c.Rules = []Rule{{
		Ref:         "managed-waf",
		Description: description,
		Action:      ActionExecute,
		Expression:  zonespec.ManagedWAFExpression(m),
		Params:      Params{Execute: &ExecuteParams{ID: zonespec.CloudflareManagedRulesetID}},
	}}

	return c, nil
}

// SSLMode sets the SSL mode (off, flexible, full or strict) for one host with
// a set_config rule, for a host whose origin differs from the zone's setting.
// ref names the rule and must be unique in the phase.
//
// Authenticated Origin Pulls has no counterpart here: it is a zone or
// hostname certificate setting, not a ruleset rule.
func SSLMode(host, mode, ref string) (Contribution, error) {
	if !hostPattern.MatchString(host) {
		return Contribution{}, fmt.Errorf("rules: ssl mode: host %q is not a domain name", host)
	}

	if ref == "" {
		return Contribution{}, fmt.Errorf("rules: ssl mode: ref is required")
	}

	if err := (&zonespec.Args{ZoneID: validationZone, SSL: mode}).Validate(); err != nil || mode == "" {
		return Contribution{}, fmt.Errorf("rules: ssl mode %q must be one of off, flexible, full, strict", mode)
	}

	return Contribution{
		Source: "ssl-mode:" + ref, Phase: PhaseConfigSettings, Order: OrderHostsCache,
		Rules: []Rule{{
			Ref:         ref,
			Description: fmt.Sprintf("%s: SSL mode %s", host, mode),
			Action:      ActionSetConfig,
			Expression:  fmt.Sprintf(`http.host eq "%s"`, host),
			Params:      Params{Config: &ConfigParams{SSL: mode}},
		}},
	}, nil
}
