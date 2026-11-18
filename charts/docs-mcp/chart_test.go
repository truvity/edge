// Copyright 2026 Truvity B.V.. All rights reserved.
// SPDX-License-Identifier: MIT

// Package chart_test holds docs-mcp's pod to the shape its security claim
// rests on: the resource-proxy sidecar is the only container that accepts MCP
// from outside the pod, docs-mcp listens on loopback with no container port
// for it, and the Service reaches the sidecar's port alone. The account id of
// the identity check is built at run time: this repository is public and its
// leak canary rejects a literal 12-digit run.
package chart_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

const base = `
issuerURL: https://issuer.example.com
resourceURL: https://docs.example.com/mcp
proxy:
  image:
    tag: v1.0.0
    digest: sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
pack:
  artifact:
    ref: registry.example.com/docs/pack
    digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
networkPolicy:
  ingressFrom:
    - namespaceSelector:
        matchLabels:
          kubernetes.io/metadata.name: gateway
`

type object = map[string]any

func render(t *testing.T, values string) (string, error) {
	t.Helper()

	// Two files, so that a key of values replaces the one of base.
	dir := t.TempDir()
	baseFile, valuesFile := filepath.Join(dir, "base.yaml"), filepath.Join(dir, "values.yaml")
	require.NoError(t, os.WriteFile(baseFile, []byte(base), 0o600))
	require.NoError(t, os.WriteFile(valuesFile, []byte(values), 0o600))

	cmd := exec.CommandContext(t.Context(), "helm", "template", "x", ".", "--namespace", "ns", "-f", baseFile, "-f", valuesFile)
	out, err := cmd.CombinedOutput()

	return string(out), err
}

// objects parses the rendered documents by kind.
func objects(t *testing.T, rendered string) map[string]object {
	t.Helper()

	out := map[string]object{}

	for _, d := range strings.Split(rendered, "\n---") {
		var m object
		if yaml.Unmarshal([]byte(d), &m) != nil || m["kind"] == nil {
			continue
		}

		out[m["kind"].(string)] = m
	}

	return out
}

func at(t *testing.T, v any, path ...any) any {
	t.Helper()

	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(object)
			require.True(t, ok, "no object at %v", k)
			v = m[k]
		case int:
			s, ok := v.([]any)
			require.True(t, ok && k < len(s), "no item %d", k)
			v = s[k]
		}
	}

	return v
}

func containers(t *testing.T, objs object) map[string]object {
	t.Helper()

	out := map[string]object{}

	for _, c := range at(t, objs, "spec", "template", "spec", "containers").([]any) {
		m := c.(object)
		out[m["name"].(string)] = m
	}

	return out
}

func envOf(c object) map[string]string {
	out := map[string]string{}

	for _, e := range c["env"].([]any) {
		m := e.(object)
		out[m["name"].(string)], _ = m["value"].(string)
	}

	return out
}

func TestOnlyTheSidecarAcceptsMCPFromOutsideThePod(t *testing.T) {
	out, err := render(t, "")
	require.NoError(t, err, out)
	objs := objects(t, out)

	cs := containers(t, objs["Deployment"])
	require.Len(t, cs, 2)

	server, proxy := cs["docs-mcp"], cs["proxy"]
	env := envOf(server)
	assert.Equal(t, "127.0.0.1:8081", env["DOCS_MCP_ADDR"], "MCP on loopback")
	assert.Equal(t, "docs.example.com,127.0.0.1,localhost", env["DOCS_MCP_ALLOWED_HOSTS"])

	// The server declares its ops port only: nothing names the MCP port.
	ports := server["ports"].([]any)
	require.Len(t, ports, 1)
	assert.Equal(t, "ops", at(t, ports, 0, "name"))
	assert.Equal(t, 9090, at(t, ports, 0, "containerPort"))

	penv := envOf(proxy)
	assert.Equal(t, "http://127.0.0.1:8081/mcp", penv["UPSTREAM"])
	assert.Equal(t, "https://issuer.example.com", penv["ISSUER_URL"])
	assert.Equal(t, "https://docs.example.com/mcp", penv["RESOURCE_URL"])
	for k := range penv {
		assert.False(t, strings.HasPrefix(k, "OUTBOUND_"), "docs-mcp has no backend: %s", k)
	}
	assert.Equal(t, "ghcr.io/truvity/sluis/resource-proxy:v1.0.0@sha256:"+strings.Repeat("b", 64), proxy["image"])

	// The Service reaches the sidecar's port and nothing else.
	svcPorts := at(t, objs["Service"], "spec", "ports").([]any)
	require.Len(t, svcPorts, 1)
	assert.Equal(t, "http", at(t, svcPorts, 0, "targetPort"))
	proxyPorts := proxy["ports"].([]any)
	require.Len(t, proxyPorts, 1)
	assert.Equal(t, "http", at(t, proxyPorts, 0, "name"))
	assert.Equal(t, 8080, at(t, proxyPorts, 0, "containerPort"))
}

