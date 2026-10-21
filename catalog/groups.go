package catalog

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// The ListenerSet half of the model, derived from the catalog: the groups
// a cluster serves from their own ListenerSet, the exposure Gateways those
// attach to, the route grant each ListenerSet carries, and which project's
// routes move onto which ListenerSet. Only groups marked listener_set take
// part; every other row is still a claim some other Gateway answers for.

const (
	// The one route kind every group admits.
	routeGroup = "gateway.networking.k8s.io"
	routeKind  = "HTTPRoute"

	wildcardPrefix = "*."
)

type (
	// GroupCertificate is one group's certificate policy: the issuer, the
	// lifetimes and the key. It is what the chart's `certificate` value
	// takes.
	GroupCertificate struct {
		Issuer       string
		Duration     string
		RenewBefore  string
		KeyAlgorithm string
		KeySize      int
	}

	// ExposureValue is one exposure Gateway, as the gateway-fleet chart's
	// exposures.<name> needs it. Its only listener is the health name: a
	// Gateway with no listener has no proxies.
	ExposureValue struct {
		Name             string
		Class            string
		HealthHostname   string
		HealthSecretName string
	}

	// GroupValue is one group as the gateway-groups chart's groups.<name>
	// needs it. The ListenerSet takes the group's name.
	GroupValue struct {
		Name     string
		Exposure string
		// Parent is the Gateway the ListenerSet attaches to, by name. It is
		// the exposure's name for every exposure whose Gateway is derived
		// from the catalog, and the DeriveInput.PrivateEntry Gateway for the private
		// exposure, whose Gateway the caller owns.
		Parent        string
		Domains       []Domain
		AllowedRoutes AllowedRoutes
		// Certificate overrides the chart's default issuer for a group
		// whose names are not origin names. Nil on a tunnel-served group,
		// which takes the cluster's origin policy (GroupsValue.
		// DefaultCertificate).
		Certificate *GroupCertificate
	}

	// GroupsValue is one cluster's ListenerSet groups.
	GroupsValue struct {
		Exposures []ExposureValue
		Groups    []GroupValue
		// DefaultCertificate is every tunnel-served group's certificate
		// policy. DeriveGroups states none; the caller lays the cluster's
		// own over it.
		DefaultCertificate GroupCertificate
		// Namespace is where every ListenerSet and its Certificates live.
		Namespace string
	}

	// ListenerSetRef is the ListenerSet a project's routes name as their
	// parent.
	ListenerSetRef struct {
		Name      string
		Namespace string
		// ParentRefsSince is the project's ParentRefsSince, which the
		// caller holds the pinned chart to. Empty: every release takes
		// the parent.
		ParentRefsSince string
	}

	// DeriveInput is what DeriveGroups reads besides the catalog: facts of
	// the estate the library has no way to know.
	DeriveInput struct {
		Cluster string
		// OriginZone is the cluster's origin zone, under which each derived
		// exposure's health name lives. Empty refuses a cluster with a
		// derived exposure.
		OriginZone string
		// Projects are the projects whose namespaces are business
		// namespaces. A route namespace that is a project is business; any
		// other is platform.
		Projects []Project
		// PlatformConsumers names the platform groups that may take a
		// ListenerSet, and the one namespace each group's routes live in.
		// A platform group missing here has nobody to attach its route.
		PlatformConsumers map[string]string
		// BusinessGrantLabel is the coarse label every business namespace
		// carries. A business group that answers on a wildcard admits by
		// it: per-install hosts land on a wildcard, in namespaces the
		// catalog does not list.
		BusinessGrantLabel string
		// WildcardByName makes a business wildcard admit by name instead of
		// by BusinessGrantLabel: its own route namespaces plus
		// WildcardNamespaces. For an estate that grants by name only and
		// writes no coarse label; BusinessGrantLabel is then not read for a
		// wildcard.
		WildcardByName bool
		// WildcardNamespaces are the namespaces per-install hosts land in
		// (CI and employee installs), which no group lists. Read only with
		// WildcardByName.
		WildcardNamespaces []string
		// ListenerSetNamespace is the namespace of every ListenerSet and
		// of the exposure Gateways.
		ListenerSetNamespace string
		// ReservedExposure, when non-empty, is an exposure name the
		// derivation refuses because a Gateway of that name is written
		// elsewhere.
		ReservedExposure string
		// PrivateEntry is the cluster's shared private entry. The private
		// exposure is not derived: it is that entry, and its groups attach
		// to its Gateway and take its certificate policy. Nil on a cluster
		// with no entry, which refuses a private group.
		PrivateEntry *PrivateEntry
	}
)

// RouteKinds is the route kinds every group admits.
func RouteKinds() []RouteKind {
	return []RouteKind{{Group: routeGroup, Kind: routeKind}}
}

