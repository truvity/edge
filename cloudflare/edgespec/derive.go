// Package edgespec holds the pure side of the edge stack: the tunnel naming
// rules, zone lookup, Derive and ValidateRouting, and the cloudflared
// ingress-order rules (OrderHosts, Covers). No Cloudflare API call, no
// Pulumi resource, no SDK import of any kind: an estate's config layer that
// only derives and validates tunnels links this package and not the Pulumi
// and Cloudflare SDKs that cloudflare/edge, cloudflare/tunnel and cloudflare/zone import to
// deploy them. cloudflare/edge re-exports everything here as type aliases and
// thin wrappers, so existing callers keep compiling.
//
// TestNoNonStdlibImports fails the moment this package imports anything
// outside the standard library.
package edgespec

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// ReleaseBase is the name stem of a cloudflared Helm release and of its
// tunnel-token Secret.
const ReleaseBase = "cloudflared"

// TunnelName is the tunnel's name in Cloudflare, and the name its Pulumi
// resources are registered under: the cluster alone for an account with
// legacy names (the tunnel that predates a second account), and
// <cluster>-<account> for any other.
func TunnelName(cluster, account string, legacy bool) string {
	return suffixed(cluster, account, legacy)
}

// ReleaseName is the cloudflared Helm release of one tunnel, and the suffix of
// its Application's name: ReleaseBase, or ReleaseBase-<account>.
func ReleaseName(account string, legacy bool) string {
	return suffixed(ReleaseBase, account, legacy)
}

// SecretName is the Kubernetes Secret holding the tunnel's token.
func SecretName(account string, legacy bool) string {
	return suffixed(ReleaseBase, account, legacy) + "-tunnel-token"
}

func suffixed(base, account string, legacy bool) string {
	if legacy {
		return base
	}

	return base + "-" + account
}

// ZoneOf returns the zone a hostname lies in: the longest of zones that is
// the name itself or one of its parents (a wildcard's `*.` is not a label of
// the zone), or "" when none holds it.
func ZoneOf(zones []string, host string) string {
	name := strings.ToLower(strings.TrimPrefix(host, "*."))
	best := ""

	for _, zone := range zones {
		if (name == zone || strings.HasSuffix(name, "."+zone)) && len(zone) > len(best) {
			best = zone
		}
	}

	return best
}

type (
	// Tunnel is one tunnel: the hosts one cluster serves through one
	// account's zones.
	Tunnel struct {
		// Name is the tunnel's name and the Pulumi name of every resource
		// built for it.
		Name    string
		Cluster string
		Account string
		// Zone is the zone of the first host. When Zones is empty, every
		// host lies in it.
		Zone string
		// Zones are the zones the hosts lie in, sorted, every one of the
		// tunnel's account. Each host's DNS record goes in the zone that is
		// its longest suffix (ZoneOf). Derive always fills it; a tunnel built
		// by hand may leave it empty to mean Zones = {Zone}.
		Zones []string
		// Hosts arrive derived and in ingress order, each in one of Zones.
		Hosts []string
		// DNSElsewhere are the hosts, a subset of Hosts, whose DNS record
		// something else writes: each keeps its ingress rule and has no
		// CNAME created for it (DNSNames). In ingress order.
		DNSElsewhere []string
		// DNSDomain is the cluster's Kubernetes DNS domain, for the origin
		// service address. Empty falls back to "cluster."+Cluster.
		DNSDomain string
	}

	// Group is one named set of hostnames a cluster serves on one exposure.
	Group struct {
		Name    string
		Cluster string
		// Hosts are the group's hostnames, exact or a leading wildcard.
		Hosts []string
		// RouteNamespaces are the namespaces the group admits routes from.
		RouteNamespaces []string
		// DNSElsewhere are the group's hosts, a subset of Hosts, whose DNS
		// record is written by something other than the tunnel: they get
		// an ingress rule and no CNAME.
		DNSElsewhere []string
		// WAFOff are the group's hosts, a subset of Hosts, that the catalog
		// opted out of the managed WAF (each with a reason, kept in the
		// catalog).
		WAFOff []string
	}

	// Exposure is one way names arrive through Cloudflare tunnels: the
	// accounts its tunnels may use and the groups it serves.
	Exposure struct {
		Name string
		// Accounts the exposure's tunnels may use. Required.
		Accounts []string
		Groups   []Group
	}

	// DeriveInput is everything Derive reads.
	DeriveInput struct {
		// Exposures are the exposures that arrive by a Cloudflare tunnel.
		Exposures []Exposure
		// Zones maps each zone name to the account that owns it.
		Zones map[string]string
		// LegacyNames maps each account to its legacy-names flag. Its keys
		// are the known accounts.
		LegacyNames map[string]bool
		// Business names the projects whose groups are cached at the edge.
		Business map[string]struct{}
		// CacheZones are the zones that declare a cache policy; only they get
		// CacheHosts.
		CacheZones []string
		// WAFZones are the zones that enable the managed WAF; only they get
		// WAFHosts, and only in them is a proxied wildcard host refused
		// unless the catalog opted it out.
		WAFZones []string
		// WAFWholeZones are the WAFZones whose rule covers the whole zone
		// (managed_waf scope: zone). In them the catalog's opted-out names
		// are returned as WAFExcluded, and a proxied wildcard host needs no
		// opt-out: the zone is covered, not listed.
		WAFWholeZones []string
	}

	// Derived is what Derive returns.
	Derived struct {
		// Tunnels are one per (cluster, account), sorted by cluster then
		// account, hosts in ingress order. DNSDomain is left empty.
		Tunnels []Tunnel
		// CacheHosts maps each cache zone to its sorted, de-duplicated hosts.
		CacheHosts map[string][]string
		// WAFHosts maps each WAF zone to its sorted, de-duplicated exact
		// hosts: every host an exposure serves in the zone, opted-out ones
		// removed. Never a wildcard.
		WAFHosts map[string][]string
		// WAFExcluded maps each WAFWholeZones zone to its sorted,
		// de-duplicated opted-out names, exact or a leading wildcard.
		WAFExcluded map[string][]string
	}
)

