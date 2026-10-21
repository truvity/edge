package catalog

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// PATH GRANTS: one host, several tenants, one prefix each.
//
// A domain without `paths` is granted whole: every route namespace of its
// group may attach any path on it, which is what every host was before
// this existed and what most hosts stay. A domain WITH `paths` is a SHARED
// host: each entry grants one path prefix to one namespace, and nothing
// else on the host belongs to anybody. The canonical case is one platform
// host serving several installations of one service, one prefix each
// (`<host>/<installation>`), where granting the host to every
// installation's namespace would let any of them answer for another's
// prefix.
//
// The catalog states the grant; it enforces nothing by itself. A Gateway
// API listener matches a host, never a path, so the ListenerSet still
// admits every granted namespace on the whole host. What holds a route to
// its prefix is whatever reads the grant back: the gateway-groups chart
// refuses a platform route outside its namespace's prefix, and an
// admission policy generated from PathGrantedHosts refuses the rest.

// PathGrant is one prefix of a shared host, granted to one namespace.
type PathGrant struct {
	// Prefix is the path prefix, matched the way Gateway API's PathPrefix
	// matches: element by element, so /a covers /a and /a/b but not /ab.
	// Lowercase, no trailing slash, no wildcard, never / alone.
	Prefix string `yaml:"prefix"`
	// Namespace is the one namespace whose routes may serve the prefix.
	// It must be one of the group's route_namespaces.
	Namespace string `yaml:"namespace"`
}

// HostPaths is one shared host on one cluster: where it is claimed and
// what each namespace is granted on it.
type HostPaths struct {
	Host     string
	Exposure string
	Group    string
	// Paths are the host's grants, sorted by prefix.
	Paths []PathGrant
}

// UnmarshalYAML takes a {prefix, namespace} mapping and refuses any other
// key: the loader's strict decoding does not reach into Domain's custom
// unmarshaller, and a misspelled key would otherwise grant nothing while
// reading as a grant.
func (p *PathGrant) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: a path grant is a {prefix, namespace} mapping", node.Line)
	}

	for i := 0; i < len(node.Content); i += 2 {
		switch key := node.Content[i].Value; key {
		case "prefix", "namespace":
		default:
			return fmt.Errorf("line %d: path grant field %q is not prefix or namespace", node.Content[i].Line, key)
		}
	}

	type plain PathGrant

	return node.Decode((*plain)(p))
}

// PathGranted reports whether the domain is a shared host: one whose
// paths are granted one prefix per namespace, rather than whole.
func (d Domain) PathGranted() bool { return len(d.Paths) > 0 }

// PrefixByNamespace is the prefix each namespace is granted on the domain,
// keyed by namespace. Nil on a domain granted whole.
func (d Domain) PrefixByNamespace() map[string]string {
	if !d.PathGranted() {
		return nil
	}

	out := make(map[string]string, len(d.Paths))
	for _, grant := range d.Paths {
		out[grant.Namespace] = grant.Prefix
	}

	return out
}

// SortedPaths is the domain's grants ordered by prefix.
func (d Domain) SortedPaths() []PathGrant {
	out := slices.Clone(d.Paths)
	slices.SortFunc(out, func(a, b PathGrant) int { return strings.Compare(a.Prefix, b.Prefix) })

	return out
}

// PrefixCovers reports whether a request path, or a route's PathPrefix or
// Exact value, falls under prefix the way Gateway API's PathPrefix
// matches: the prefix itself, or the prefix followed by a "/". So /a
// covers /a, /a/ and /a/b, and does not cover /ab.
func PrefixCovers(prefix, path string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+"/")
}

// PrefixesOverlap reports whether two prefixes could both match one
// request: they are equal, or one covers the other.
func PrefixesOverlap(a, b string) bool {
	return PrefixCovers(a, b) || PrefixCovers(b, a)
}

// PathPrefixes is, for one host on one cluster, the prefix granted to each
// namespace. ok is false when no group on the cluster claims the host or
// when the host is granted whole; either way there is no per-namespace
// prefix to hold a route to.
func (c *Config) PathPrefixes(cluster, host string) (map[string]string, bool) {
	for _, shared := range c.PathGrantedHosts(cluster) {
		if shared.Host == host {
			out := make(map[string]string, len(shared.Paths))
			for _, grant := range shared.Paths {
				out[grant.Namespace] = grant.Prefix
			}

			return out, true
		}
	}

	return nil, false
}

// PathGrantedHosts is every shared host on one cluster, on every exposure,
// sorted by host. It is what an admission policy is generated from: for
// each host, the one prefix each namespace may route, and no route at all
// from any other namespace. Nil when the cluster has no shared host.
func (c *Config) PathGrantedHosts(cluster string) []HostPaths {
	var out []HostPaths

	for _, exposure := range c.ExposuresOn(cluster) {
		for _, group := range c.Groups(exposure, cluster) {
			for _, domain := range group.Domains {
				if !domain.PathGranted() {
					continue
				}

				out = append(out, HostPaths{
					Host:     domain.Host,
					Exposure: exposure,
					Group:    group.Name,
					Paths:    domain.SortedPaths(),
				})
			}
		}
	}

	slices.SortFunc(out, func(a, b HostPaths) int { return strings.Compare(a.Host, b.Host) })

	return out
}

// PathGranted reports whether the group serves shared hosts. A valid
// group's domains are all shared or none are (validatePaths).
func (g Group) PathGranted() bool {
	return slices.ContainsFunc(g.Domains, Domain.PathGranted)
}

// PathGrantedNamespaces is every namespace holding a path grant on any of
// the group's domains, sorted.
func (g Group) PathGrantedNamespaces() []string {
	set := map[string]bool{}

	for _, domain := range g.Domains {
		for _, grant := range domain.Paths {
			set[grant.Namespace] = true
		}
	}

	return slices.Sorted(maps.Keys(set))
}

