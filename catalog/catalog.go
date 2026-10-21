// Package catalog loads and validates the edge catalog: which names are
// served, on which way in, by which namespaces' routes.
//
// The two words this package keeps apart:
//
//   - an EXPOSURE is one way in (a Gateway). It decides the address, the
//     failure domain and the client range, and the platform owns it.
//   - a GROUP is one project's claim on an exposure (a ListenerSet): the
//     names it answers to and the namespaces whose routes may attach. A
//     project can own a group without being able to change the way in.
//
// The per-project groups are generated from the projects a caller supplies
// (see WithProjects) rather than written by hand.
package catalog

import (
	"bytes"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Arrival is how traffic reaches an exposure. It is an enum rather than
// free text because each value implies a different Service shape, and a
// typo would otherwise render an exposure nothing can reach.
const (
	// ArrivalCloudflareTunnel: cloudflared dials out and the fleet's
	// Service is a ClusterIP. Nothing listens on a public address.
	ArrivalCloudflareTunnel = "cloudflare-tunnel"
	// ArrivalPrivateClusterIP: a pinned ClusterIP the tailnet and the
	// peered VPCs reach directly.
	ArrivalPrivateClusterIP = "private-clusterip"
	// ArrivalNLB: a load balancer, for the one case a ClusterIP cannot
	// serve -- a client outside every peered network.
	ArrivalNLB = "nlb"
)

const (
	// PrivateExposure is the exposure every cluster keeps: the way in
	// that answers on the pinned ClusterIP, and the one whose loss cannot
	// be worked around from outside the cluster.
	PrivateExposure = "private"
)

const (
	// Version is the only catalog shape this loader understands. A file
	// that says anything else is from a different edge model, and
	// guessing at it would render the wrong listeners rather than fail.
	Version = 1

	// FileName is the catalog's conventional path within a config
	// filesystem.
	FileName = "gateways.yaml"
)

type (
	// Config is the whole catalog.
	Config struct {
		Version   int                 `yaml:"version"`
		Exposures map[string]Exposure `yaml:"exposures"`
	}

	// Exposure is one way in, on every cluster that has it.
	Exposure struct {
		Description string `yaml:"description,omitempty"`
		Class       string `yaml:"class"`
		Arrival     string `yaml:"arrival"`
		// CloudflareAccounts are the Cloudflare accounts a
		// cloudflare-tunnel exposure may arrive through. Each domain
		// resolves to its zone and so to an account, and each cluster
		// gets one tunnel per account in use : listing an
		// account is what lets a domain row create a tunnel and a
		// cloudflared install, so it never happens as a side effect.
		// Required on a cloudflare-tunnel exposure (configcloudflare,
		// which knows the accounts) and refused on any other arrival.
		CloudflareAccounts []string `yaml:"cloudflare_accounts,omitempty"`
		// ProjectGroups, when present, makes the exposure's per-project
		// groups GENERATED: see WithProjects. A project's group is not
		// written under clusters.<c>.groups; only what the generation
		// cannot know is.
		ProjectGroups *ProjectGroups     `yaml:"project_groups,omitempty"`
		Clusters      map[string]Cluster `yaml:"clusters"`
	}

	// Cluster is one exposure on one cluster: the Gateway, and the
	// groups whose ListenerSets attach to it.
	Cluster struct {
		// Class overrides the exposure's GatewayClass on this cluster
		// alone. It exists for one move and should stay rare: giving an
		// exposure its own data plane is a change to live traffic, and
		// doing it on every cluster at once is how a way in that nothing
		// else can replace goes down everywhere.
		// Empty means the exposure's own class.
		Class  string           `yaml:"class,omitempty"`
		Groups map[string]Group `yaml:"groups"`
	}

	// NamedGroup is a group with the key it was filed under.
	NamedGroup struct {
		Name string
		Group
	}

	// Group is one project's claim on an exposure.
	Group struct {
		Description string   `yaml:"description,omitempty"`
		Domains     []Domain `yaml:"domains"`
		// RouteNamespaces are the namespaces whose HTTPRoutes may
		// attach. Naming them is the boundary: a project cannot route a
		// name it was not given, and cannot be routed to from a
		// namespace nobody listed.
		RouteNamespaces []string `yaml:"route_namespaces"`
		// CrossCluster marks a group whose clients dial it from ANOTHER
		// cluster's VPC. VPC peering carries VPC CIDRs and not service
		// CIDRs, so such a group cannot be reached on a private
		// exposure's pinned ClusterIP: its cluster keeps a load balancer
		// in front of the fleet (PrivateEntryNeedsLoadBalancer, which is
		// what renders privateEntry.service), and its names stay out of
		// the A records the dns stack points at that ClusterIP.
		//
		// A claim about who dials, not about which cluster runs the
		// workload -- and nothing on a cloudflare-tunnel exposure, where
		// there is no address to reach, so it is refused there.
		CrossCluster bool `yaml:"cross_cluster,omitempty"`
		// RecordElsewhere says this private name's DNS record is written
		// by another stack, so the `dns` stack leaves it alone.
		//
		// A name has ONE record. A group moving onto the private entry
		// still answers at the address its old record names -- a
		// dedicated load balancer's, written by that component's own
		// Pulumi stack -- and Route53 will not hold an A record beside a
		// CNAME of the same name anyway: the `dns` stack would fail on
		// every apply rather than take the name over. So the catalog row
		// comes first (listener, certificate, route, all reachable at the
		// private entry's address with the name sent as SNI), this flag
		// keeps the live record where it is while the new path is
		// verified, and the cutover deletes the old load balancer and its
		// record and drops this line: one name, one owner, at every
		// moment.
		//
		// cross_cluster implies the same thing for a different reason
		// (its record points at a load balancer the ClusterIP cannot
		// replace, ever), so the two are refused together: one says
		// "not yet", the other "never".
		RecordElsewhere bool `yaml:"record_elsewhere,omitempty"`
		// BackendTLS asks for a BackendTLSPolicy: the hop from the
		// gateway to the application is verified, not merely encrypted.
		BackendTLS bool `yaml:"backend_tls,omitempty"`
		// ListenerSet is how far the group has moved onto its own
		// ListenerSet (the gateway-groups chart) on this cluster. Off is
		// the state most groups are still in: the row records the claim,
		// and a per-component Gateway or the shared wildcard still
		// answers for it.
		//
		// A setting rather than "every row renders", because a
		// ListenerSet is only half of a cutover. The group's routes have
		// to name it as their parent, and the tunnel has to send the
		// group's exact name as origin SNI; rendering
		// listeners for every row at once would put a second listener
		// beside each live one with no route on it. Setting it is the
		// per-group step.
		ListenerSet ListenerSetMode `yaml:"listener_set,omitempty"`
	}

	// ListenerSetMode is a group's listener_set: false or true.
	ListenerSetMode int

	// Domain is one name a group answers to. In YAML it is a hostname,
	// or a mapping that also names the objects derived from it.
	//
	// The names exist for one reason: to keep an object that is already
	// live. Left empty they follow the gateway-groups chart's slug
	// (DomainSlug), which is what every new group should use. Set, they
	// let a group take over a listener and a Secret created under other
	// names, so Argo CD updates them in place instead of deleting one
	// and creating another -- and a recreated listener drops every
	// connection on it.
	Domain struct {
		Host string `yaml:"host"`
		// ListenerName is the ListenerSet listener's name, which is
		// also what a route's sectionName would point at.
		ListenerName string `yaml:"listener_name,omitempty"`
		// SecretName is the TLS Secret, and the Certificate writing it.
		SecretName string `yaml:"secret_name,omitempty"`
		// DNSElsewhere says this tunnel-served name's DNS record is
		// written by something other than the tunnel (another DNS
		// provider, or a record kept by hand in the zone): the tunnel
		// still gets the name's ingress rule, so traffic that reaches
		// the tunnel is routed, but the tunnel does not create the
		// name's CNAME. Only meaningful on a cloudflare-tunnel exposure,
		// whose records are the tunnel's own, so it is refused on any
		// other arrival.
		DNSElsewhere bool `yaml:"dns_elsewhere,omitempty"`
		// WAF is "off" to leave this name out of the Cloudflare managed
		// WAF the zone's `managed_waf: {enabled: true}` derives; absent
		// means the name is covered. The only value is "off": covering a
		// name is the default and needs no word. Only meaningful on a
		// cloudflare-tunnel exposure, the one whose names Cloudflare
		// proxies, so it is refused on any other arrival.
		WAF string `yaml:"waf,omitempty"`
		// WAFReason is why the name is not covered. Required with
		// waf: off and refused without it, so every gap in the WAF is
		// explained next to the name it is a gap for.
		WAFReason string `yaml:"waf_reason,omitempty"`
		// Paths, when present, makes the name a SHARED host: each entry
		// grants one path prefix to one of the group's route namespaces,
		// and nothing else on the host is anybody's. Absent, the name is
		// granted whole to every route namespace, as before paths
		// existed. See PathGrant for the rules and PathGrantedHosts for
		// reading the grants back.
		Paths []PathGrant `yaml:"paths,omitempty"`
	}
)

// WAFOff is the only value of Domain.WAF.
const WAFOff = "off"

// WAFCovered reports whether the managed WAF covers the domain's name.
func (d Domain) WAFCovered() bool { return d.WAF != WAFOff }

// The two listener_set states, in the order a group moves through them.
//
// There was a third between them, `attach`: a business group whose routes
// named a ListenerSet that did not exist yet while a shared wildcard
// Gateway still served them, so that the listener had its routes on its
// first reconcile. That Gateway is gone , so the half-step has
// no parent to keep the routes on and is retired with the rest of the
// pre-catalog vocabulary .
const (
	// ListenerSetOff (listener_set: false, or absent): no ListenerSet,
	// and the group's routes keep their default parent.
	ListenerSetOff ListenerSetMode = iota
	// ListenerSetServe (listener_set: true): the ListenerSet renders, and
	// the routes name it alone.
	ListenerSetServe
)

// UnmarshalYAML takes true or false. Anything else is refused rather than
// read as false: a typo would silently leave a group where it was, which
// in the middle of a cutover is the one thing nobody checks.
func (m *ListenerSetMode) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode && node.ShortTag() == "!!bool" {
		var serve bool
		if err := node.Decode(&serve); err != nil {
			return err
		}

		*m = ListenerSetOff
		if serve {
			*m = ListenerSetServe
		}

		return nil
	}

	return fmt.Errorf("line %d: listener_set is true or false, not %q", node.Line, node.Value)
}