func TestThePodPassesPSARestricted(t *testing.T) {
	out, err := render(t, "")
	require.NoError(t, err, out)
	dep := objects(t, out)["Deployment"]

	pod := at(t, dep, "spec", "template", "spec", "securityContext")
	assert.Equal(t, true, at(t, pod, "runAsNonRoot"))
	assert.Equal(t, "RuntimeDefault", at(t, pod, "seccompProfile", "type"))
	assert.Equal(t, false, at(t, dep, "spec", "template", "spec", "automountServiceAccountToken"))

	for name, c := range containers(t, dep) {
		sc := at(t, c, "securityContext")
		assert.Equal(t, false, at(t, sc, "allowPrivilegeEscalation"), name)
		assert.Equal(t, true, at(t, sc, "readOnlyRootFilesystem"), name)
		assert.Equal(t, []any{"ALL"}, at(t, sc, "capabilities", "drop"), name)
	}
}

func TestTheNetworkPolicyAdmitsTheGatewayAndReachesDNSAndHTTPS(t *testing.T) {
	out, err := render(t, `
networkPolicy:
  extraEgress:
    - to:
        - ipBlock:
            cidr: 169.254.170.23/32
      ports:
        - port: 80
          protocol: TCP
`)
	require.NoError(t, err, out)
	np := objects(t, out)["NetworkPolicy"]

	ingress := at(t, np, "spec", "ingress").([]any)
	require.Len(t, ingress, 1, "the ops port is closed unless metricsFrom names a scraper")
	assert.Equal(t, 8080, at(t, ingress, 0, "ports", 0, "port"))

	egress := at(t, np, "spec", "egress").([]any)
	require.Len(t, egress, 3)
	assert.Equal(t, 53, at(t, egress, 0, "ports", 0, "port"))
	assert.Equal(t, 443, at(t, egress, 1, "ports", 0, "port"))
	assert.Len(t, at(t, egress, 1, "ports"), 1, "the issuer and the registry are both on 443")
	assert.Equal(t, "169.254.170.23/32", at(t, egress, 2, "to", 0, "ipBlock", "cidr"))
}

func TestTheNetworkPolicyOpensThePortsOfTheURLs(t *testing.T) {
	out, err := render(t, `
issuerURL: https://issuer.example.com:8443
pack:
  artifact:
    ref: registry.example.com:5000/docs/pack
`)
	require.NoError(t, err, out)
	egress := at(t, objects(t, out)["NetworkPolicy"], "spec", "egress").([]any)

	var ports []any
	for _, p := range at(t, egress, 1, "ports").([]any) {
		ports = append(ports, p.(object)["port"])
	}

	assert.ElementsMatch(t, []any{443, 8443, 5000}, ports)
}

