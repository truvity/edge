// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

// Package chart_test holds static-site's identity check to the account it is
// told to expect. The id is built at run time: this repository is public and
// its leak canary rejects a literal 12-digit run.
package chart_test

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

const identityValues = `
registry:
  auth: ecr
  ack:
    enabled: true
    clusterName: example
    repositoryArns:
      - arn:aws:ecr:eu-central-1:%[1]s:repository/example
%[2]s
site:
  artifact:
    ref: registry.example.com/static/site
serviceAccount:
  name: static-site
`

func render(t *testing.T, values string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "helm", "template", "x", ".", "--namespace", "ns", "-f", "-")
	cmd.Stdin = strings.NewReader(values)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

func checkArgs(t *testing.T, rendered string) []any {
	t.Helper()

	for _, d := range strings.Split(rendered, "\n---") {
		var m map[string]any
		if yaml.Unmarshal([]byte(d), &m) != nil || m["kind"] != "Job" {
			continue
		}

		spec := m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)

		args, _ := spec["containers"].([]any)[0].(map[string]any)["args"].([]any) // absent when nothing is asserted

		return args
	}

	t.Fatalf("no identity check Job in:\n%s", rendered)

	return nil
}

func TestIdentityCheckExpectsTheAccount(t *testing.T) {
	account := strings.Repeat("7", 12)

	out, err := render(t, fmt.Sprintf(identityValues, account, "    accountId: \""+account+"\""))
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	args := checkArgs(t, out)
	want := []string{"-expect-account", account}

	if len(args) < 2 || args[len(args)-2] != want[0] || args[len(args)-1] != want[1] {
		t.Fatalf("args = %v, want them to end with %v", args, want)
	}
}

func TestIdentityCheckWithoutAccountPassesNoFlag(t *testing.T) {
	out, err := render(t, fmt.Sprintf(identityValues, strings.Repeat("7", 12), ""))
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}

	for _, a := range checkArgs(t, out) {
		if a == "-expect-account" {
			t.Fatalf("no accountId was set, but the check passes -expect-account")
		}
	}
}

func TestIdentityCheckRefusesAShortAccount(t *testing.T) {
	if out, err := render(t, fmt.Sprintf(identityValues, strings.Repeat("7", 12), "    accountId: \"12345\"")); err == nil {
		t.Fatalf("a short accountId rendered:\n%s", out)
	}
}

const routeValues = `
site:
  artifact:
    ref: registry.example.com/static/site
route:
%s
`

// routeOf is the HTTPRoute the chart renders, or nil.
func routeOf(t *testing.T, rendered string) map[string]any {
	t.Helper()

	for _, d := range strings.Split(rendered, "\n---") {
		var m map[string]any
		if yaml.Unmarshal([]byte(d), &m) == nil && m["kind"] == "HTTPRoute" {
			return m
		}
	}

	return nil
}

// The chart renders no route unless asked: a site behind an Ingress, or one
// whose route something else renders, must not get a second one.
func TestRouteIsOffByDefault(t *testing.T) {
	out, err := render(t, fmt.Sprintf(routeValues, "  enabled: false"))
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	if routeOf(t, out) != nil {
		t.Fatalf("a route is rendered with route.enabled false:\n%s", out)
	}
}

