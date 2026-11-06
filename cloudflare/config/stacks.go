package config

import (
	"fmt"
	"strings"
)

// The Cloudflare stacks of an estate are PER ACCOUNT. A stack name maps to
// exactly one account of the registry:
//
//	cloudflare-<kind>            the legacy_names account (the names that
//	                             predate per-account stacks)
//	cloudflare-<kind>-<account>  any other account
//
// and each stack works on ForAccount(account): that account's zones,
// tunnels and R2 buckets, nothing of any other account. So a row added for
// one account never shows up in another account's preview, and the
// legacy_names account's stacks see exactly what they saw before a second
// account existed.
//
// The kinds below are the three the library's own components split into. An
// estate with stacks of another kind (the prototypes stack of the
// sluis presets, say) names and parses them itself.
const (
	// StackRoot mints the account's least-privilege children
	// (cloudflare/tokensplit).
	StackRoot = "root"
	// StackEdge owns the account's tunnels, DNS and zone settings
	// (cloudflare/edge).
	StackEdge = "edge"
	// StackR2 owns the account's R2 buckets (cloudflare/r2).
	StackR2 = "r2"

	stackPrefix = "cloudflare-"
)

// LegacyAccount is the account that sets legacy_names, if any.
func (c *Config) LegacyAccount() (string, bool) {
	for _, name := range sortedKeys(c.Accounts) {
		if c.Accounts[name].LegacyNames {
			return name, true
		}
	}

	return "", false
}

// CompanyOf is the company an account belongs to: accounts.<name>.company,
// else the account's own name. Stacks that are per company
// (cloudflare/zonestack and cloudflare/services) group accounts by it.
func (c *Config) CompanyOf(account string) string {
	if co := c.Accounts[account].Company; co != "" {
		return co
	}

	return account
}

// AccountsOf is the accounts of one company (CompanyOf), sorted.
func (c *Config) AccountsOf(company string) []string {
	var out []string

	for _, name := range sortedKeys(c.Accounts) {
		if c.CompanyOf(name) == company {
			out = append(out, name)
		}
	}

	return out
}

// StackName is the Pulumi stack of one kind (StackRoot, StackEdge,
// StackR2) for one account.
func (c *Config) StackName(kind, account string) string {
	if c.Accounts[account].LegacyNames {
		return stackPrefix + kind
	}

	return stackPrefix + kind + "-" + account
}

// StackAccount is the account a stack of the given kind serves, from its
// name (see the const block above). It refuses a name of another kind, an
// unknown account, and the legacy_names account spelled with a suffix (one
// account, one stack per kind).
func (c *Config) StackAccount(stack, kind string) (string, error) {
	base := stackPrefix + kind

	if stack == base {
		legacy, ok := c.LegacyAccount()
		if !ok {
			return "", fmt.Errorf("stack %s: %s has no legacy_names account for the unsuffixed stack to serve", stack, FileName)
		}

		return legacy, nil
	}

	account, ok := strings.CutPrefix(stack, base+"-")
	if !ok {
		return "", fmt.Errorf("stack %s is not a %s stack (%s or %s-<account>)", stack, kind, base, base)
	}

	a, ok := c.Accounts[account]
	if !ok {
		return "", fmt.Errorf("stack %s: %s has no account %q (known: %s)", stack, FileName, account, strings.Join(sortedKeys(c.Accounts), ", "))
	}

	if a.LegacyNames {
		return "", fmt.Errorf("stack %s: %q is the legacy_names account, whose stack is %s", stack, account, base)
	}

	return account, nil
}

// RootFor is the account's root-stack child rotations: the top-level
// `root` block for the legacy_names account, accounts.<name>.root for any
// other (every child at the default rotation when that is absent).
func (c *Config) RootFor(account string) RootChildren {
	a := c.Accounts[account]
	if a.LegacyNames {
		return c.Root
	}

	if a.Root != nil {
		return *a.Root
	}

	var r RootChildren
	r.applyDefaults()

	return r
}

// ForAccount is the registry as one account's stacks see it: that account
// alone, its zones, its tunnels and its R2 buckets (enabled or not), the
// fleet, and its own rotations as Root. It refuses while one of the
// account's zones is zone_id_pending: every stack of the account needs the
// zone id (the children are scoped to it, the edge stack manages it).
func (c *Config) ForAccount(account string) (*Config, error) {
	a, ok := c.Accounts[account]
	if !ok {
		return nil, fmt.Errorf("%s: no account %q (known: %s)", FileName, account, strings.Join(sortedKeys(c.Accounts), ", "))
	}

	out := &Config{
		Accounts: map[string]Account{account: a},
		Zones:    map[string]Zone{},
		Fleets:   c.Fleets,
		Root:     c.RootFor(account),

		providerNames: c.providerNames,
	}

	for _, name := range sortedKeys(c.Zones) {
		z := c.Zones[name]
		if z.Account != account {
			continue
		}

		if z.ZoneIDPending {
			return nil, fmt.Errorf("%s: zones[%s] is zone_id_pending: the account %q's stacks need its zone id, "+
				"an owner TODO (Cloudflare dashboard, the zone's Overview page); write zone_id and drop zone_id_pending",
				FileName, name, account)
		}

		out.Zones[name] = z
	}

	for i := range c.Tunnels {
		t := &c.Tunnels[i]

		if t.Account == account {
			out.Tunnels = append(out.Tunnels, *t)
		}
	}

	for _, name := range sortedKeys(c.R2) {
		if b := c.R2[name]; b.Account == account {
			if out.R2 == nil {
				out.R2 = map[string]R2Bucket{}
			}

			out.R2[name] = b
		}
	}

	return out, nil
}

// The kinds of the per-company stacks (see CompanyOf). Unlike the per-account
// kinds above they are named after the company, never an account, and have no
// legacy spelling.
const (
	// StackKindZone owns a company's zones: zone settings, rulesets, cache,
	// WAF, bot management and the zone's own records (cloudflare/zonestack).
	StackKindZone = "zone"
	// StackKindServices owns a company's tunnels and their DNS records
	// (cloudflare/services).
	StackKindServices = "services"
	// StackKindSTS is a company's token-minting (sluis) stack.
	StackKindSTS = "sts"
)

// StackName is the Pulumi stack of one per-company kind for one company:
// cloudflare-<company>-<kind>. This is the function; the method of the same
// name on Config names the per-account stacks (cloudflare-<kind>-<account>).
func StackName(kind, company string) string {
	return stackPrefix + company + "-" + kind
}

// StackZone is a company's zone stack, cloudflare-<company>-zone.
func StackZone(company string) string { return StackName(StackKindZone, company) }

// StackServices is a company's services stack, cloudflare-<company>-services.
func StackServices(company string) string { return StackName(StackKindServices, company) }

// StackSTS is a company's sts stack, cloudflare-<company>-sts.
func StackSTS(company string) string { return StackName(StackKindSTS, company) }
