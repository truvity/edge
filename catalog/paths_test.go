package catalog_test

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/truvity/edge/v4/catalog"
)

// sharedFixture has one shared host (audit) beside one host granted whole
// (console), so every test reads both shapes from one catalog.
const sharedFixture = `
version: 1
exposures:
  public:
    class: edge
    arrival: cloudflare-tunnel
    clusters:
      dev:
        groups:
          audit:
            domains:
              - host: audit.dev.example.com
                paths:
                  - {prefix: /second, namespace: second-audit}
                  - {prefix: /first, namespace: first-audit}
            route_namespaces: [first-audit, second-audit]
            listener_set: true
          console:
            domains: [console.dev.example.com]
            route_namespaces: [console]
            listener_set: true
      prod:
        groups:
          audit:
            domains:
              - host: audit.prod.example.com
                paths:
                  - {prefix: /first, namespace: first-audit}
            route_namespaces: [first-audit]
            listener_set: true
`

func TestPathGrantedHosts(t *testing.T) {
	c := load(t, sharedFixture)

	got := c.PathGrantedHosts("dev")
	want := []catalog.HostPaths{{
		Host:     "audit.dev.example.com",
		Exposure: "public",
		Group:    "audit",
		Paths: []catalog.PathGrant{
			{Prefix: "/first", Namespace: "first-audit"},
			{Prefix: "/second", Namespace: "second-audit"},
		},
	}}

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PathGrantedHosts(dev) = %+v, want %+v (sorted by prefix, the whole host left out)", got, want)
	}

	if got := c.PathGrantedHosts("staging"); got != nil {
		t.Errorf("PathGrantedHosts(staging) = %+v, want nil for a cluster the catalog does not name", got)
	}
}

func TestPathPrefixes(t *testing.T) {
	c := load(t, sharedFixture)

	got, ok := c.PathPrefixes("dev", "audit.dev.example.com")
	if !ok {
		t.Fatal("audit.dev.example.com is a shared host")
	}

	if want := map[string]string{"first-audit": "/first", "second-audit": "/second"}; !reflect.DeepEqual(got, want) {
		t.Errorf("PathPrefixes = %v, want %v", got, want)
	}

	// A host granted whole has no per-namespace prefix: the answer is the
	// same as for a host nobody claims, and the same as before paths
	// existed.
	for _, host := range []string{"console.dev.example.com", "nowhere.dev.example.com", "audit.prod.example.com"} {
		if got, ok := c.PathPrefixes("dev", host); ok || got != nil {
			t.Errorf("PathPrefixes(dev, %s) = %v, %v; want nil, false", host, got, ok)
		}
	}
}

func TestDomainPrefixByNamespace(t *testing.T) {
	whole := catalog.Domain{Host: "console.example.com"}
	if whole.PathGranted() || whole.PrefixByNamespace() != nil {
		t.Errorf("a domain without paths is granted whole: %+v", whole)
	}

	shared := catalog.Domain{Host: "audit.example.com", Paths: []catalog.PathGrant{{Prefix: "/a", Namespace: "a"}}}
	if got := shared.PrefixByNamespace(); !reflect.DeepEqual(got, map[string]string{"a": "/a"}) {
		t.Errorf("PrefixByNamespace = %v", got)
	}
}

func TestPrefixCovers(t *testing.T) {
	for _, tc := range []struct {
		prefix, path string
		want         bool
	}{
		{"/a", "/a", true},
		{"/a", "/a/", true},
		{"/a", "/a/b", true},
		{"/a", "/ab", false},
		{"/a", "/", false},
		{"/a/b", "/a", false},
		{"/a/b", "/a/b/c", true},
	} {
		if got := catalog.PrefixCovers(tc.prefix, tc.path); got != tc.want {
			t.Errorf("PrefixCovers(%s, %s) = %v, want %v", tc.prefix, tc.path, got, tc.want)
		}
	}

	if !catalog.PrefixesOverlap("/a/b", "/a") || catalog.PrefixesOverlap("/a", "/ab") {
		t.Error("PrefixesOverlap: /a and /a/b nest, /a and /ab do not")
	}
}

// sharedGroup is one group on one cluster, with the domain lines given.
func sharedGroup(domains, namespaces string) string {
	return `
version: 1
exposures:
  public:
    class: edge
    arrival: cloudflare-tunnel
    clusters:
      dev:
        groups:
          audit:
            domains:
` + domains + `
            route_namespaces: ` + namespaces + `
`
}