func TestTheRouteMatchesTheResourceURLsPath(t *testing.T) {
	out, err := render(t, `
resourceURL: https://docs.example.com/agents/mcp/
httpRoute:
  enabled: true
  parentRefs:
    - {group: gateway.networking.k8s.io, kind: ListenerSet, name: mcp, namespace: gateway}
`)
	require.NoError(t, err, out)
	route := objects(t, out)["HTTPRoute"]

	rules := at(t, route, "spec", "rules").([]any)
	require.Len(t, rules, 3)

	want := [][2]string{
		{"PathPrefix", "/agents/mcp"},
		{"PathPrefix", "/.well-known/oauth-protected-resource/agents/mcp"},
		{"Exact", "/.well-known/oauth-protected-resource"},
	}
	for i, w := range want {
		assert.Equal(t, w[0], at(t, rules, i, "matches", 0, "path", "type"))
		assert.Equal(t, w[1], at(t, rules, i, "matches", 0, "path", "value"))
		assert.Equal(t, "0s", at(t, rules, i, "timeouts", "request"), "no request timeout")
		assert.Equal(t, 8080, at(t, rules, i, "backendRefs", 0, "port"))
		assert.Nil(t, at(t, rules, i, "filters"), "no filters")
	}
}

func TestNoRouteUnlessEnabled(t *testing.T) {
	out, err := render(t, "")
	require.NoError(t, err, out)
	assert.NotContains(t, objects(t, out), "HTTPRoute")
}

func TestPackDigestAndRegistryAuthReachTheServer(t *testing.T) {
	out, err := render(t, `
registry:
  auth: ecr
serviceAccount:
  name: docs
`)
	require.NoError(t, err, out)
	dep := objects(t, out)["Deployment"]
	env := envOf(containers(t, dep)["docs-mcp"])

	assert.Equal(t, "sha256:"+strings.Repeat("a", 64), env["DOCS_MCP_ARTIFACT_DIGEST"])
	assert.Equal(t, "ecr", env["DOCS_MCP_REGISTRY_AUTH"])
	assert.Equal(t, false, at(t, dep, "spec", "template", "spec", "automountServiceAccountToken"),
		"Pod Identity and IRSA project their own token: the ServiceAccount's is never mounted")
}

const identityValues = `
registry:
  auth: ecr
  ack:
    enabled: true
    clusterName: example
    repositoryArns:
      - arn:aws:ecr:eu-central-1:%[1]s:repository/example
%[2]s
serviceAccount:
  name: docs-mcp
`

func checkArgs(t *testing.T, rendered string) []any {
	t.Helper()

	for _, d := range strings.Split(rendered, "\n---") {
		var m object
		if yaml.Unmarshal([]byte(d), &m) != nil || m["kind"] != "Job" {
			continue
		}

		spec := m["spec"].(object)["template"].(object)["spec"].(object)
		args, _ := spec["containers"].([]any)[0].(object)["args"].([]any) // absent when nothing is asserted

		return args
	}

	t.Fatalf("no identity check Job in:\n%s", rendered)

	return nil
}

func TestTheIdentityIsStaticSiteInfras(t *testing.T) {
	account := strings.Repeat("7", 12)

	out, err := render(t, fmt.Sprintf(identityValues, account, "    accountId: \""+account+"\""))
	require.NoError(t, err, out)

	objs := objects(t, out)
	assert.NotContains(t, objs, "ServiceAccount", "the ServiceAccount is rendered after the association, by static-site-infra")
	assert.NotContains(t, objs, "Role")
	assert.NotContains(t, objs, "PodIdentityAssociation")

	args := checkArgs(t, out)
	require.GreaterOrEqual(t, len(args), 2)
	assert.Equal(t, []any{"-expect-account", account}, args[len(args)-2:])
}

func TestIdentityCheckWithoutAccountPassesNoFlag(t *testing.T) {
	out, err := render(t, fmt.Sprintf(identityValues, strings.Repeat("7", 12), ""))
	require.NoError(t, err, out)
	assert.NotContains(t, checkArgs(t, out), "-expect-account")
}

func TestIdentityCheckRefusesAShortAccount(t *testing.T) {
	out, err := render(t, fmt.Sprintf(identityValues, strings.Repeat("7", 12), "    accountId: \"12345\""))
	assert.Error(t, err, out)
}

func TestNoIdentityCheckWithoutAck(t *testing.T) {
	out, err := render(t, "")
	require.NoError(t, err, out)
	assert.NotContains(t, objects(t, out), "Job")
}