// String is the value as the catalog spells it.
func (m ListenerSetMode) String() string {
	if m == ListenerSetServe {
		return "true"
	}

	return "false"
}

// UnmarshalYAML takes a bare hostname or a {host, listener_name,
// secret_name, dns_elsewhere, waf, waf_reason, paths} mapping. Unknown
// keys are refused here by hand: the loader's strict decoding does not
// reach into a custom unmarshaller, and a misspelled secret_name would
// otherwise mint a second Secret.
func (d *Domain) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.ScalarNode:
		return node.Decode(&d.Host)
	case yaml.MappingNode:
		for i := 0; i < len(node.Content); i += 2 {
			switch key := node.Content[i].Value; key {
			case "host", "listener_name", "secret_name", "dns_elsewhere", "waf", "waf_reason", "paths":
			default:
				return fmt.Errorf("line %d: domain field %q is not host, listener_name, secret_name, dns_elsewhere, waf, waf_reason or paths",
					node.Content[i].Line, key)
			}
		}

		type plain Domain

		return node.Decode((*plain)(d))
	default:
		return fmt.Errorf("line %d: a domain is a hostname or a {host, listener_name, secret_name, dns_elsewhere, waf, waf_reason, paths} mapping", node.Line)
	}
}

// ListenerNameOrDefault is the listener's name as the chart renders it.
func (d Domain) ListenerNameOrDefault() string {
	if d.ListenerName != "" {
		return d.ListenerName
	}

	return DomainSlug(d.Host)
}

