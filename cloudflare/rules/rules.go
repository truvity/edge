package rules

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/truvity/edge/v4/cloudflare/rulespec"
)

// The rule shapes live in cloudflare/rulespec, which zonespec can import;
// they are aliased here, so rules.Rule and rulespec.Rule are one type.
type (
	// Phase is rulespec.Phase.
	Phase = rulespec.Phase
	// TTL is rulespec.TTL.
	TTL = rulespec.TTL
	// CacheParams is rulespec.CacheParams.
	CacheParams = rulespec.CacheParams
	// ConfigParams is rulespec.ConfigParams.
	ConfigParams = rulespec.ConfigParams
	// SkipParams is rulespec.SkipParams.
	SkipParams = rulespec.SkipParams
	// ExecuteParams is rulespec.ExecuteParams.
	ExecuteParams = rulespec.ExecuteParams
	// RedirectParams is rulespec.RedirectParams.
	RedirectParams = rulespec.RedirectParams
	// Params is rulespec.Params.
	Params = rulespec.Params
	// RateLimit is rulespec.RateLimit.
	RateLimit = rulespec.RateLimit
	// Rule is rulespec.Rule.
	Rule = rulespec.Rule
	// Contribution is rulespec.Contribution.
	Contribution = rulespec.Contribution
)

// The phases this package assembles.
const (
	PhaseCacheSettings   Phase = "http_request_cache_settings"
	PhaseConfigSettings  Phase = "http_config_settings"
	PhaseDynamicRedirect Phase = "http_request_dynamic_redirect"
	PhaseFirewallCustom  Phase = "http_request_firewall_custom"
	PhaseFirewallManaged Phase = "http_request_firewall_managed"
	PhaseRateLimit       Phase = "http_ratelimit"
)

// Phases lists every phase, in a fixed order.
func Phases() []Phase {
	return []Phase{
		PhaseCacheSettings, PhaseConfigSettings, PhaseDynamicRedirect,
		PhaseFirewallCustom, PhaseFirewallManaged, PhaseRateLimit,
	}
}

// Conventional Orders in the cache phase.
const (
	// OrderHostsCache places the zone's cacheable-hosts rule.
	OrderHostsCache = 100
	// OrderBypass is where Assemble puts the catch-all bypass rule.
	OrderBypass = 500
	// OrderPathCaches places rules that cache part of a host that
	// the hosts rule does not.
	OrderPathCaches = 900
)

// BypassRef and BypassSource identify the rule Assemble synthesizes in the
// cache phase. No contribution may use that Ref.
const (
	BypassRef         = "cache-bypass"
	BypassSource      = "assembler"
	BypassDescription = "Not a cacheable hostname"
)

// Rule actions the builders use.
const (
	ActionSetCacheSettings = "set_cache_settings"
	ActionSetConfig        = "set_config"
	ActionSkip             = "skip"
	ActionExecute          = "execute"
	ActionRedirect         = "redirect"
)

// TTL modes of Cloudflare cache settings.
const (
	TTLOverrideOrigin  = "override_origin"
	TTLRespectOrigin   = "respect_origin"
	TTLBypassByDefault = "bypass_by_default"
)