// BusinessRouteGrant is the coarse grant: it admits the routes of every
// namespace carrying label. No ListenerSet should select it except a
// business wildcard (see DeriveGroups).
func BusinessRouteGrant(label string) AllowedRoutes {
	return AllowedRoutes{
		AllowedNamespaces: AllowedNamespaces{
			From:     FromSelector,
			Selector: LabelSelector{MatchLabels: map[string]string{label: "true"}},
		},
		RouteKinds: RouteKinds(),
	}
}

// DeriveGroups reads one cluster's listener_set groups out of the catalog.
// It returns nil when the cluster has none.
//
// Each group's route grant follows from whose namespaces it names:
//
//   - A BUSINESS group (every route namespace is a project) admits exactly
//     its own namespaces by name, see NamespacesGrant. A business wildcard
//     is the exception: per-install hosts land on it, in namespaces the
//     catalog does not list, so it admits by DeriveInput.BusinessGrantLabel
//     -- or, with DeriveInput.WildcardByName, by name over its own
//     namespaces and DeriveInput.WildcardNamespaces.
//   - A PLATFORM group (no route namespace is a project) admits its one
//     namespace by name.
//   - A PRIVATE group naming one namespace admits that one by name, even
//     where it is a project's: the coarse label would let any project
//     attach a route to somebody else's private name.
//
// A group naming both business and platform namespaces is refused: one
// ListenerSet has one grant.
func (c *Config) DeriveGroups(in DeriveInput) (*GroupsValue, error) {
	exposures, groups, err := c.deriveGroups(in)
	if err != nil || len(groups) == 0 {
		return nil, err
	}

	return &GroupsValue{Exposures: exposures, Groups: groups, Namespace: in.ListenerSetNamespace}, nil
}

//nolint:gocyclo,funlen // one pass over exposures and their groups, in the order the checks read.
func (c *Config) deriveGroups(in DeriveInput) ([]ExposureValue, []GroupValue, error) {
	var (
		outExposures []ExposureValue
		outGroups    []GroupValue
	)

	projects := map[string]Project{}
	for _, project := range in.Projects {
		projects[project.Name] = project
	}

	// The ListenerSet takes the group's name, so two exposures with a
	// listener_set group of one name on one cluster would be one object.
	claimedBy := map[string]string{}

	for _, exposureName := range c.ExposuresOn(in.Cluster) {
		exposure := c.Exposures[exposureName]

		var groups []GroupValue

		for _, group := range c.Groups(exposureName, in.Cluster) {
			if group.ListenerSet == ListenerSetOff {
				continue
			}

			where := fmt.Sprintf("cfg/gateways: exposures[%s].clusters[%s].groups[%s]", exposureName, in.Cluster, group.Name)

			if other, ok := claimedBy[group.Name]; ok {
				return nil, nil, fmt.Errorf("%s: exposure %s has a listener_set group of the same name, and both would be ListenerSet %s/%s",
					where, other, in.ListenerSetNamespace, group.Name)
			}

			claimedBy[group.Name] = exposureName

			grant, err := routeGrant(in, projects, exposure.Arrival, group)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", where, err)
			}

			domains := make([]Domain, 0, len(group.Domains))
			for _, domain := range group.Domains {
				// Resolved here rather than left to the chart's default,
				// so the values say the object names outright and a
				// reviewer of a render diff sees a rename as a rename.
				domains = append(domains, Domain{
					Host:         domain.Host,
					ListenerName: domain.ListenerNameOrDefault(),
					SecretName:   domain.SecretNameOrDefault(),
					// The chart holds the group's own routes to these.
					Paths: domain.SortedPaths(),
				})
			}

			groups = append(groups, GroupValue{
				Name:          group.Name,
				Exposure:      exposureName,
				Domains:       domains,
				AllowedRoutes: grant,
			})
		}

		if len(groups) == 0 {
			continue
		}

		// THE PRIVATE EXPOSURE IS NOT DERIVED, and its groups still attach
		// to it. `private` is the caller's shared entry: one Gateway per
		// cluster with the pinned ClusterIP in front of it and the health
		// listener that keeps it alive. So this exposure contributes no
		// ExposureValue -- one would create a SECOND Gateway of the
		// exposure's name and leave the entry's own without its groups --
		// and each group names the entry's Gateway as its parent instead.
		//
		// The certificates differ with it: an origin name's leaf is the
		// in-cluster CA's, which nothing verifies but the tunnel; a
		// private name's is the private trust domain's, which every client
		// on the private network does verify.
		if exposure.Arrival == ArrivalPrivateClusterIP {
			if in.PrivateEntry == nil {
				return nil, nil, fmt.Errorf(
					"cfg/gateways: exposures[%s] on %s has listener_set groups, and the cluster has no private entry for them to attach to",
					exposureName, in.Cluster)
			}

			for i := range groups {
				groups[i].Parent = in.PrivateEntry.Names.Gateway
				groups[i].Certificate = in.PrivateEntry.GroupCertificate()
			}

			outGroups = append(outGroups, groups...)

			continue
		}

		switch {
		case in.ReservedExposure != "" && exposureName == in.ReservedExposure:
			return nil, nil, fmt.Errorf(
				"cfg/gateways: exposures[%s] on %s: %q is the shared fleet's own Gateway, and a catalog exposure of that name would replace it",
				exposureName, in.Cluster, exposureName)
		case exposure.Arrival != ArrivalCloudflareTunnel:
			// Not refused on principle: nothing derives such an
			// exposure's Gateway, health name or address, and guessing
			// one would render a Gateway nobody can reach. The private
			// exposure is the case that has an answer, above.
			return nil, nil, fmt.Errorf(
				"cfg/gateways: exposures[%s] on %s has listener_set groups, and only %s and %s exposures are derived so far (this one arrives by %s)",
				exposureName, in.Cluster,
				ArrivalCloudflareTunnel, ArrivalPrivateClusterIP, exposure.Arrival)
		case in.OriginZone == "":
			return nil, nil, fmt.Errorf(
				"cfg/gateways: exposures[%s] on %s has listener_set groups, and the cluster has no origin zone for the exposure's health name",
				exposureName, in.Cluster)
		}

		for i := range groups {
			groups[i].Parent = exposureName
		}

		// A SPECIFIC health name, never a wildcard: a wildcard here would
		// conflict with a group's wildcard listener on the same merged
		// fleet.
		outExposures = append(outExposures, ExposureValue{
			Name:             exposureName,
			Class:            exposure.Class,
			HealthHostname:   HealthHostname(exposureName, in.OriginZone),
			HealthSecretName: HealthSecretName(exposureName),
		})
		outGroups = append(outGroups, groups...)
	}

	return outExposures, outGroups, nil
}