// pathPrefixPattern is a lowercase absolute path of one or more non-empty
// segments, with no trailing slash and none of the characters that would
// make it a pattern or an encoding (*, ?, #, %, whitespace).
var pathPrefixPattern = regexp.MustCompile(`^(/[a-z0-9._~-]+)+$`)

// validatePaths holds a group's path grants to their rules. Each refusal
// names the host and the entry, because a grant that is wrong in the
// catalog is a tenant answering for another's prefix once it is live.
func (g Group) validatePaths(where string) error {
	granted := 0

	for _, domain := range g.Domains {
		if domain.PathGranted() {
			granted++
		}
	}

	if granted == 0 {
		return nil
	}

	// One ListenerSet carries one route grant for every listener of the
	// group. A namespace admitted for a prefix on a shared host would be
	// admitted on a whole host beside it, with no prefix to hold it to.
	if granted != len(g.Domains) {
		return fmt.Errorf(
			"cfg/gateways: %s.domains: some domains have paths and some do not; one ListenerSet admits the same namespaces on every listener, "+
				"so a namespace granted a prefix on one host would be admitted on the whole of the other. Put the shared host in a group of its own",
			where)
	}

	for _, domain := range g.Domains {
		if err := g.validateDomainPaths(where, domain); err != nil {
			return err
		}
	}

	// A route namespace with no prefix on any shared host has nothing it
	// may serve, yet the ListenerSet would admit it on every host.
	holders := g.PathGrantedNamespaces()
	for _, namespace := range g.RouteNamespaces {
		if !slices.Contains(holders, namespace) {
			return fmt.Errorf(
				"cfg/gateways: %s.route_namespaces: %q holds no path on any of the group's domains, and every domain is a shared host; "+
					"grant it a prefix or remove it from route_namespaces",
				where, namespace)
		}
	}

	return nil
}

func (g Group) validateDomainPaths(where string, domain Domain) error {
	at := fmt.Sprintf("cfg/gateways: %s.domains[%s].paths", where, domain.Host)

	if strings.HasPrefix(domain.Host, wildcardPrefix) {
		return fmt.Errorf("%s: %q is a wildcard; a path grant shares one exact host, so list that host as its own domain", at, domain.Host)
	}

	byNamespace := map[string]string{}

	for i, grant := range domain.Paths {
		entry := fmt.Sprintf("%s[%d]", at, i)

		if err := validatePrefix(entry, grant.Prefix); err != nil {
			return err
		}

		switch {
		case grant.Namespace == "":
			return fmt.Errorf("%s.namespace is required: the one namespace whose routes may serve %s", entry, grant.Prefix)
		case !dnsLabel.MatchString(grant.Namespace):
			return fmt.Errorf("%s.namespace: %q is not a namespace name (a DNS label)", entry, grant.Namespace)
		case !slices.Contains(g.RouteNamespaces, grant.Namespace):
			return fmt.Errorf(
				"%s.namespace: %q is not in the group's route_namespaces %v; the ListenerSet would not admit its routes at all",
				entry, grant.Namespace, g.RouteNamespaces)
		}

		if other, ok := byNamespace[grant.Namespace]; ok {
			return fmt.Errorf(
				"%s: namespace %q already holds %s on this host; a namespace is granted one prefix per host",
				entry, grant.Namespace, other)
		}

		byNamespace[grant.Namespace] = grant.Prefix

		for j, earlier := range domain.Paths[:i] {
			if PrefixesOverlap(earlier.Prefix, grant.Prefix) {
				if earlier.Prefix == grant.Prefix {
					return fmt.Errorf("%s: prefix %s is granted twice on this host (also paths[%d], to %s)",
						entry, grant.Prefix, j, earlier.Namespace)
				}

				return fmt.Errorf(
					"%s: prefix %s overlaps %s (paths[%d], granted to %s); one request would match both, so prefixes on a host never nest",
					entry, grant.Prefix, earlier.Prefix, j, earlier.Namespace)
			}
		}
	}

	return nil
}

// validatePrefix refuses the shapes that look like a prefix and match
// something else, or nothing.
func validatePrefix(entry, prefix string) error {
	switch {
	case prefix == "":
		return fmt.Errorf("%s.prefix is required", entry)
	case !strings.HasPrefix(prefix, "/"):
		return fmt.Errorf("%s.prefix: %q does not start with /", entry, prefix)
	case prefix == "/":
		return fmt.Errorf("%s.prefix: / is the whole host; a host granted whole has no paths", entry)
	case strings.HasSuffix(prefix, "*"):
		return fmt.Errorf("%s.prefix: %q ends in a wildcard; a prefix already covers everything below it, so write it without the *", entry, prefix)
	case strings.HasSuffix(prefix, "/"):
		return fmt.Errorf("%s.prefix: %q ends in /; write it without the trailing slash (it covers %s/... either way)",
			entry, prefix, strings.TrimSuffix(prefix, "/"))
	case prefix != strings.ToLower(prefix):
		return fmt.Errorf("%s.prefix: %q is not lowercase; paths compare case-sensitively, so the catalog keeps one spelling", entry, prefix)
	case !pathPrefixPattern.MatchString(prefix):
		return fmt.Errorf("%s.prefix: %q is not a path of non-empty segments of [a-z0-9._~-] (no *, ?, #, %%, // or spaces)", entry, prefix)
	}

	for segment := range strings.SplitSeq(prefix[1:], "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("%s.prefix: %q has a %q segment, which a client normalises away", entry, prefix, segment)
		}
	}

	return nil
}