// SecretNameOrDefault is the TLS Secret's name as the chart renders it.
func (d Domain) SecretNameOrDefault() string {
	if d.SecretName != "" {
		return d.SecretName
	}

	return DomainSlug(d.Host) + "-tls"
}

// Hosts is the group's hostnames, in catalog order.
func (g Group) Hosts() []string {
	out := make([]string, 0, len(g.Domains))
	for _, d := range g.Domains {
		out = append(out, d.Host)
	}

	return out
}

// Load reads the catalog from the given filesystem. Unknown keys are
// refused: a misspelled key must not silently leave a group where it was.
// A catalog with generated groups (project_groups) comes back unvalidated
// until WithProjects is called.
func Load(fsys fs.FS) (*Config, error) {
	data, err := fs.ReadFile(fsys, FileName)
	if err != nil {
		return nil, fmt.Errorf("cfg: read %s: %w", FileName, err)
	}

	var config Config

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&config); err != nil {
		return nil, fmt.Errorf("cfg: parse %s: %w", FileName, err)
	}

	// With generated groups the catalog is incomplete until WithProjects
	// has added them (a cluster may have no written group at all), so
	// WithProjects validates instead.
	for _, exposure := range config.Exposures {
		if exposure.ProjectGroups != nil {
			return &config, nil
		}
	}

	if err := config.Validate(); err != nil {
		return nil, err
	}

	return &config, nil
}