// Derive resolves every host of every exposure to its zone and account,
// groups the hosts into one tunnel per (cluster, account) in ingress order
// (OrderHosts), and derives each cache zone's hosts. A host outside
// every zone, or in a zone of an account its exposure does not list, is
// refused here, where the exposure is still known. A tunnel's hosts may lie
// in several zones of its account (Tunnel.Zones); a zone of another account
// never joins it, since the tunnel is keyed by the zone's account. Callers
// pass exposures, clusters and groups in a stable order so the first refusal
// is stable.
func Derive(in DeriveInput) (Derived, error) {
	zones := sortedKeys(in.Zones)

	type key struct{ cluster, account string }

	hosts := map[key][]string{}
	elsewhere := map[key]map[string]bool{}

	for _, e := range in.Exposures {
		if len(e.Accounts) == 0 {
			return Derived{}, fmt.Errorf("exposures[%s] arrives by a Cloudflare tunnel and lists no accounts: name the accounts its tunnels may use", e.Name)
		}

		for _, account := range e.Accounts {
			if _, ok := in.LegacyNames[account]; !ok {
				return Derived{}, fmt.Errorf("exposures[%s].accounts: no account %q (known: %s)", e.Name, account, strings.Join(sortedKeys(in.LegacyNames), ", "))
			}
		}

		for gi := range e.Groups {
			g := &e.Groups[gi]

			for _, host := range g.Hosts {
				zone := ZoneOf(zones, host)
				if zone == "" {
					return Derived{}, fmt.Errorf("exposures[%s].clusters[%s].groups[%s]: %q lies in no zone (%s)",
						e.Name, g.Cluster, g.Name, host, strings.Join(zones, ", "))
				}

				account := in.Zones[zone]
				if !slices.Contains(e.Accounts, account) {
					return Derived{}, fmt.Errorf(
						"exposures[%s].clusters[%s].groups[%s]: %q is in zone %s of account %q, and the exposure arrives through %v: "+
							"list the account on the exposure, or serve the name elsewhere",
						e.Name, g.Cluster, g.Name, host, zone, account, e.Accounts)
				}

				k := key{g.Cluster, account}
				hosts[k] = append(hosts[k], host)

				if slices.Contains(g.DNSElsewhere, host) {
					if elsewhere[k] == nil {
						elsewhere[k] = map[string]bool{}
					}

					elsewhere[k][host] = true
				}
			}
		}
	}

	wafHosts, wafExcluded, err := deriveWAF(in, zones)
	if err != nil {
		return Derived{}, err
	}

	out := Derived{CacheHosts: deriveCache(in, zones), WAFHosts: wafHosts, WAFExcluded: wafExcluded}

	for k, list := range hosts {
		ordered := OrderHosts(list)

		// Each host's record goes in its own zone (cloudflare/tunnel DNS.Zones), so
		// one tunnel may serve names in several zones of its account.
		inZone := map[string]bool{}
		for _, host := range ordered {
			inZone[ZoneOf(zones, host)] = true
		}

		var skipped []string

		for _, host := range ordered {
			if elsewhere[k][host] {
				skipped = append(skipped, host)
			}
		}

		out.Tunnels = append(out.Tunnels, Tunnel{
			Name:         TunnelName(k.cluster, k.account, in.LegacyNames[k.account]),
			Cluster:      k.cluster,
			Account:      k.account,
			Zone:         ZoneOf(zones, ordered[0]),
			Zones:        sortedKeys(inZone),
			Hosts:        ordered,
			DNSElsewhere: skipped,
		})
	}

	sortTunnels(out.Tunnels)

	return out, nil
}

