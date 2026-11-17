// Package staticsite is the engine of cmd/static-site: it fetches one OCI
// artifact holding a built static site, unpacks it, and serves it with the
// caching, placeholder and safety rules a CDN in front of it relies on.
package staticsite

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultHashedPattern matches a file name that carries a content hash
// (app.3f9a1c2b.js, chunk-0123abcd.css): a dot, dash or underscore, eight or
// more hex digits, then the extension. Such a name changes whenever its
// content does, so it may be cached for ever.
const DefaultHashedPattern = `[.\-_][0-9a-fA-F]{8,}(\.[A-Za-z0-9]+)+$`

// Config is everything the server reads. FromEnv fills it from STATIC_SITE_*
// variables; tests build it directly.
type Config struct {
	// Addr is the listen address of the site, OpsAddr of /healthz, /readyz and
	// /metrics.
	Addr    string
	OpsAddr string

	// Dir serves a local directory and skips the registry (tests, local use).
	Dir string
	// ContentDir is where the artifact is unpacked; a fresh sub-directory per
	// load. Put it on a memory-backed emptyDir.
	ContentDir string

	// ArtifactRef is registry/repository[:tag]; ArtifactDigest, when set, wins
	// over any tag in ArtifactRef and pins the pull.
	ArtifactRef    string
	ArtifactDigest string
	PlainHTTP      bool
	// RegistryAuth is "" or auto (a Docker config file if one is configured,
	// else ECR through the AWS SDK for an ECR host, else anonymous),
	// dockerconfig, ecr or none. DockerConfigDir is DOCKER_CONFIG: when set,
	// a Docker config file is configured.
	RegistryAuth    string
	DockerConfigDir string
	PullTimeout     time.Duration
	MaxBytes        int64
	MaxFiles        int

	// HashedPattern selects the file names cached as immutable.
	HashedPattern *regexp.Regexp
	// HTMLMaxAge and StaleWhileRevalidate are the TTLs of everything that is
	// not hashed.
	HTMLMaxAge           time.Duration
	StaleWhileRevalidate time.Duration
	// CachePrivate sends Cache-Control: private, no-store on every response,
	// the redirects included. Shared caches then store nothing. The TTLs and
	// HashedPattern are unused while it is on. The server does not
	// authenticate: access control must sit in front of it.
	CachePrivate bool

	SPAFallback  bool
	NotFoundPage string // path inside the site, "" for a plain 404

	// ContentTypes sets the Content-Type of named paths ("/a/b"), over the
	// one the extension gives. It is for a file whose name carries no
	// extension but whose type a client insists on.
	ContentTypes map[string]string

	// Headers are added to every response; a value of "" removes a default.
	Headers map[string]string

	// Placeholders is the allowlist: only these names are substituted.
	Placeholders map[string]string
	// PlaceholderKeys are the names a deployment declares as expected (they
	// come from STATIC_SITE_PLACEHOLDERS_KEYS). With PlaceholderStrict, a
	// token for one of them that has no value fails the load.
	PlaceholderKeys []string
	// PlaceholderStrict makes a token the content carries fail the load when
	// its name is unknown or has no value, instead of staying as written.
	PlaceholderStrict bool

	// LiveDir is a directory of live files: files that are not in the
	// artifact, written by something beside the server, and re-read every
	// LiveInterval. Only the names in LiveFiles are served, each at the site
	// root (/name). A file is checked before it is served (LiveMaxBytes, and
	// a JSON file must parse); one that fails keeps its last good version.
	LiveDir      string
	LiveFiles    []string
	LiveInterval time.Duration
	LiveMaxBytes int64

	ShutdownDelay time.Duration
}

// DefaultHeaders are the security headers every response carries unless the
// configuration overrides them.
func DefaultHeaders() map[string]string {
	return map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "strict-origin-when-cross-origin",
	}
}

// Defaults returns the configuration with no environment applied.
func Defaults() Config {
	return Config{
		Addr:                 ":8080",
		OpsAddr:              ":8081",
		ContentDir:           "/var/run/site",
		PullTimeout:          2 * time.Minute,
		MaxBytes:             512 << 20,
		MaxFiles:             50000,
		HashedPattern:        regexp.MustCompile(DefaultHashedPattern),
		HTMLMaxAge:           60 * time.Second,
		StaleWhileRevalidate: 300 * time.Second,
		NotFoundPage:         "404.html",
		Headers:              DefaultHeaders(),
		LiveInterval:         DefaultLiveInterval,
		LiveMaxBytes:         DefaultLiveMaxBytes,
		ShutdownDelay:        5 * time.Second,
	}
}