// Validate checks everything that can be checked without reading another
// file. The cross-file check is ValidateClusters, which the caller runs
// with what it has; the tunnel side is derived from this catalog
// (configcloudflare.Load), so it cannot disagree with it.
func (c *Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("cfg/gateways: version %d is not %d — this loader reads one catalog shape", c.Version, Version)
	}

	if len(c.Exposures) == 0 {
		return fmt.Errorf("cfg/gateways: no exposures — a catalog with no way in serves nothing")
	}

	for _, name := range sortedKeys(c.Exposures) {
		if err := c.Exposures[name].validate(name); err != nil {
			return err
		}
	}

	return c.validateDomainsAreUnique()
}

func (e Exposure) validate(name string) error {
	switch e.Arrival {
	case ArrivalCloudflareTunnel, ArrivalPrivateClusterIP, ArrivalNLB:
	case "":
		return fmt.Errorf("cfg/gateways: exposures[%s].arrival is required — how traffic arrives decides the Service shape", name)
	default:
		return fmt.Errorf(
			"cfg/gateways: exposures[%s].arrival: %q is not %q, %q or %q",
			name, e.Arrival, ArrivalCloudflareTunnel, ArrivalPrivateClusterIP, ArrivalNLB)
	}

	if len(e.CloudflareAccounts) > 0 && e.Arrival != ArrivalCloudflareTunnel {
		return fmt.Errorf(
			"cfg/gateways: exposures[%s].cloudflare_accounts: this exposure arrives by %s, and only a %s exposure has a Cloudflare account",
			name, e.Arrival, ArrivalCloudflareTunnel)
	}

	if dup := firstDuplicate(e.CloudflareAccounts); dup != "" {
		return fmt.Errorf("cfg/gateways: exposures[%s].cloudflare_accounts: %q is listed twice", name, dup)
	}

	if e.Class == "" {
		return fmt.Errorf("cfg/gateways: exposures[%s].class is required — a Gateway with no GatewayClass is never programmed", name)
	}

	if len(e.Clusters) == 0 {
		return fmt.Errorf("cfg/gateways: exposures[%s] runs on no cluster", name)
	}

	if err := e.ProjectGroups.validate(name, e); err != nil {
		return err
	}

	for _, cluster := range sortedKeys(e.Clusters) {
		if class := e.Clusters[cluster].Class; class != "" && class == e.Class {
			return fmt.Errorf(
				"cfg/gateways: exposures[%s].clusters[%s].class is %q, which is the exposure's own — an override that overrides nothing outlives the move it was for",
				name, cluster, class)
		}

		groups := e.Clusters[cluster].Groups
		if len(groups) == 0 {
			return fmt.Errorf(
				"cfg/gateways: exposures[%s].clusters[%s] has no groups — an exposure that admits no listener is a Gateway nothing can use",
				name, cluster)
		}

		for _, group := range sortedKeys(groups) {
			if err := groups[group].validate(name, cluster, group); err != nil {
				return err
			}

			// cross_cluster buys a group an address its clients can route
			// to. A tunnel-served name has no address at all -- cloudflared
			// dials out -- so the flag there would read as "this needs a
			// load balancer" and render none.
			if e.Arrival == ArrivalCloudflareTunnel && groups[group].CrossCluster {
				return fmt.Errorf(
					"cfg/gateways: exposures[%s].clusters[%s].groups[%s].cross_cluster: this exposure arrives by %s, "+
						"which has no address to reach; cross_cluster asks for one",
					name, cluster, group, ArrivalCloudflareTunnel)
			}

			// record_elsewhere is about the record the `dns` stack would
			// write for a private name. A tunnel-served name's record is
			// the tunnel's CNAME, derived from this same catalog, and
			// nothing else ever writes it.
			if e.Arrival == ArrivalCloudflareTunnel && groups[group].RecordElsewhere {
				return fmt.Errorf(
					"cfg/gateways: exposures[%s].clusters[%s].groups[%s].record_elsewhere: this exposure arrives by %s, "+
						"whose records are the tunnel's own CNAMEs",
					name, cluster, group, ArrivalCloudflareTunnel)
			}

			// dns_elsewhere says the tunnel does not write the name's
			// CNAME. On an exposure that is not a tunnel there is no
			// tunnel CNAME to leave out, so the flag would read as a
			// promise nothing keeps.
			if e.Arrival != ArrivalCloudflareTunnel {
				for _, d := range groups[group].Domains {
					if d.DNSElsewhere {
						return fmt.Errorf(
							"cfg/gateways: exposures[%s].clusters[%s].groups[%s].domains[%s].dns_elsewhere: this exposure arrives by %s, "+
								"and only a %s exposure has a tunnel record to leave out",
							name, cluster, group, d.Host, e.Arrival, ArrivalCloudflareTunnel)
					}
				}
			}

			// waf says whether the zone's managed WAF covers a name
			// Cloudflare proxies. A name on any other arrival is not
			// proxied by Cloudflare, so there is nothing to opt out of.
			for _, d := range groups[group].Domains {
				if err := d.validateWAF(name, cluster, group, e.Arrival); err != nil {
					return err
				}
			}

			// cross_cluster AND record_elsewhere together is a real
			// combination, not a contradiction: the load balancer is
			// this cluster's shared private entry, and the RECORD is
			// another stack's. a service whose own stack has
			// written the name since before the entry was shared is the case, and the dns stack must not write it
			// twice. Without this the two would fight over one record
			// and Route53 would refuse the second.
		}
	}

	return nil
}