// DNSNames are the hosts the tunnel creates a CNAME for: Hosts without
// DNSElsewhere, in ingress order. Empty when every host's record is written
// elsewhere, and then the tunnel has no DNS block at all.
func (t Tunnel) DNSNames() []string {
	if len(t.DNSElsewhere) == 0 {
		return t.Hosts
	}

	out := make([]string, 0, len(t.Hosts))

	for _, host := range t.Hosts {
		if !slices.Contains(t.DNSElsewhere, host) {
			out = append(out, host)
		}
	}

	return out
}

func sortTunnels(tunnels []Tunnel) {
	slices.SortFunc(tunnels, func(a, b Tunnel) int {
		if d := strings.Compare(a.Cluster, b.Cluster); d != 0 {
			return d
		}

		return strings.Compare(a.Account, b.Account)
	})
}

// deriveCache is each cache zone's hosts: every exact name a BUSINESS
// project is served on, from an exposure that arrives from the internet.
//
// WHY DERIVED, AND WHY THESE. A zone caches by file extension unless it is
// told otherwise: a response under a name ending in .js or .css is stored at
// the edge whether or not its origin asked, and where the origin sent no
// Cache-Control the zone claims four hours of the browser's. That is how a
// single-page console, which answers its own unknown paths with its shell,
// ends up with one page cached under another page's name.
//
// The answer is not "cache less" but "cache what an origin asked for", and
// only where an origin is in a position to ask. A business project's chart is
// the estate's own, ships content-addressed assets and sets the headers to say
// so. A platform console is somebody else's chart on somebody else's release
// cycle. Those are left out, and left out is safe: the rules this list feeds
// bypass everything they do not name.
//
// A group is a business project's when EVERY namespace it admits routes from
// is a project. Mixed or unknown reads as platform, which caches nothing: the
// failure this direction is a request that reached a pod the long way, and the
// other is a response held at the edge that nobody meant to publish.
//
// A WILDCARD IS NOT A PROJECT'S CLAIM ON EVERY NAME UNDER IT. `*.<tier>.<zone>`
// matches every name of that tier, including the consoles other groups serve,
// whose bundles are the reason this list exists. A suffix test cannot say
// "except those", so a wildcard is left out and the exact names beside it carry
// the policy.
func deriveCache(in DeriveInput, zones []string) map[string][]string {
	if len(in.CacheZones) == 0 {
		return nil
	}

	byZone := map[string][]string{}

	for _, e := range in.Exposures {
		for gi := range e.Groups {
			g := &e.Groups[gi]

			if !businessGroup(*g, in.Business) {
				continue
			}

			for _, host := range g.Hosts {
				if strings.HasPrefix(host, "*.") {
					continue
				}

				if zone := ZoneOf(zones, host); zone != "" {
					byZone[zone] = append(byZone[zone], host)
				}
			}
		}
	}

	out := make(map[string][]string, len(in.CacheZones))

	for _, zone := range in.CacheZones {
		hosts := byZone[zone]
		slices.Sort(hosts)

		out[zone] = slices.Compact(hosts)
	}

	return out
}

