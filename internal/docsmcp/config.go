// Package docsmcp is the engine of cmd/docs-mcp serve: it pulls one OCI
// artifact holding a documentation pack (internal/docspack), checks it, and
// serves it to agents over MCP (Streamable HTTP) with four read-only tools.
//
// The server holds no authentication code. It listens on loopback and trusts
// whatever sits in front of it in the same pod: in the chart, the sluis
// resource-proxy sidecar, which checks the bearer token and writes the audit
// line. See docs/docs-mcp.md.
package docsmcp

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/truvity/edge/v4/internal/docspack"
	"github.com/truvity/edge/v4/internal/staticsite"
)

// Config is everything the server reads. FromEnv fills it from DOCS_MCP_*
// variables; tests build it directly.
type Config struct {
	// Addr is the listen address of the MCP endpoint (/mcp). It must be a
	// loopback address unless AllowNonLoopback is set: the server does not
	// authenticate.
	Addr             string
	AllowNonLoopback bool
	// OpsAddr is the listen address of /healthz, /readyz and /metrics. It is
	// where a kubelet probes, so it is not loopback by default; it serves
	// nothing of the MCP surface.
	OpsAddr string
	// AllowedHosts, when not empty, are the Host headers the MCP endpoint
	// answers (the public host the proxy forwards, and the loopback names).
	AllowedHosts []string

	// Dir serves a pack directory and skips the registry (tests, local use).
	Dir string
	// ContentDir is where the artifact is unpacked; a fresh sub-directory per
	// load, removed once the pack is in memory. Put it on a memory-backed
	// emptyDir.
	ContentDir string

	// ArtifactRef is registry/repository[:tag]; ArtifactDigest, when set,
	// wins over any tag in ArtifactRef. One of them must pin a digest.
	ArtifactRef    string
	ArtifactDigest string
	PlainHTTP      bool
	// RegistryAuth is "" or auto, dockerconfig, ecr or none, as in
	// static-site. DockerConfigDir is DOCKER_CONFIG.
	RegistryAuth    string
	DockerConfigDir string
	PullTimeout     time.Duration
	// MaxBytes caps the bytes of the pack, as pulled and as loaded.
	MaxBytes int64

	// MaxResponseBytes caps the text of one tool result.
	MaxResponseBytes int

	ShutdownDelay time.Duration
}

// Defaults returns the configuration with no environment applied.
func Defaults() Config {
	return Config{
		Addr:             "127.0.0.1:8081",
		OpsAddr:          ":9090",
		ContentDir:       "/var/run/pack",
		PullTimeout:      2 * time.Minute,
		MaxBytes:         docspack.DefaultMaxBytes,
		MaxResponseBytes: 128 << 10,
		ShutdownDelay:    5 * time.Second,
	}
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// FromEnv reads the configuration from getenv (os.Getenv in main).
//
//	DOCS_MCP_ADDR                        MCP listen address, loopback
//	                                     (default 127.0.0.1:8081)
//	DOCS_MCP_ALLOW_NON_LOOPBACK          boolean; accept a non-loopback ADDR
//	DOCS_MCP_OPS_ADDR                    /healthz, /readyz, /metrics (:9090)
//	DOCS_MCP_ALLOWED_HOSTS               comma list of Host headers the MCP
//	                                     endpoint answers; empty: any
//	DOCS_MCP_DIR                         serve this pack directory, no pull
//	DOCS_MCP_CONTENT_DIR                 unpack target
//	DOCS_MCP_ARTIFACT_REF, _ARTIFACT_DIGEST   what to pull
//	DOCS_MCP_PLAIN_HTTP                  registry without TLS
//	DOCS_MCP_REGISTRY_AUTH               auto (default), dockerconfig, ecr
//	                                     or none
//	DOCKER_CONFIG                        directory of the Docker config file
//	DOCS_MCP_PULL_TIMEOUT, _MAX_BYTES
//	DOCS_MCP_MAX_RESPONSE_BYTES          cap on one tool result
//	DOCS_MCP_SHUTDOWN_DELAY
func FromEnv(getenv func(string) string) (Config, error) {
	c := Defaults()
	var errs []error
	str := func(key string, dst *string) {
		if v := getenv("DOCS_MCP_" + key); v != "" {
			*dst = v
		}
	}
	boolean := func(key string, dst *bool) {
		if v := getenv("DOCS_MCP_" + key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("DOCS_MCP_%s: %q is not a boolean", key, v))
				return
			}
			*dst = b
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v := getenv("DOCS_MCP_" + key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				errs = append(errs, fmt.Errorf("DOCS_MCP_%s: %q is not a duration", key, v))
				return
			}
			*dst = d
		}
	}
	positive := func(key string, set func(int64)) {
		if v := getenv("DOCS_MCP_" + key); v != "" {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil || n <= 0 {
				errs = append(errs, fmt.Errorf("DOCS_MCP_%s: %q is not a positive integer", key, v))
				return
			}
			set(n)
		}
	}
	str("ADDR", &c.Addr)
	str("OPS_ADDR", &c.OpsAddr)
	str("DIR", &c.Dir)
	str("CONTENT_DIR", &c.ContentDir)
	str("ARTIFACT_REF", &c.ArtifactRef)
	str("ARTIFACT_DIGEST", &c.ArtifactDigest)
	str("REGISTRY_AUTH", &c.RegistryAuth)
	c.DockerConfigDir = getenv("DOCKER_CONFIG")
	boolean("ALLOW_NON_LOOPBACK", &c.AllowNonLoopback)
	boolean("PLAIN_HTTP", &c.PlainHTTP)
	dur("PULL_TIMEOUT", &c.PullTimeout)
	dur("SHUTDOWN_DELAY", &c.ShutdownDelay)
	positive("MAX_BYTES", func(n int64) { c.MaxBytes = n })
	positive("MAX_RESPONSE_BYTES", func(n int64) { c.MaxResponseBytes = int(min(n, 64<<20)) })
	for _, h := range strings.Split(getenv("DOCS_MCP_ALLOWED_HOSTS"), ",") {
		if h = strings.TrimSpace(h); h != "" {
			c.AllowedHosts = append(c.AllowedHosts, h)
		}
	}
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