func (g Group) validate(exposure, cluster, name string) error {
	where := fmt.Sprintf("exposures[%s].clusters[%s].groups[%s]", exposure, cluster, name)

	if len(g.Domains) == 0 {
		return fmt.Errorf("cfg/gateways: %s.domains is empty — a listener with no hostname matches nothing", where)
	}

	if len(g.RouteNamespaces) == 0 {
		return fmt.Errorf(
			"cfg/gateways: %s.route_namespaces is empty — a group nothing may attach to is a certificate and no traffic",
			where)
	}

	for _, domain := range g.Domains {
		if err := validateDomain(where, domain.Host); err != nil {
			return err
		}

		if err := g.validateDomainNames(where, domain); err != nil {
			return err
		}
	}

	if dup := firstDuplicate(g.Hosts()); dup != "" {
		return fmt.Errorf("cfg/gateways: %s.domains: %q is listed twice", where, dup)
	}

	listeners := make([]string, 0, len(g.Domains))
	secrets := make([]string, 0, len(g.Domains))

	for _, domain := range g.Domains {
		listeners = append(listeners, domain.ListenerNameOrDefault())
		secrets = append(secrets, domain.SecretNameOrDefault())
	}

	// Two domains of one group on one listener name is a ListenerSet the
	// API server rejects; on one Secret it is two Certificates fighting
	// over it. Both only happen through a name override.
	if dup := firstDuplicate(listeners); dup != "" {
		return fmt.Errorf("cfg/gateways: %s.domains: listener name %q is used twice", where, dup)
	}

	if dup := firstDuplicate(secrets); dup != "" {
		return fmt.Errorf("cfg/gateways: %s.domains: secret name %q is used twice", where, dup)
	}

	if dup := firstDuplicate(g.RouteNamespaces); dup != "" {
		return fmt.Errorf("cfg/gateways: %s.route_namespaces: %q is listed twice", where, dup)
	}

	return g.validatePaths(where)
}