// businessGroup reports whether every namespace a group admits routes from is
// a business project.
func businessGroup(g Group, business map[string]struct{}) bool {
	if len(g.RouteNamespaces) == 0 {
		return false
	}

	for _, ns := range g.RouteNamespaces {
		if _, ok := business[ns]; !ok {
			return false
		}
	}

	return true
}

// ValidateRouting enforces the tunnel ingress invariants cloudflared cannot
// check itself:
//   - every hostname is unique across every tunnel, and so across every zone
//     and account: a duplicate makes two ingress rules (or two tunnels) claim
//     one DNS name, and only the first ever receives traffic;
//   - within a tunnel, no rule is caught by an EARLIER rule, with
//     cloudflared's own matching: an exact host after a wildcard that covers
//     it, or a deeper wildcard after a broader one (`*` spans dots). A
//     shadowed rule is unreachable, and its traffic arrives under the
//     shadowing rule's SNI (see OrderHosts).
func ValidateRouting(tunnels []Tunnel) error {
	sorted := slices.Clone(tunnels)
	sortTunnels(sorted)

	seen := map[string]string{} // lowercased host to "tunnel cluster/account: hosts[i]"

	for ti := range sorted {
		t := &sorted[ti]

		for i, host := range t.Hosts {
			key := strings.ToLower(host)
			at := fmt.Sprintf("tunnel %s/%s: hosts[%d]", t.Cluster, t.Account, i)

			if prev, ok := seen[key]; ok {
				return fmt.Errorf("%s: hostname %q already declared at %s", at, host, prev)
			}

			seen[key] = at

			for _, earlier := range t.Hosts[:i] {
				if Covers(strings.ToLower(earlier), key) {
					return fmt.Errorf(
						"%s: %q is shadowed by earlier rule %q: exact hosts first, "+
							"deeper wildcards before broader ones (first match wins; origin SNI = matched rule)",
						at, host, earlier)
				}
			}
		}
	}

	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// deriveWAF is each WAF zone's hosts: every exact name an exposure serves
// through a tunnel in the zone (the names Cloudflare proxies, whether or not
// the tunnel writes their CNAME), minus the names the catalog opted out.
//
// EXPLICIT, NEVER A WILDCARD. The managed WAF lists the names it covers, one
// by one, beside where each is declared. A wildcard host cannot be listed: it
// would be a suffix test that also covers names nobody declared. So a proxied
// wildcard host in a WAF zone must be opted out with a reason, and is
// refused here, naming the host, when it is not.
//
// A zone in WAFWholeZones is the other form: its rule covers the whole zone,
// so nothing is listed (no host, and no wildcard to refuse) and the opted-out
// names, wildcards included, are returned as excluded.
func deriveWAF(in DeriveInput, zones []string) (hosts, excluded map[string][]string, err error) {
	if len(in.WAFZones) == 0 {
		return nil, nil, nil
	}

	byZone := map[string][]string{}
	offByZone := map[string][]string{}

	for _, e := range in.Exposures {
		for gi := range e.Groups {
			g := &e.Groups[gi]

			for _, host := range g.Hosts {
				zone := ZoneOf(zones, host)
				if zone == "" || !slices.Contains(in.WAFZones, zone) {
					continue
				}

				whole := slices.Contains(in.WAFWholeZones, zone)

				if slices.Contains(g.WAFOff, host) {
					if whole {
						offByZone[zone] = append(offByZone[zone], host)
					}

					continue
				}

				if whole {
					continue
				}

				if strings.HasPrefix(host, "*.") {
					return nil, nil, fmt.Errorf(
						"exposures[%s].clusters[%s].groups[%s]: %q is a proxied wildcard in zone %s, whose managed_waf lists exact names only: "+
							"give the domain waf: off and a waf_reason, and list the exact names it serves",
						e.Name, g.Cluster, g.Name, host, zone)
				}

				byZone[zone] = append(byZone[zone], host)
			}
		}
	}

	hosts = make(map[string][]string, len(in.WAFZones))
	if len(in.WAFWholeZones) > 0 {
		excluded = make(map[string][]string, len(in.WAFWholeZones))
	}

	for _, zone := range in.WAFZones {
		list := byZone[zone]
		slices.Sort(list)

		hosts[zone] = slices.Compact(list)
	}

	for _, zone := range in.WAFWholeZones {
		list := offByZone[zone]
		slices.Sort(list)

		excluded[zone] = slices.Compact(list)
	}

	return hosts, excluded, nil
}