var placeholderName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

// safeValue is the character set a placeholder value may use. It is the
// reason a value cannot break out of an attribute, a string or a stylesheet;
// the per-type escaping is a second line behind it.
var safeValue = regexp.MustCompile(`^[A-Za-z0-9 ._:/@#+=,~-]{0,512}$`)

// mediaType is what a configured Content-Type may be: a plain type/subtype,
// with the one parameter a text type needs. It cannot carry a header break.
var mediaType = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]*/[a-z0-9][a-z0-9.+-]*(; charset=utf-8)?$`)

// FromEnv reads the configuration from getenv (os.Getenv in main).
//
//	STATIC_SITE_ADDR, STATIC_SITE_OPS_ADDR       listen addresses
//	STATIC_SITE_DIR                              serve this directory, no pull
//	STATIC_SITE_CONTENT_DIR                      unpack target
//	STATIC_SITE_ARTIFACT_REF, _ARTIFACT_DIGEST   what to pull
//	STATIC_SITE_PLAIN_HTTP                       registry without TLS
//	STATIC_SITE_REGISTRY_AUTH                    auto (default), dockerconfig,
//	                                             ecr or none
//	DOCKER_CONFIG                                directory of the Docker config
//	                                             file (a mounted pull secret)
//	STATIC_SITE_PULL_TIMEOUT, _MAX_BYTES, _MAX_FILES
//	STATIC_SITE_HASHED_PATTERN                   regexp, immutable file names
//	STATIC_SITE_HTML_MAX_AGE, _STALE_WHILE_REVALIDATE   durations
//	STATIC_SITE_CACHE_PRIVATE                    boolean; every response is
//	                                             private, no-store
//	STATIC_SITE_SPA_FALLBACK, _NOT_FOUND_PAGE
//	STATIC_SITE_CONTENT_TYPES                    JSON object, path -> type,
//	                                             for files the extension
//	                                             does not type
//	STATIC_SITE_HEADERS                          JSON object, name -> value
//	STATIC_SITE_PLACEHOLDERS_FILE                JSON object, name -> value
//	STATIC_SITE_PLACEHOLDER_NAMES                comma list; the value of NAME
//	                                             is STATIC_SITE_PLACEHOLDER_<NAME>
//	STATIC_SITE_PLACEHOLDERS_DIR                 a directory, one file per
//	                                             name, as a mounted Secret is
//	STATIC_SITE_PLACEHOLDERS_KEYS                comma list of expected names
//	STATIC_SITE_PLACEHOLDERS_OPTIONAL_KEYS       comma list; absent means ""
//	STATIC_SITE_PLACEHOLDERS_STRICT              unknown or valueless tokens
//	                                             fail the load
//	STATIC_SITE_LIVE_DIR                         a directory of live files
//	STATIC_SITE_LIVE_FILES                       comma list of the file names
//	                                             served from it, at the root
//	STATIC_SITE_LIVE_INTERVAL                    how often they are re-read
//	STATIC_SITE_LIVE_MAX_BYTES                   the largest one accepted
//	STATIC_SITE_SHUTDOWN_DELAY
func FromEnv(getenv func(string) string) (Config, error) {
	c := Defaults()
	var errs []error
	str := func(key string, dst *string) {
		if v := getenv("STATIC_SITE_" + key); v != "" {
			*dst = v
		}
	}
	dur := func(key string, dst *time.Duration) {
		if v := getenv("STATIC_SITE_" + key); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d < 0 {
				errs = append(errs, fmt.Errorf("STATIC_SITE_%s: %q is not a duration", key, v))
				return
			}
			*dst = d
		}
	}
	boolean := func(key string, dst *bool) {
		if v := getenv("STATIC_SITE_" + key); v != "" {
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Errorf("STATIC_SITE_%s: %q is not a boolean", key, v))
				return
			}
			*dst = b
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
	str("NOT_FOUND_PAGE", &c.NotFoundPage)
	boolean("PLAIN_HTTP", &c.PlainHTTP)
	boolean("SPA_FALLBACK", &c.SPAFallback)
	boolean("CACHE_PRIVATE", &c.CachePrivate)
	dur("PULL_TIMEOUT", &c.PullTimeout)
	dur("HTML_MAX_AGE", &c.HTMLMaxAge)
	dur("STALE_WHILE_REVALIDATE", &c.StaleWhileRevalidate)
	dur("SHUTDOWN_DELAY", &c.ShutdownDelay)
	str("LIVE_DIR", &c.LiveDir)
	c.LiveFiles = splitList(getenv("STATIC_SITE_LIVE_FILES"))
	dur("LIVE_INTERVAL", &c.LiveInterval)
	if v := getenv("STATIC_SITE_LIVE_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("STATIC_SITE_LIVE_MAX_BYTES: %q is not a positive integer", v))
		} else {
			c.LiveMaxBytes = n
		}
	}
	if v := getenv("STATIC_SITE_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("STATIC_SITE_MAX_BYTES: %q is not a positive integer", v))
		} else {
			c.MaxBytes = n
		}
	}
	if v := getenv("STATIC_SITE_MAX_FILES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			errs = append(errs, fmt.Errorf("STATIC_SITE_MAX_FILES: %q is not a positive integer", v))
		} else {
			c.MaxFiles = n
		}
	}
	if v := getenv("STATIC_SITE_HASHED_PATTERN"); v != "" {
		re, err := regexp.Compile(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("STATIC_SITE_HASHED_PATTERN: %w", err))
		} else {
			c.HashedPattern = re
		}
	}
	if v := getenv("STATIC_SITE_HEADERS"); v != "" {
		var h map[string]string
		if err := json.Unmarshal([]byte(v), &h); err != nil {
			errs = append(errs, fmt.Errorf("STATIC_SITE_HEADERS: %w", err))
		}
		for k, val := range h {
			c.Headers[k] = val
		}
	}
	if v := getenv("STATIC_SITE_CONTENT_TYPES"); v != "" {
		if err := json.Unmarshal([]byte(v), &c.ContentTypes); err != nil {
			errs = append(errs, fmt.Errorf("STATIC_SITE_CONTENT_TYPES: %w", err))
		}
	}
	c.Placeholders = map[string]string{}
	if f := getenv("STATIC_SITE_PLACEHOLDERS_FILE"); f != "" {
		data, err := os.ReadFile(f)
		if err != nil {
			errs = append(errs, fmt.Errorf("STATIC_SITE_PLACEHOLDERS_FILE: %w", err))
		} else if err := json.Unmarshal(data, &c.Placeholders); err != nil {
			errs = append(errs, fmt.Errorf("STATIC_SITE_PLACEHOLDERS_FILE: %w", err))
		}
	}
	boolean("PLACEHOLDERS_STRICT", &c.PlaceholderStrict)
	keys := splitList(getenv("STATIC_SITE_PLACEHOLDERS_KEYS"))
	optional := splitList(getenv("STATIC_SITE_PLACEHOLDERS_OPTIONAL_KEYS"))
	c.PlaceholderKeys = append(append([]string{}, keys...), optional...)
	if d := getenv("STATIC_SITE_PLACEHOLDERS_DIR"); d != "" {
		vals, err := readPlaceholderDir(d, c.PlaceholderKeys)
		if err != nil {
			errs = append(errs, fmt.Errorf("STATIC_SITE_PLACEHOLDERS_DIR: %w", err))
		}
		for k, v := range vals {
			c.Placeholders[k] = v
		}
	}
	for _, name := range optional {
		if _, ok := c.Placeholders[name]; !ok {
			c.Placeholders[name] = ""
		}
	}
	for _, name := range strings.Split(getenv("STATIC_SITE_PLACEHOLDER_NAMES"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := "STATIC_SITE_PLACEHOLDER_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
		v, ok := lookup(getenv, key)
		if !ok {
			errs = append(errs, fmt.Errorf("placeholder %q is allowlisted but %s is not set", name, key))
			continue
		}
		c.Placeholders[name] = v
	}
	if err := c.Validate(); err != nil {
		errs = append(errs, err)
	}
	return c, errors.Join(errs...)
}

// PlaceholderNames lists the names that have a value, sorted. For logs: it
// returns names only.
func (c *Config) PlaceholderNames() []string {
	names := make([]string, 0, len(c.Placeholders))
	for n := range c.Placeholders {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func splitList(s string) []string {
	var out []string
	for _, n := range strings.Split(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// readPlaceholderDir reads one value per declared name from dir, a file named
// as the placeholder (a mounted Secret). Only declared names are read, so a
// stray key in the Secret is never served, and a name that is not a plain
// placeholder name cannot reach outside dir. A missing file is not an error
// here: a declared name without a value fails the load only if the content
// uses it. One trailing newline is trimmed; the value is otherwise kept as
// written. Errors name the file, never its contents.
func readPlaceholderDir(dir string, names []string) (map[string]string, error) {
	out := map[string]string{}
	var errs []error
	for _, name := range names {
		if !placeholderName.MatchString(name) {
			errs = append(errs, fmt.Errorf("placeholder name %q is not [A-Za-z][A-Za-z0-9_-]{0,63}", name))
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("placeholder %q: %w", name, err))
			continue
		}
		out[name] = strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	}
	return out, errors.Join(errs...)
}

// lookup distinguishes an empty value from an unset variable through the
// getenv seam: an unset variable is the only "" it cannot tell apart, so an
// allowlisted name must carry a non-empty value or be given in the file.
func lookup(getenv func(string) string, key string) (string, bool) {
	v := getenv(key)
	return v, v != ""
}

// Validate reports every problem with the configuration.
func (c *Config) Validate() error {
	var errs []error
	if c.Dir == "" && c.ArtifactRef == "" {
		errs = append(errs, errors.New("one of STATIC_SITE_DIR and STATIC_SITE_ARTIFACT_REF is required"))
	}
	if c.Dir != "" && c.ArtifactRef != "" {
		errs = append(errs, errors.New("STATIC_SITE_DIR and STATIC_SITE_ARTIFACT_REF are exclusive"))
	}
	if c.ArtifactDigest != "" && !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(c.ArtifactDigest) {
		errs = append(errs, fmt.Errorf("artifact digest %q is not sha256:<64 hex>", c.ArtifactDigest))
	}
	switch c.RegistryAuth {
	case "", RegistryAuthAuto, RegistryAuthDockerConfig, RegistryAuthECR, RegistryAuthNone:
	default:
		errs = append(errs, fmt.Errorf("STATIC_SITE_REGISTRY_AUTH: %q is not auto, dockerconfig, ecr or none", c.RegistryAuth))
	}
	for name, v := range c.Placeholders {
		if !placeholderName.MatchString(name) {
			errs = append(errs, fmt.Errorf("placeholder name %q is not [A-Za-z][A-Za-z0-9_-]{0,63}", name))
		}
		if !safeValue.MatchString(v) {
			errs = append(errs, fmt.Errorf("placeholder %q: value has characters outside the safe set [A-Za-z0-9 ._:/@#+=,~-] or is longer than 512", name))
		}
	}
	errs = append(errs, c.validateLive()...)
	errs = append(errs, validateContentTypes(c)...)
	for k := range c.Headers {
		if k == "" || strings.ContainsAny(k, " \t\r\n:") {
			errs = append(errs, fmt.Errorf("header name %q is invalid", k))
		}
	}
	return errors.Join(errs...)
}

// validateLive checks the live-file settings. The names are an allowlist of
// what is served from a directory another process writes, so each must be a
// plain file name and the list must be explicit.
func (c *Config) validateLive() []error {
	var errs []error
	if (c.LiveDir == "") != (len(c.LiveFiles) == 0) {
		return append(errs, errors.New("STATIC_SITE_LIVE_DIR and STATIC_SITE_LIVE_FILES are set together or not at all"))
	}
	if c.LiveDir == "" {
		return nil
	}
	if len(c.LiveFiles) > maxLiveFiles {
		errs = append(errs, fmt.Errorf("STATIC_SITE_LIVE_FILES lists %d names; at most %d", len(c.LiveFiles), maxLiveFiles))
	}
	seen := map[string]bool{}
	for _, n := range c.LiveFiles {
		if !liveName.MatchString(n) {
			errs = append(errs, fmt.Errorf("live file name %q is not a plain file name ([A-Za-z0-9][A-Za-z0-9._-]{0,127})", n))
		}
		if seen[n] {
			errs = append(errs, fmt.Errorf("live file name %q is listed twice", n))
		}
		seen[n] = true
	}
	if c.LiveInterval < minLiveInterval {
		errs = append(errs, fmt.Errorf("STATIC_SITE_LIVE_INTERVAL: %s is shorter than %s", c.LiveInterval, minLiveInterval))
	}
	if c.LiveMaxBytes <= 0 {
		errs = append(errs, errors.New("STATIC_SITE_LIVE_MAX_BYTES must be positive"))
	}
	return errs
}

// validateContentTypes checks the configured Content-Type overrides: each
// path is the key a file is indexed under, and each type is a plain media
// type, so the value cannot smuggle a second header.
func validateContentTypes(c *Config) []error {
	var errs []error
	for p, t := range c.ContentTypes {
		if key, ok := cleanPath(p); !ok || key != p || strings.HasSuffix(p, "/") {
			errs = append(errs, fmt.Errorf("content type path %q is not a clean absolute file path", p))
		}
		if !mediaType.MatchString(t) {
			errs = append(errs, fmt.Errorf("content type of %q: %q is not type/subtype, optionally with ; charset=utf-8", p, t))
		}
	}
	return errs
}