// validateDomainNames checks a domain's name overrides. They are object
// names, so they have to be DNS labels; and they mean nothing on a group
// no ListenerSet serves, where writing one would only look like it had
// taken effect.
func (g Group) validateDomainNames(where string, d Domain) error {
	if d.ListenerName == "" && d.SecretName == "" {
		return nil
	}

	// Allowed on attach: they name what the next state renders, and a
	// group takes a live listener over by them on its first serve.
	if g.ListenerSet == ListenerSetOff {
		return fmt.Errorf(
			"cfg/gateways: %s.domains[%s]: listener_name and secret_name name ListenerSet objects, and this group is not listener_set",
			where, d.Host)
	}

	for _, field := range []struct{ key, value string }{
		{"listener_name", d.ListenerName},
		{"secret_name", d.SecretName},
	} {
		if field.value != "" && !dnsLabel.MatchString(field.value) {
			return fmt.Errorf("cfg/gateways: %s.domains[%s].%s: %q is not a DNS label", where, d.Host, field.key, field.value)
		}
	}

	return nil
}

// dnsLabel is RFC 1123, which both a listener name and a Secret name
// satisfy when they are this.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)

// validateDomain rejects the shapes that look like a hostname and are
// not. Each of these renders a listener that silently matches nothing,
// which reads as "the app is down" rather than as a bad catalog entry.
func validateDomain(where, domain string) error {
	switch {
	case domain == "":
		return fmt.Errorf("cfg/gateways: %s.domains contains an empty entry", where)
	case strings.Contains(domain, "://"):
		return fmt.Errorf("cfg/gateways: %s.domains: %q is a URL, not a hostname", where, domain)
	case strings.Contains(domain, "/"):
		return fmt.Errorf("cfg/gateways: %s.domains: %q has a path; a listener matches a host, not a path", where, domain)
	case strings.Contains(domain, ":"):
		return fmt.Errorf("cfg/gateways: %s.domains: %q carries a port; the exposure decides the port", where, domain)
	case domain != strings.ToLower(domain):
		return fmt.Errorf("cfg/gateways: %s.domains: %q is not lowercase; SNI comparison is", where, domain)
	case strings.Contains(strings.TrimPrefix(domain, "*."), "*"):
		return fmt.Errorf(
			"cfg/gateways: %s.domains: %q — a wildcard is only ever the leftmost label",
			where, domain)
	case !strings.Contains(domain, "."):
		return fmt.Errorf("cfg/gateways: %s.domains: %q is a single label, not a hostname", where, domain)
	}

	return nil
}

// validateDomainsAreUnique refuses the same name on two groups. Two
// listeners for one hostname is not a preference the catalog can
// express: which one serves a request would be decided by whichever
// resource the controller resolved first, and that changes between
// restarts.
func (c *Config) validateDomainsAreUnique() error {
	type site struct{ exposure, cluster, group string }

	seen := map[string]site{}

	for _, exposure := range sortedKeys(c.Exposures) {
		clusters := c.Exposures[exposure].Clusters
		for _, cluster := range sortedKeys(clusters) {
			groups := clusters[cluster].Groups
			for _, group := range sortedKeys(groups) {
				for _, domain := range groups[group].Hosts() {
					// The same name on two CLUSTERS is a different
					// thing and a legitimate one: a per-cluster host
					// like console.<cluster>.example.com is unique
					// anyway, and a shared apex name served from two
					// clusters is a migration, not a mistake. Only a
					// collision within one cluster is ambiguous.
					key := cluster + "|" + domain
					if prev, ok := seen[key]; ok {
						return fmt.Errorf(
							"cfg/gateways: %q is claimed on %s by both exposures[%s].groups[%s] and exposures[%s].groups[%s] — "+
								"which listener serves it would depend on resolution order",
							domain, cluster, prev.exposure, prev.group, exposure, group)
					}

					seen[key] = site{exposure, cluster, group}
				}
			}
		}
	}

	return nil
}