// routeGrant decides one group's grant, and refuses a group whose routes
// nothing would move.
func routeGrant(in DeriveInput, projects map[string]Project, arrival string, group NamedGroup) (AllowedRoutes, error) {
	kinds := RouteKinds()

	// A PRIVATE GROUP NAMING ONE NAMESPACE ADMITS THAT ONE BY NAME, even
	// where the namespace is a project's. The coarse grant is a label every
	// project namespace carries, so on a private name it would let any
	// project attach a route to somebody else's admin console -- and these
	// listeners exist precisely because the name must not be reachable by
	// anyone who happens to be inside. The name label is the API server's,
	// immutable and unique, so this costs no rollout.
	//
	// Several namespaces would need a label somebody writes, which is the
	// coarse grant again: those fall through below.
	if arrival == ArrivalPrivateClusterIP && len(group.RouteNamespaces) == 1 {
		return NamespaceGrant(group.RouteNamespaces[0], kinds), nil
	}

	var business, platform []string

	for _, namespace := range group.RouteNamespaces {
		if _, ok := projects[namespace]; ok {
			business = append(business, namespace)
		} else {
			platform = append(platform, namespace)
		}
	}

	switch {
	case len(business) > 0 && len(platform) > 0:
		return AllowedRoutes{}, fmt.Errorf(
			"route namespaces %v are projects and %v are not; one ListenerSet has one grant, so business and platform namespaces cannot share a group",
			business, platform)

	case len(platform) > 0:
		namespace, ok := in.PlatformConsumers[group.Name]
		if !ok {
			return AllowedRoutes{}, fmt.Errorf(
				"route namespaces %v are not projects, and nothing moves a platform group named %q onto its ListenerSet (moved today: %v)",
				platform, group.Name, slices.Sorted(maps.Keys(in.PlatformConsumers)))
		}

		// A SHARED HOST'S GROUP ADMITS EVERY NAMESPACE HOLDING A PREFIX on
		// it, by name: one installation per namespace, each held to its
		// prefix by what reads the path grants (the chart's routes and the
		// admission policy), not by the listener, which matches a host and
		// never a path. Every route namespace holds one (validatePaths), so
		// this is exactly the group's own list. The consumer must be among
		// them, or nothing attaches the route this group exists for.
		if group.PathGranted() {
			if !slices.Contains(platform, namespace) {
				return AllowedRoutes{}, fmt.Errorf(
					"route namespaces %v: the %s group's routes are attached from namespace %s, which holds no path on its shared host",
					platform, group.Name, namespace)
			}

			return NamespacesGrant(platform, kinds), nil
		}

		if len(platform) != 1 || platform[0] != namespace {
			return AllowedRoutes{}, fmt.Errorf(
				"route namespaces %v: the %s group's routes live in namespace %s alone, and its grant admits exactly that namespace",
				platform, group.Name, namespace)
		}

		// Chosen over a per-group label because nothing has to write it
		// first: a label would be one more namespace change per group,
		// which must land and sync before the ListenerSet selecting on it,
		// on another clock. The name label is immutable and no other
		// namespace can carry it.
		return NamespaceGrant(namespace, kinds), nil
	}

	// A BUSINESS WILDCARD ADMITS BY THE COARSE LABEL. A business group that
	// answers on a wildcard is where per-install hosts land, however many
	// projects it lists: CI rings and employee installs. Those namespaces
	// are not the projects' own, so a name grant over the listed projects
	// would 404 every one of them. The coarse label is exactly the set that
	// may land here, and a wildcard listener cannot take an exact host from
	// its own listener, so the name-grant property holds where it matters:
	// every exact-host business listener still admits its own namespace by
	// name.
	//
	// AN ESTATE THAT GRANTS BY NAME ONLY (WildcardByName) writes no coarse
	// label, so selecting it would admit nobody and 404 every per-install
	// host. Its wildcard admits the per-install namespaces it names, plus
	// the group's own, by the immutable name label.
	if hasWildcardDomain(group.Domains) {
		if in.WildcardByName {
			names := slices.Concat(business, in.WildcardNamespaces)
			slices.Sort(names)

			return NamespacesGrant(slices.Compact(names), kinds), nil
		}

		return BusinessRouteGrant(in.BusinessGrantLabel), nil
	}

	// A BUSINESS GROUP ADMITS ITS OWN NAMESPACES BY NAME, never by the
	// coarse label: that label sits on every project, employee and CI
	// namespace, so a route in any of them could attach to any business
	// listener. One namespace is a plain match; several are an In
	// expression over exactly the namespaces the catalog lists.
	return NamespacesGrant(business, kinds), nil
}