func TestPathGrantRefusals(t *testing.T) {
	const two = "[a, b]"

	for name, tc := range map[string]struct {
		domains, namespaces, want string
	}{
		"nested prefixes": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a}, {prefix: /a/b, namespace: b}]",
			two, "prefix /a/b overlaps /a (paths[0], granted to a)",
		},
		"nested the other way": {
			"              - host: audit.example.com\n                paths: [{prefix: /a/b, namespace: a}, {prefix: /a, namespace: b}]",
			two, "prefix /a overlaps /a/b",
		},
		"prefix twice": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a}, {prefix: /a, namespace: b}]",
			two, "prefix /a is granted twice on this host",
		},
		"no leading slash": {
			"              - host: audit.example.com\n                paths: [{prefix: a, namespace: a}]",
			"[a]", `"a" does not start with /`,
		},
		"trailing wildcard": {
			"              - host: audit.example.com\n                paths: [{prefix: \"/a/*\", namespace: a}]",
			"[a]", "ends in a wildcard",
		},
		"glob wildcard": {
			"              - host: audit.example.com\n                paths: [{prefix: \"/a*\", namespace: a}]",
			"[a]", "ends in a wildcard",
		},
		"inner wildcard": {
			"              - host: audit.example.com\n                paths: [{prefix: \"/a/*/b\", namespace: a}]",
			"[a]", "is not a path of non-empty segments",
		},
		"trailing slash": {
			"              - host: audit.example.com\n                paths: [{prefix: /a/, namespace: a}]",
			"[a]", "ends in /",
		},
		"whole host": {
			"              - host: audit.example.com\n                paths: [{prefix: /, namespace: a}]",
			"[a]", "/ is the whole host",
		},
		"uppercase": {
			"              - host: audit.example.com\n                paths: [{prefix: /A, namespace: a}]",
			"[a]", "is not lowercase",
		},
		"dot segment": {
			"              - host: audit.example.com\n                paths: [{prefix: /a/.., namespace: a}]",
			"[a]", `has a ".." segment`,
		},
		"no prefix": {
			"              - host: audit.example.com\n                paths: [{namespace: a}]",
			"[a]", ".prefix is required",
		},
		"no namespace": {
			"              - host: audit.example.com\n                paths: [{prefix: /a}]",
			"[a]", ".namespace is required",
		},
		"namespace not granted routes": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a}, {prefix: /c, namespace: c}]",
			"[a]", `"c" is not in the group's route_namespaces [a]`,
		},
		"namespace twice on a host": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a}, {prefix: /b, namespace: a}]",
			"[a]", `namespace "a" already holds /a on this host`,
		},
		"wildcard host": {
			"              - host: \"*.example.com\"\n                paths: [{prefix: /a, namespace: a}]",
			"[a]", "is a wildcard",
		},
		"shared and whole hosts in one group": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a}]\n              - console.example.com",
			"[a]", "some domains have paths and some do not",
		},
		"route namespace without a prefix": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a}]",
			two, `"b" holds no path on any of the group's domains`,
		},
		"unknown key": {
			"              - host: audit.example.com\n                paths: [{prefix: /a, namespace: a, path_type: Exact}]",
			"[a]", `path grant field "path_type" is not prefix or namespace`,
		},
		"bare string": {
			"              - host: audit.example.com\n                paths: [/a]",
			"[a]", "a path grant is a {prefix, namespace} mapping",
		},
	} {
		t.Run(name, func(t *testing.T) {
			doc := sharedGroup(tc.domains, tc.namespaces)

			_, err := catalog.Load(fstest.MapFS{catalog.FileName: {Data: []byte(doc)}})
			if err == nil {
				t.Fatalf("loaded:\n%s", doc)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not say %q", err, tc.want)
			}
		})
	}
}

func TestPathGrantsAccepted(t *testing.T) {
	// Siblings that share leading characters but not a segment do not
	// overlap, and one shared host may serve as many namespaces as it has
	// prefixes.
	doc := sharedGroup(
		"              - host: audit.example.com\n                paths:\n"+
			"                  - {prefix: /a, namespace: a}\n"+
			"                  - {prefix: /ab, namespace: b}\n"+
			"                  - {prefix: /a-b/c.d_e~f, namespace: c}",
		"[a, b, c]")

	if _, err := catalog.Load(fstest.MapFS{catalog.FileName: {Data: []byte(doc)}}); err != nil {
		t.Fatal(err)
	}
}

func TestDeriveGroupsAdmitsEveryNamespaceOfASharedHost(t *testing.T) {
	c := load(t, sharedFixture)

	in := catalog.DeriveInput{
		Cluster:              "dev",
		OriginZone:           "dev.example.com",
		PlatformConsumers:    map[string]string{"audit": "first-audit", "console": "console"},
		ListenerSetNamespace: "gw-system",
	}

	got, err := c.DeriveGroups(in)
	if err != nil {
		t.Fatal(err)
	}

	audit := group(t, got, "audit")

	if want := catalog.NamespacesGrant([]string{"first-audit", "second-audit"}, catalog.RouteKinds()); !reflect.DeepEqual(audit.AllowedRoutes, want) {
		t.Errorf("audit grant = %+v, want both installations by name: %+v", audit.AllowedRoutes, want)
	}

	if want := []catalog.PathGrant{
		{Prefix: "/first", Namespace: "first-audit"},
		{Prefix: "/second", Namespace: "second-audit"},
	}; !reflect.DeepEqual(audit.Domains[0].Paths, want) {
		t.Errorf("audit paths = %+v, want the grants carried, sorted: %+v", audit.Domains[0].Paths, want)
	}

	// The whole host is untouched: one namespace, no paths.
	console := group(t, got, "console")
	if console.Domains[0].Paths != nil {
		t.Errorf("console paths = %+v, want none", console.Domains[0].Paths)
	}

	if want := catalog.NamespaceGrant("console", catalog.RouteKinds()); !reflect.DeepEqual(console.AllowedRoutes, want) {
		t.Errorf("console grant = %+v, want %+v", console.AllowedRoutes, want)
	}

	// The consumer attaching the group's route must hold a prefix on it.
	in.PlatformConsumers["audit"] = "elsewhere"

	_, err = c.DeriveGroups(in)
	if err == nil || !strings.Contains(err.Error(), "namespace elsewhere, which holds no path on its shared host") {
		t.Fatalf("DeriveGroups = %v, want the consumer refused", err)
	}
}