// ValidateClusters refuses a catalog entry for a cluster that does not
// exist. Passed the cluster names rather than reading the cluster list, so
// this package does not depend on that one.
func (c *Config) ValidateClusters(known []string) error {
	set := map[string]bool{}
	for _, name := range known {
		set[name] = true
	}

	for _, exposure := range sortedKeys(c.Exposures) {
		for _, cluster := range sortedKeys(c.Exposures[exposure].Clusters) {
			if !set[cluster] {
				return fmt.Errorf(
					"cfg/gateways: exposures[%s].clusters[%s]: no such cluster in cfg/k8s.yaml",
					exposure, cluster)
			}
		}
	}

	return nil
}

// HealthHostname is an exposure's own health listener on one cluster: a
// SPECIFIC name in that cluster's origin zone, never a wildcard, because a
// wildcard would conflict with the tenants wildcard on the same merged
// fleet. It is a listener with a certificate of its own, so it belongs to
// the origin role's name list as much as any group's host does.
func HealthHostname(exposure, originZone string) string {
	return "gateway-health-" + exposure + "." + originZone
}

// HealthSecretName is the Secret behind HealthHostname's certificate.
func HealthSecretName(exposure string) string {
	return "gateway-health-" + exposure + "-tls"
}

// TunnelHosts is every name on cluster that arrives through its
// Cloudflare tunnel: each domain of each group, listener_set or not, on
// every cloudflare-tunnel exposure. It is the tunnel's hostname set;
// configcloudflare orders it into ingress rules.
//
// Every group counts, not only listener_set ones. A ListenerSet group
// needs its exact name as a rule of its own (cloudflared sends the
// matched rule's hostname as origin SNI), and a group still answered by
// a per-component Gateway or the shared wildcard listener is matched on
// its exact SNI all the same (Envoy falls back to a covering wildcard) --
// so a rule per row is what a flip needs, and harmless before it. The
// rule has to be applied before the flip, which is why it exists from
// the row's first commit. Sorted, so a caller that forgets to order
// still gets a stable list.
func (c *Config) TunnelHosts(cluster string) []string {
	var out []string

	for _, exposure := range c.ExposuresOn(cluster) {
		if c.Exposures[exposure].Arrival != ArrivalCloudflareTunnel {
			continue
		}

		for _, group := range c.Groups(exposure, cluster) {
			out = append(out, group.Hosts()...)
		}
	}

	sort.Strings(out)

	return out
}

// TunnelClusters is every cluster with a cloudflare-tunnel exposure,
// sorted: the clusters that need a tunnel.
func (c *Config) TunnelClusters() []string {
	set := map[string]bool{}

	for _, exposure := range c.Exposures {
		if exposure.Arrival != ArrivalCloudflareTunnel {
			continue
		}

		for cluster := range exposure.Clusters {
			set[cluster] = true
		}
	}

	return sortedKeys(set)
}

// Groups returns one cluster's groups on one exposure, in a stable
// order, so a render of the same catalog is byte-identical.
func (c *Config) Groups(exposure, cluster string) []NamedGroup {
	e, ok := c.Exposures[exposure]
	if !ok {
		return nil
	}

	cl, ok := e.Clusters[cluster]
	if !ok {
		return nil
	}

	out := make([]NamedGroup, 0, len(cl.Groups))
	for _, name := range sortedKeys(cl.Groups) {
		out = append(out, NamedGroup{Name: name, Group: cl.Groups[name]})
	}

	return out
}

// ExposuresOn returns the exposures present on one cluster, in a stable
// order.
func (c *Config) ExposuresOn(cluster string) []string {
	var out []string

	for _, name := range sortedKeys(c.Exposures) {
		if _, ok := c.Exposures[name].Clusters[cluster]; ok {
			out = append(out, name)
		}
	}

	return out
}

