package config

import (
	"fmt"

	"github.com/truvity/edge/v4/cloudflare/rules"
)

// SSLModeRef is the rule ref of host's rule in the zone's ssl_mode_rules:
// the pinned ssl_mode_rule_refs entry, else "ssl-mode-<host>". A pinned ref
// keeps a hand-made rule's identity when the next apply adopts it.
func (z Zone) SSLModeRef(host string) string {
	if ref := z.SSLModeRuleRefs[host]; ref != "" {
		return ref
	}

	return "ssl-mode-" + host
}

// Contributions are the rules the zone's blocks ask for, built with the
// cloudflare/rules builders so a caller hands them to the zone component
// (zonespec.Args.Contributions) and builds none by hand: public_cache (the
// public documents of one host cached at the edge), ssl_mode_rules (one
// set_config rule per host), rate_limits, trusted_clients and managed_waf. A
// block the zone does not declare contributes nothing.
//
// Not included, because the zone component takes them as blocks of their
// own: the cacheable hosts (`cache`, zonespec.Args.Cache with CacheHosts),
// the canonical-host redirects (`redirects`, zonespec.Args.Redirects) and
// the settings that are not rules (always_use_https, hsts, total_tls,
// min_tls_version, records). A caller that passes these contributions must
// not also pass the same blocks as zonespec.Args.RateLimits, TrustedClients,
// ManagedWAF or Redirects, or the rules are written twice.
//
// The zone's Name must be set (Parse and Validate do); an error is the
// builders', unchanged, prefixed with the block.
func (z Zone) Contributions() ([]rules.Contribution, error) {
	if z.SecurityTxtOnly() {
		return nil, nil
	}

	var out []rules.Contribution

	if p := z.PublicCache; p != nil {
		rule, err := rules.PublicPathsCache(p.Host, rules.PublicPathsArgs{TTL: p.EdgeTTL()})
		if err != nil {
			return nil, fmt.Errorf("public_cache: %w", err)
		}

		out = append(out, rule.Contribution())
	}

	for _, host := range sortedKeys(z.SSLModeRules) {
		c, err := rules.SSLMode(host, z.SSLModeRules[host], z.SSLModeRef(host))
		if err != nil {
			return nil, fmt.Errorf("ssl_mode_rules[%s]: %w", host, err)
		}

		out = append(out, c)
	}

	if rl := z.RateLimits.Library(); rl != nil {
		c, err := rules.RateLimits(rl.Plan, rl.Rules)
		if err != nil {
			return nil, fmt.Errorf("rate_limits: %w", err)
		}

		out = append(out, c)
	}

	if t := z.TrustedClients; t != nil {
		c, err := rules.TrustedClients(t.Library(z.Name))
		if err != nil {
			return nil, fmt.Errorf("trusted_clients: %w", err)
		}

		out = append(out, c)
	}

	if w := z.ManagedWAFLibrary(z.Name); w != nil {
		c, err := rules.ManagedWAF(w)
		if err != nil {
			return nil, fmt.Errorf("managed_waf: %w", err)
		}

		out = append(out, c)
	}

	return out, nil
}