// hasWildcardDomain reports whether any of a group's domains is a wildcard.
func hasWildcardDomain(domains []Domain) bool {
	for _, domain := range domains {
		if strings.HasPrefix(domain.Host, wildcardPrefix) {
			return true
		}
	}

	return false
}

// ProjectHost is one project's primary hostname and its additional
// hostnames, as the project's chart takes a parent list: one list for
// every route it renders.
type ProjectHost struct {
	Project  string
	Hostname string
	Surfaces []string
}

// ListenerSetsFor points each project at the ListenerSet that serves its
// primary hostname, if one does. The result is keyed by project name; a
// project no group serves has no entry.
//
// A project whose surfaces would land on a different group than its
// primary -- or on none while the primary moved -- cannot be expressed
// with one parent list, and is refused rather than half-moved: the
// surface would leave the wildcard and match no listener.
func (g *GroupsValue) ListenerSetsFor(hosts []ProjectHost, projects []Project) (map[string]ListenerSetRef, error) {
	if g == nil {
		return nil, nil
	}

	byHost := map[string]*GroupValue{}

	for i := range g.Groups {
		for _, domain := range g.Groups[i].Domains {
			byHost[domain.Host] = &g.Groups[i]
		}
	}

	out := map[string]ListenerSetRef{}

	for _, host := range hosts {
		group := byHost[host.Hostname]

		for _, surface := range host.Surfaces {
			if byHost[surface] != group {
				return nil, fmt.Errorf(
					"cfg/gateways: project %s serves %s and %s from different groups (%s, %s); its chart takes one parentRefs list, so both hosts move together",
					host.Project, host.Hostname, surface, groupName(group), groupName(byHost[surface]))
			}
		}

		if group == nil {
			continue
		}

		// A project moves by its hostname, which a group can serve without
		// naming the project's namespace; the project must still be one
		// the caller knows, or nothing holds its chart to the parent.
		project, known := findProject(projects, host.Project)
		if !known {
			return nil, fmt.Errorf(
				"cfg/gateways: group %s serves %s, and %s is not among the projects",
				group.Name, host.Hostname, host.Project)
		}

		out[host.Project] = ListenerSetRef{Name: group.Name, Namespace: g.Namespace, ParentRefsSince: project.ParentRefsSince}
	}

	return out, nil
}

func findProject(projects []Project, name string) (Project, bool) {
	for _, project := range projects {
		if project.Name == name {
			return project, true
		}
	}

	return Project{}, false
}

func groupName(group *GroupValue) string {
	if group == nil {
		return "no listener_set group"
	}

	return group.Name
}

// SortedGroups orders groups by name across exposures, which is what keeps
// a render stable (names are unique per cluster, see DeriveGroups).
func SortedGroups(groups []GroupValue) []GroupValue {
	out := slices.Clone(groups)
	slices.SortFunc(out, func(a, b GroupValue) int { return strings.Compare(a.Name, b.Name) })

	return out
}