// The route ships with the Service it sends to: every path of every name, as
// the one rule `site`, on the parent the caller names. The backend's group,
// kind and weight are written out, so a controller that compares the live
// route with the rendered one sees no difference.
func TestRouteSendsEveryPathToTheSite(t *testing.T) {
	out, err := render(t, fmt.Sprintf(routeValues, `  enabled: true
  hostname: www.example.com
  hostnames: [example.com, www.example.com]
  parentRef:
    kind: ListenerSet
    name: www
    namespace: gateways
  annotations:
    example.com/access: anonymous
  labels:
    example.com/site: www`))
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	route := routeOf(t, out)
	if route == nil {
		t.Fatalf("no HTTPRoute in:\n%s", out)
	}

	meta := route["metadata"].(map[string]any)
	if meta["name"] != "x-static-site" || meta["namespace"] != "ns" {
		t.Errorf("the route is %v in %v, want the release's name in its namespace", meta["name"], meta["namespace"])
	}

	if got := meta["annotations"].(map[string]any)["example.com/access"]; got != "anonymous" {
		t.Errorf("the caller's annotation is %v", got)
	}

	if got := meta["labels"].(map[string]any)["example.com/site"]; got != "www" {
		t.Errorf("the caller's label is %v", got)
	}

	spec := route["spec"].(map[string]any)

	wantParent := map[string]any{"group": "gateway.networking.k8s.io", "kind": "ListenerSet", "name": "www", "namespace": "gateways"}
	if got := spec["parentRefs"].([]any); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(wantParent) {
		t.Errorf("parentRefs are %v, want one, %v", got, wantParent)
	}

	if got := fmt.Sprint(spec["hostnames"]); got != "[www.example.com example.com]" {
		t.Errorf("hostnames are %s, want the hostname first and each name once", got)
	}

	rules := spec["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("%d rules, want one", len(rules))
	}

	rule := rules[0].(map[string]any)
	if rule["name"] != "site" {
		t.Errorf("the rule is named %v: a policy targets it as `site`", rule["name"])
	}

	path := rule["matches"].([]any)[0].(map[string]any)["path"].(map[string]any)
	if path["type"] != "PathPrefix" || path["value"] != "/" {
		t.Errorf("the rule matches %v, want every path", path)
	}

	wantBackend := map[string]any{"group": "", "kind": "Service", "name": "x-static-site", "port": 80, "weight": 1}
	if got := rule["backendRefs"].([]any); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(wantBackend) {
		t.Errorf("backendRefs are %v, want one, %v", got, wantBackend)
	}
}

// A parent with a name alone is a Gateway of the route's own namespace.
func TestRouteParentDefaultsToAGateway(t *testing.T) {
	out, err := render(t, fmt.Sprintf(routeValues, "  enabled: true\n  hostname: www.example.com\n  parentRef:\n    name: public\n    sectionName: https"))
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	want := map[string]any{"group": "gateway.networking.k8s.io", "kind": "Gateway", "name": "public", "sectionName": "https"}
	if got := routeOf(t, out)["spec"].(map[string]any)["parentRefs"].([]any)[0]; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the parent is %v, want %v", got, want)
	}
}

// A route with no name to serve or no listener to attach to is refused at
// render: it would be accepted by nothing, silently.
func TestRouteNeedsAHostnameAndAParent(t *testing.T) {
	for name, tc := range map[string]struct{ values, want string }{
		"no hostname":        {"  enabled: true\n  parentRef:\n    name: public", "route.hostname is required"},
		"no parent":          {"  enabled: true\n  hostname: www.example.com", "route.parentRef.name is required"},
		"a parent, no name":  {"  enabled: true\n  hostname: www.example.com\n  parentRef:\n    namespace: gateways", "route.parentRef.name is required"},
		"an unknown key":     {"  enabled: true\n  hostname: www.example.com\n  parentRef:\n    name: public\n  rules: []", "rules"},
		"a name with a path": {"  enabled: true\n  hostname: www.example.com/app\n  parentRef:\n    name: public", "hostname"},
	} {
		out, err := render(t, fmt.Sprintf(routeValues, tc.values))
		if err == nil {
			t.Errorf("%s: rendered, want a refusal", name)

			continue
		}

		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: refused without naming %q:\n%s", name, tc.want, out)
		}
	}
}

const extraFilesValues = `
site:
  artifact:
    ref: registry.example.com/static/site
extraFiles:
  manifest.json: '{"a": 1}'
%s
`

const extraFilesWriter = `
extraContainers:
  - name: writer
    image: registry.example.com/writer:1.0.0
    volumeMounts:
      - name: extra-files
        mountPath: /etc/extra
        readOnly: true
`