// Validate reports every problem with the configuration.
func (c *Config) Validate() error {
	var errs []error
	switch {
	case c.Dir == "" && c.ArtifactRef == "":
		errs = append(errs, errors.New("one of DOCS_MCP_DIR and DOCS_MCP_ARTIFACT_REF is required"))
	case c.Dir != "" && c.ArtifactRef != "":
		errs = append(errs, errors.New("DOCS_MCP_DIR and DOCS_MCP_ARTIFACT_REF are exclusive"))
	}
	if c.ArtifactDigest != "" && !digestRE.MatchString(c.ArtifactDigest) {
		errs = append(errs, fmt.Errorf("artifact digest %q is not sha256:<64 hex>", c.ArtifactDigest))
	}
	if c.ArtifactRef != "" && c.ArtifactDigest == "" && !strings.Contains(c.ArtifactRef, "@sha256:") {
		errs = append(errs, errors.New("the pack is pulled by digest: set DOCS_MCP_ARTIFACT_DIGEST, or give DOCS_MCP_ARTIFACT_REF as repository@sha256:<digest>"))
	}
	switch c.RegistryAuth {
	case "", staticsite.RegistryAuthAuto, staticsite.RegistryAuthDockerConfig, staticsite.RegistryAuthECR, staticsite.RegistryAuthNone:
	default:
		errs = append(errs, fmt.Errorf("DOCS_MCP_REGISTRY_AUTH: %q is not auto, dockerconfig, ecr or none", c.RegistryAuth))
	}
	if !c.AllowNonLoopback {
		if err := requireLoopback(c.Addr); err != nil {
			errs = append(errs, err)
		}
	}
	if c.OpsAddr == c.Addr {
		errs = append(errs, errors.New("DOCS_MCP_ADDR and DOCS_MCP_OPS_ADDR are the same address"))
	}
	for _, h := range c.AllowedHosts {
		if h == "" || strings.ContainsAny(h, " /\t\r\n") {
			errs = append(errs, fmt.Errorf("allowed host %q is not a host name", h))
		}
	}
	if c.MaxBytes <= 0 || c.MaxResponseBytes <= 0 {
		errs = append(errs, errors.New("the byte limits must be positive"))
	}
	return errors.Join(errs...)
}

// requireLoopback refuses a listen address that is reachable from outside the
// pod: the server has no authentication of its own.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("DOCS_MCP_ADDR %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("DOCS_MCP_ADDR %q is not a loopback address: the server does not authenticate, "+
		"so it listens where only the sidecar in front of it can reach it (set DOCS_MCP_ALLOW_NON_LOOPBACK=true to override)", addr)
}

// PullConfig is the static-site configuration the shared OCI puller reads.
func (c *Config) PullConfig() staticsite.Config {
	sc := staticsite.Defaults()
	sc.ArtifactRef = c.ArtifactRef
	sc.ArtifactDigest = c.ArtifactDigest
	sc.PlainHTTP = c.PlainHTTP
	sc.RegistryAuth = c.RegistryAuth
	sc.DockerConfigDir = c.DockerConfigDir
	sc.PullTimeout = c.PullTimeout
	sc.MaxBytes = c.MaxBytes
	sc.MaxFiles = docspack.MaxDocs + 8
	return sc
}