// Assemble merges contributions into one ordered rule list per phase. It
// sorts by (Order, Source), rejects conflicting writers, and in the cache
// phase synthesizes the catch-all bypass rule at OrderBypass. A phase with no
// contribution is absent from the result; an absent phase is one the caller
// does not manage. The input is not modified.
func Assemble(c []Contribution) (map[Phase][]Rule, error) {
	sorted := append([]Contribution(nil), c...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Order != sorted[j].Order {
			return sorted[i].Order < sorted[j].Order
		}

		return sorted[i].Source < sorted[j].Source
	})

	var errs []error

	refs := map[Phase]map[string]string{}
	claims := []string{}
	cacheSeen := false
	byPhase := map[Phase][]Contribution{}

	for i := range sorted {
		con := &sorted[i]
		at := fmt.Sprintf("rules: contribution %q", con.Source)

		if !knownPhase(con.Phase) {
			errs = append(errs, fmt.Errorf("%s: unknown phase %q", at, con.Phase))

			continue
		}

		if con.Source == "" {
			errs = append(errs, fmt.Errorf("rules: a %s contribution has no source", con.Phase))
		}

		if con.Claim != "" && con.Phase != PhaseCacheSettings {
			errs = append(errs, fmt.Errorf("%s: a claim belongs to the %s phase, not %s", at, PhaseCacheSettings, con.Phase))
		}

		if con.Phase == PhaseCacheSettings {
			cacheSeen = true

			if con.Claim != "" {
				if !strings.HasPrefix(con.Claim, "(") || !strings.HasSuffix(con.Claim, ")") {
					errs = append(errs, fmt.Errorf("%s: claim %q must be a parenthesised expression", at, con.Claim))
				}

				claims = append(claims, con.Claim)
			}
		}

		if refs[con.Phase] == nil {
			refs[con.Phase] = map[string]string{}
		}

		for i := range con.Rules {
			r := &con.Rules[i]
			if err := checkRule(r); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", at, err))

				continue
			}

			if prev, dup := refs[con.Phase][r.Ref]; dup {
				errs = append(errs, fmt.Errorf("%s: rule ref %q in %s is already written by %q", at, r.Ref, con.Phase, prev))

				continue
			}

			refs[con.Phase][r.Ref] = con.Source
		}

		byPhase[con.Phase] = append(byPhase[con.Phase], *con)
	}

	if cacheSeen {
		bypass := Contribution{
			Source: BypassSource, Phase: PhaseCacheSettings, Order: OrderBypass,
			Rules: []Rule{BypassRule(claims)},
		}
		if prev, dup := refs[PhaseCacheSettings][BypassRef]; dup {
			errs = append(errs, fmt.Errorf("rules: rule ref %q in %s is reserved for the assembler's bypass, written by %q", BypassRef, PhaseCacheSettings, prev))
		}

		list := append(slices.Clone(byPhase[PhaseCacheSettings]), bypass)
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].Order != list[j].Order {
				return list[i].Order < list[j].Order
			}

			return list[i].Source < list[j].Source
		})
		byPhase[PhaseCacheSettings] = list
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	out := map[Phase][]Rule{}

	for phase, list := range byPhase {
		rules := []Rule{}
		for i := range list {
			rules = append(rules, list[i].Rules...)
		}

		out[phase] = rules
	}

	return out, nil
}

// BypassRule is the cache phase's catch-all: it bypasses the cache for every
// request no claim covers. See the package documentation for the fold.
func BypassRule(claims []string) Rule {
	return Rule{
		Ref:         BypassRef,
		Description: BypassDescription,
		Action:      ActionSetCacheSettings,
		Expression:  BypassExpression(claims),
		Params:      Params{Cache: &CacheParams{Cache: false}},
	}
}

// BypassExpression folds claims, in the order given, into the expression of
// the requests none of them serves: "true" for no claims, "not a" for one,
// then "(<previous>) and not <claim>" for each further claim.
func BypassExpression(claims []string) string {
	if len(claims) == 0 {
		return "true"
	}

	expr := "not " + claims[0]
	for _, claim := range claims[1:] {
		expr = "(" + expr + ") and not " + claim
	}

	return expr
}

func knownPhase(p Phase) bool {
	for _, k := range Phases() {
		if p == k {
			return true
		}
	}

	return false
}

func checkRule(r *Rule) error {
	switch {
	case r.Ref == "":
		return errors.New("a rule has no ref")
	case r.Action == "":
		return fmt.Errorf("rule %q has no action", r.Ref)
	case r.Expression == "":
		return fmt.Errorf("rule %q has no expression", r.Ref)
	}

	return nil
}