// The files reach the writer and nobody else: one ConfigMap, a volume of the
// pod, and no mount of it in the server, which serves what it mounts.
func TestExtraFilesAreAVolumeTheServerDoesNotMount(t *testing.T) {
	out, err := render(t, fmt.Sprintf(extraFilesValues, extraFilesWriter))
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	var data map[string]any

	var pod map[string]any

	for _, d := range strings.Split(out, "\n---") {
		var m map[string]any
		if yaml.Unmarshal([]byte(d), &m) != nil {
			continue
		}

		meta, _ := m["metadata"].(map[string]any)

		switch {
		case m["kind"] == "ConfigMap" && meta["name"] == "x-static-site-extra-files":
			data, _ = m["data"].(map[string]any)
		case m["kind"] == "Deployment":
			pod = m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		}
	}

	if data["manifest.json"] != `{"a": 1}` {
		t.Fatalf("the ConfigMap does not hold the file as given: %v", data)
	}

	var volume bool

	for _, v := range pod["volumes"].([]any) {
		if v.(map[string]any)["name"] == "extra-files" {
			volume = true
		}
	}

	if !volume {
		t.Fatal("the pod has no volume extra-files")
	}

	for _, c := range pod["containers"].([]any) {
		c := c.(map[string]any)
		if c["name"] != "static-site" {
			continue
		}

		for _, m := range c["volumeMounts"].([]any) {
			if m.(map[string]any)["name"] == "extra-files" {
				t.Fatal("the server mounts extra-files")
			}
		}
	}
}

// Nobody reads the files without an extra container, so the render is refused.
func TestExtraFilesNeedAnExtraContainer(t *testing.T) {
	out, err := render(t, fmt.Sprintf(extraFilesValues, ""))
	if err == nil {
		t.Fatalf("rendered without an extra container:\n%s", out)
	}

	if !strings.Contains(out, "extraFiles needs extraContainers") {
		t.Fatalf("refused for another reason:\n%s", out)
	}
}

const flagsValues = `
site:
  artifact:
    ref: registry.example.com/static/site
registry:
  auth: ecr
serviceAccount:
  name: static-site
flags:
  appconfig:
    enabled: true
    application: shop-web
    environment: globex
    profile: flags
    region: eu-central-1
`

// The flags file needs nothing but flags.appconfig: the agent's manifest names
// the one configuration and the path the server reads, the server lists that
// file as live, and the agent runs as the server does, with no write but the
// live volume.
func TestFlagsFromAppConfigAreOneBlock(t *testing.T) {
	out, err := render(t, flagsValues)
	if err != nil {
		t.Fatalf("render: %v\n%s", err, out)
	}

	var manifest string

	var pod map[string]any

	for _, d := range strings.Split(out, "\n---") {
		var m map[string]any
		if yaml.Unmarshal([]byte(d), &m) != nil {
			continue
		}

		meta, _ := m["metadata"].(map[string]any)

		switch {
		case m["kind"] == "ConfigMap" && meta["name"] == "x-static-site-flags":
			manifest, _ = m["data"].(map[string]any)["manifest.json"].(string)
		case m["kind"] == "Deployment":
			pod = m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
		}
	}

	if want := `{"shop-web:globex:flags":{"writeTo":{"path":"/var/run/live/flags.json"}}}`; manifest != want {
		t.Fatalf("the manifest is %q, want %q", manifest, want)
	}

	env := func(c map[string]any, name string) any {
		for _, e := range c["env"].([]any) {
			if e.(map[string]any)["name"] == name {
				return e.(map[string]any)["value"]
			}
		}

		return nil
	}

	seen := map[string]bool{}

	for _, c := range pod["containers"].([]any) {
		c := c.(map[string]any)
		seen[c["name"].(string)] = true

		switch c["name"] {
		case "static-site":
			if got := env(c, "STATIC_SITE_LIVE_FILES"); got != "flags.json" {
				t.Fatalf("the server's live files are %v", got)
			}
		case "appconfig-agent":
			if got := env(c, "PREFETCH_LIST"); got != "shop-web:globex:flags" {
				t.Fatalf("the agent prefetches %v", got)
			}

			sc := c["securityContext"].(map[string]any)
			if sc["readOnlyRootFilesystem"] != true || sc["runAsNonRoot"] != true || sc["allowPrivilegeEscalation"] != false {
				t.Fatalf("the agent's securityContext is %v", sc)
			}

			if !strings.Contains(c["image"].(string), "@sha256:") {
				t.Fatalf("the agent's image is not pinned by digest: %v", c["image"])
			}
		}
	}

	if !seen["static-site"] || !seen["appconfig-agent"] {
		t.Fatalf("containers: %v", seen)
	}
}
