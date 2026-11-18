package docsmcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestFromEnvDefaultsAreLoopback(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"DOCS_MCP_ARTIFACT_REF": "registry.example.com/docs/pack", "DOCS_MCP_ARTIFACT_DIGEST": digest}))
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:8081", c.Addr)
	assert.Equal(t, ":9090", c.OpsAddr)
	assert.Empty(t, c.AllowedHosts)
	pc := c.PullConfig()
	assert.Equal(t, "registry.example.com/docs/pack", pc.ArtifactRef)
	assert.Equal(t, digest, pc.ArtifactDigest)
}

func TestFromEnvReadsEverything(t *testing.T) {
	c, err := FromEnv(env(map[string]string{
		"DOCS_MCP_ARTIFACT_REF":       "registry.example.com/docs/pack@" + digest,
		"DOCS_MCP_ADDR":               "[::1]:9000",
		"DOCS_MCP_OPS_ADDR":           ":9100",
		"DOCS_MCP_ALLOWED_HOSTS":      "docs.example.com, 127.0.0.1 ,,localhost",
		"DOCS_MCP_PLAIN_HTTP":         "true",
		"DOCS_MCP_REGISTRY_AUTH":      "ecr",
		"DOCS_MCP_PULL_TIMEOUT":       "30s",
		"DOCS_MCP_MAX_BYTES":          "1048576",
		"DOCS_MCP_MAX_RESPONSE_BYTES": "4096",
		"DOCKER_CONFIG":               "/etc/registry-auth",
	}))
	require.NoError(t, err)
	assert.Equal(t, []string{"docs.example.com", "127.0.0.1", "localhost"}, c.AllowedHosts)
	assert.True(t, c.PlainHTTP)
	assert.Equal(t, "ecr", c.RegistryAuth)
	assert.EqualValues(t, 1048576, c.MaxBytes)
	assert.Equal(t, 4096, c.MaxResponseBytes)
	assert.Equal(t, "/etc/registry-auth", c.DockerConfigDir)
}

func TestFromEnvRefuses(t *testing.T) {
	ref := map[string]string{"DOCS_MCP_ARTIFACT_REF": "registry.example.com/docs/pack", "DOCS_MCP_ARTIFACT_DIGEST": digest}
	with := func(k, v string) map[string]string {
		m := map[string]string{}
		for a, b := range ref {
			m[a] = b
		}
		m[k] = v
		return m
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		want string
	}{
		"nothing to serve":    {map[string]string{}, "required"},
		"both":                {with("DOCS_MCP_DIR", "/pack"), "exclusive"},
		"a tag, no digest":    {map[string]string{"DOCS_MCP_ARTIFACT_REF": "registry.example.com/docs/pack:main"}, "by digest"},
		"a bad digest":        {with("DOCS_MCP_ARTIFACT_DIGEST", "sha256:abc"), "sha256:<64 hex>"},
		"a public address":    {with("DOCS_MCP_ADDR", "0.0.0.0:8081"), "not a loopback"},
		"every interface":     {with("DOCS_MCP_ADDR", ":8081"), "not a loopback"},
		"a host name":         {with("DOCS_MCP_ADDR", "docs.example.com:8081"), "not a loopback"},
		"no port":             {with("DOCS_MCP_ADDR", "127.0.0.1"), "DOCS_MCP_ADDR"},
		"one port twice":      {with("DOCS_MCP_OPS_ADDR", "127.0.0.1:8081"), "same address"},
		"a bad auth":          {with("DOCS_MCP_REGISTRY_AUTH", "magic"), "not auto, dockerconfig, ecr or none"},
		"a bad boolean":       {with("DOCS_MCP_PLAIN_HTTP", "maybe"), "not a boolean"},
		"a bad limit":         {with("DOCS_MCP_MAX_BYTES", "-1"), "positive integer"},
		"a bad duration":      {with("DOCS_MCP_PULL_TIMEOUT", "soon"), "not a duration"},
		"a bad allowed host":  {with("DOCS_MCP_ALLOWED_HOSTS", "https://docs.example.com"), "not a host name"},
		"a bad response size": {with("DOCS_MCP_MAX_RESPONSE_BYTES", "0"), "positive integer"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := FromEnv(env(tc.env))
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.want)
		})
	}
}

func TestNonLoopbackNeedsTheOverride(t *testing.T) {
	c, err := FromEnv(env(map[string]string{"DOCS_MCP_DIR": "/pack", "DOCS_MCP_ADDR": ":8081", "DOCS_MCP_ALLOW_NON_LOOPBACK": "true"}))
	require.NoError(t, err)
	assert.Equal(t, ":8081", c.Addr)
}

func TestAllowHosts(t *testing.T) {
	assert.Equal(t, "docs.example.com", hostOnly("docs.example.com:443"))
	assert.Equal(t, "::1", hostOnly("[::1]:8081"))
	assert.Equal(t, "::1", hostOnly("[::1]"))
	assert.Equal(t, "docs.example.com", hostOnly("docs.example.com"))
}