// PrivateEntryNeedsLoadBalancer reports whether this cluster's private
// entry keeps a load balancer in front of the fleet: exactly when one of
// its private-clusterip groups is marked cross_cluster.
//
// A load balancer is an hourly cost and an address nobody reads off cfg,
// so it is not something a cluster HAS -- it is something a client in
// another VPC needs, because peering carries VPC CIDRs and not service
// CIDRs. Every other private client (the tailnet, and anything inside
// this cluster's own VPC) reaches the pinned ClusterIP directly.
//
// This is the catalog's answer, so adding the first cross-cluster
// consumer to a cluster brings its load balancer back by rendering one
// row -- and nothing in the render names a cluster.
func (c *Config) PrivateEntryNeedsLoadBalancer(cluster string) bool {
	for _, exposure := range c.ExposuresOn(cluster) {
		if c.Exposures[exposure].Arrival != ArrivalPrivateClusterIP {
			continue
		}

		for _, group := range c.Groups(exposure, cluster) {
			if group.CrossCluster {
				return true
			}
		}
	}

	return false
}

// ClassOn is the GatewayClass an exposure's Gateway takes on one cluster:
// the cluster's override while a split is being rolled out, or the
// exposure's own. An unknown exposure or cluster answers with the empty
// string, which no Gateway is ever programmed with.
func (c *Config) ClassOn(exposure, cluster string) string {
	e, ok := c.Exposures[exposure]
	if !ok {
		return ""
	}

	if override := e.Clusters[cluster].Class; override != "" {
		return override
	}

	return e.Class
}

// Classes are every GatewayClass this cluster needs, sorted: one fleet per
// class, so the count is the count of data planes. More than one is the
// point of the split — a public-side overload cannot take the private way
// in with it.
func (c *Config) Classes(cluster string) []string {
	seen := map[string]bool{}

	for _, exposure := range c.ExposuresOn(cluster) {
		if class := c.ClassOn(exposure, cluster); class != "" {
			seen[class] = true
		}
	}

	return sortedKeys(seen)
}

// DomainSlug is the name a wildcard becomes where a Kubernetes object
// needs one. It matches the gateway-groups chart's own `groups.slug`, so
// a Certificate named here and a Certificate named there are the same
// object rather than two.
func DomainSlug(domain string) string {
	slug := strings.ReplaceAll(strings.TrimPrefix(domain, "*."), ".", "-")
	if strings.HasPrefix(domain, "*.") {
		return "wildcard-" + slug
	}

	return slug
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

func firstDuplicate(values []string) string {
	seen := map[string]bool{}

	for _, v := range values {
		if seen[v] {
			return v
		}

		seen[v] = true
	}

	return ""
}

// validateWAF holds a domain's waf and waf_reason to their rules: waf is
// "off" or absent, off needs a reason, a reason needs off, and only a
// cloudflare-tunnel exposure has a proxied name to opt out.
func (d Domain) validateWAF(exposure, cluster, group, arrival string) error {
	where := fmt.Sprintf("cfg/gateways: exposures[%s].clusters[%s].groups[%s].domains[%s]", exposure, cluster, group, d.Host)

	switch {
	case d.WAF != "" && d.WAF != WAFOff:
		return fmt.Errorf("%s.waf: %q is not \"off\"; covering a name is the default and is not written", where, d.WAF)
	case d.WAF == WAFOff && strings.TrimSpace(d.WAFReason) == "":
		return fmt.Errorf("%s.waf_reason: required with waf: off; say why the name is left out of the managed WAF", where)
	case d.WAF == "" && d.WAFReason != "":
		return fmt.Errorf("%s.waf_reason: only meaningful with waf: off", where)
	case d.WAF != "" && arrival != ArrivalCloudflareTunnel:
		return fmt.Errorf("%s.waf: this exposure arrives by %s, and only a %s exposure has a name Cloudflare proxies",
			where, arrival, ArrivalCloudflareTunnel)
	}

	return nil
}
