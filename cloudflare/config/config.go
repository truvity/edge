// Package config is the Cloudflare config model of an estate: the accounts
// it uses, the zones inside them and the settings each zone carries, the
// fleet Service every tunnel delivers to, the R2 buckets, and the rotation
// knobs of the token split. It is the typed form of one YAML file
// (FileName, conventionally cloudflare.yaml), and the schema is the library's:
// an estate loads the file and calls this package, it does not redefine the
// types.
//
// Tunnels are not authored. Load derives them from the edge catalog
// (package catalog): every domain on a cloudflare-tunnel exposure resolves to
// its zone and so to that zone's account, and each cluster gets one tunnel
// per account its names live in. A domain the catalog marks dns_elsewhere
// keeps its ingress rule and has no CNAME (Tunnel.DNSElsewhere).
//
// The package links no Pulumi and no Cloudflare SDK: it imports the standard
// library, the YAML decoder, and this module's catalog, cloudflare/edgespec,
// cloudflare/zonespec, cloudflare/rules and cloudflare/cfnames, which are
// themselves SDK-free. TestNoSDKImports fails the moment that changes.
//
// What the library owns: the schema, strict decoding (an unknown key fails),
// every validation that needs only the file and the catalog, the derivation
// of tunnels and cache hosts, the per-account view (ForAccount), the stack
// name convention (StackName, StackAccount), and the conversion of a zone's
// blocks into the library's inputs (Zone.Contributions, the Library
// methods).
//
// What stays with the estate, because it names the estate's own layout and
// not the Cloudflare model: where a tunnel token is stored (an OpenBao KV key,
// an SSM path), where a child token's root lives, the check that those keys
// are rows of the estate's secret layout, the estate's prototype stacks, and
// the Pulumi program that turns the model into resources. The tunnel's
// object names (Name, Release, SecretName) are the library's, because the
// cloudflared chart and cloudflare/edge already agree on them.
//
// docs/cloudflare/config.md has the schema reference.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/truvity/edge/v4/catalog"
	"github.com/truvity/edge/v4/cloudflare/cfnames"
	"github.com/truvity/edge/v4/cloudflare/edgespec"
	cfzone "github.com/truvity/edge/v4/cloudflare/zonespec"
)

const (
	// FileName is the registry's conventional path within a config
	// filesystem.
	FileName = "cloudflare.yaml"

	// defaultChildRotation is every root-stack child's rotation when
	// cloudflare.yaml's `root` block, or one child inside it, says
	// nothing: the first rotation, easy to bump later, never a reason on
	// its own to require an edit before the stack's first apply.
	defaultChildRotation = "1"
)

type (
	// Config is the registry, plus the tunnels derived from the edge
	// catalog.
	Config struct {
		Accounts map[string]Account `yaml:"accounts"`
		Zones    map[string]Zone    `yaml:"zones"`
		Fleets   FleetConfig        `yaml:"fleets"`
		// Tunnels is one entry per (cluster, account), sorted by cluster
		// then account. Not read from cloudflare.yaml: Load derives it
		// from the edge catalog (edgespec.Derive).
		Tunnels []Tunnel `yaml:"-"`
		// R2 is every bucket the estate's R2 stack provisions
		// (cloudflare/r2), by its own name — the
		// map key, not the Cloudflare bucket name, is what makes the
		// token-store key (the root stack's own r2-parent-<name> child)
		// and the Pulumi child/resource names: "scratch" for
		// r2["scratch"]. Empty unless a bucket is declared.
		R2 map[string]R2Bucket `yaml:"r2,omitempty"`
		// Root is the root stack's own children (the token split,
		// cloudflare/tokensplit): rotation knobs for the least-privilege
		// tokens it mints under the account's root token. Read only by that
		// stack; everything above is the edge stack's, except R2 above, the
		// R2 stack's own.
		Root RootChildren `yaml:"root"`
	}

	// Account is one Cloudflare account and the API token that manages it.
	Account struct {
		AccountID string `yaml:"account_id"`
		// LegacyNames keeps, for this account's tunnels, the names every
		// object had before the registry existed (Tunnel's name methods).
		// At most one account sets it; it is what made the move to the
		// registry rename nothing live.
		LegacyNames bool `yaml:"legacy_names,omitempty"`
		// Root is this account's root-stack children's
		// rotation knobs (RootFor). The legacy_names account's are the
		// top-level `root` block, so it must not set this; any other
		// account that leaves it out gets defaultChildRotation for every
		// child. Per account so rotating one account's child never
		// rotates another account's.
		Root *RootChildren `yaml:"root,omitempty"`
		// BreakglassItem names the password-manager item that holds this
		// account's ROOT token: the break-glass way to the account when the
		// preset issuer cannot mint (cloudflare/presettoken). Not a secret:
		// a name; where the item lives is the estate's. Empty for an account
		// whose item is not named: break-glass for it then refuses, naming
		// this field.
		BreakglassItem string `yaml:"breakglass_item,omitempty"`
		// Company is the company that owns the account, a free label the
		// estate uses to group accounts (stack granularity, access rosters);
		// the library does not interpret it beyond its shape: a lowercase
		// name (CompanyPattern). Optional.
		Company string `yaml:"company,omitempty"`
	}

	// Zone is one Cloudflare zone: the account it lives in, and the zone
	// settings this estate manages (cloudflare/zonespec).
	Zone struct {
		// Name is the zone's domain name, the key it is filed under. Set by
		// Parse and Validate; not read from the file. Contributions needs it.
		Name    string `yaml:"-"`
		Account string `yaml:"account"`
		ZoneID  string `yaml:"zone_id"`
		// Manage says how much of the zone this registry manages: ManageFull
		// (the default, empty) is everything the other fields declare;
		// ManageSecurityTxt brings a hand-managed zone in for its
		// security.txt ONLY. Such a zone must carry zone_id, account and
		// security_txt and nothing else (validateSecurityTxtOnly); every
		// builder skips it, and the edge preset grants its token one group,
		// Zone Settings Write.
		Manage string `yaml:"manage,omitempty"`
		// ZoneIDPending declares a zone whose id nobody has looked up yet
		// (an owner TODO: no credential for its account exists). It
		// stands in for zone_id, never beside it, and only on an account
		// that is not the legacy_names one: tunnels and hostnames may be
		// derived for the zone, but its account's own stacks
		// (cloudflare-<kind>-<account>, ForAccount) refuse until the id
		// is written, and no other account's stack ever sees the zone.
		ZoneIDPending bool   `yaml:"zone_id_pending,omitempty"`
		SSLMode       string `yaml:"ssl_mode"`
		// MinTLSVersion is the zone's floor for browsers; empty leaves it
		// unmanaged.
		MinTLSVersion string `yaml:"min_tls_version,omitempty"`
		// TotalTLS makes the zone issue a certificate for every proxied
		// hostname, so a new name needs no certificate pack of its own.
		// Nil leaves the setting unmanaged.
		TotalTLS *TotalTLSCfg `yaml:"total_tls,omitempty"`
		// Cache decides which of this zone's hostnames may be cached at
		// all. Nil leaves the zone's caching alone; a block turns it
		// into a policy, and the HOSTS ARE DERIVED -- see derive (edgespec.Derive).
		Cache *CacheCfg `yaml:"cache,omitempty"`
		// CacheHosts is that derivation: the names of this zone a
		// business project is served on. Written by Load, never
		// authored, and empty unless Cache is set.
		CacheHosts []string `yaml:"-"`
		// WAFHosts is the exact hosts the managed WAF covers when
		// managed_waf.enabled: derived by Load from the edge catalog (every
		// name an exposure serves in this zone, minus those a domain opted
		// out with waf: off) and from this zone's proxied records, plus
		// whatever the consumer adds with Config.AddWAFHosts. Sorted,
		// de-duplicated, never a wildcard. Never authored.
		WAFHosts []string `yaml:"-"`
		// TrustedClients exempts named client address ranges from the
		// zone's Cloudflare security features on named hosts. Nil
		// manages no firewall ruleset at all.
		TrustedClients *TrustedClientsCfg `yaml:"trusted_clients,omitempty"`
		// RateLimits is the zone's WHOLE http_ratelimit ruleset: the
		// rate limiting rules that protect public ingest endpoints
		// (D251 B: no public ingest endpoint goes live before a rule
		// covers it). Nil leaves the phase unmanaged; a block owns it,
		// so a rule added in the dashboard is replaced.
		RateLimits *RateLimitsCfg `yaml:"rate_limits,omitempty"`
		// Records are DNS records this zone serves for an origin no tunnel
		// carries (a load balancer, a CDN), and child-zone NS delegations:
		// the one place a hostname is written here rather than derived
		// from the edge catalog. Only the listed records are managed; the
		// zone's other records (hand made, imported) stay as they are. A
		// name a tunnel serves is refused (validateRecordTunnels), and so
		// is a tunnel host at or below an NS delegation.
		Records []RecordCfg `yaml:"records,omitempty"`
		// Redirects are the zone's canonical-host Single Redirects: the
		// complete intended rule list of the
		// http_request_dynamic_redirect phase. Nil manages no redirect
		// ruleset; a declared list owns the phase. A redirect whose source
		// record is proxied makes that record wait for the ruleset's apply
		// (zone.Args.Redirects), and a DNS-only source stages it inert.
		Redirects []RedirectCfg `yaml:"redirects,omitempty"`
		// OriginCNAMEs is the switch that moves a tunnel-served host's
		// proxied CNAME off its tunnel and onto another origin (the AWS
		// API Gateway regional domain name of a service that fronts
		// the host itself). Key: an exact host of this zone
		// that the edge catalog already serves through a tunnel. Value:
		// "" keeps the tunnel (the default, and the rollback), or the
		// origin hostname. The host's tunnel ingress rule stays either
		// way, so the rollback is one value back to "". Setting any
		// non-empty value also turns on the zone's Authenticated Origin
		// Pulls (ActiveOrigins), because that origin verifies Cloudflare's
		// client certificate.
		OriginCNAMEs map[string]string `yaml:"origin_cnames,omitempty"`
		// PublicCache caches an installation's public documents (a key set and
		// two metadata documents; rules.PublicPathsCache) at the edge for ONE host of this zone. It adds a
		// rule to the zone's cache policy, so it needs Cache (the cache
		// ruleset it extends). Nil leaves every request reaching the origin.
		PublicCache *PublicCacheCfg `yaml:"public_cache,omitempty"`
		// SSLModeRules is the zone's configuration rules: host -> the SSL mode
		// Cloudflare uses toward that host's origin, overriding ssl_mode for
		// that host alone. Nil leaves the http_config_settings phase unmanaged;
		// a map owns the phase's whole entrypoint ruleset, so a configuration
		// rule added in the dashboard is replaced.
		SSLModeRules map[string]string `yaml:"ssl_mode_rules,omitempty"`
		// SSLModeRuleRefs pins the Cloudflare rule ref (32 hex digits) of a
		// host's rule in ssl_mode_rules, so adopting a hand-made rule keeps
		// its ref instead of Cloudflare minting a new one. Optional; a key
		// must also be a key of ssl_mode_rules.
		SSLModeRuleRefs map[string]string `yaml:"ssl_mode_rule_refs,omitempty"`
		// ConfigRulesImportID is the id of an entrypoint ruleset of that phase
		// made by hand, adopted by the next apply instead of created (a second
		// entrypoint cannot be created). Remove it once that apply is done: a
		// standing import blocks every later replace.
		ConfigRulesImportID string `yaml:"config_rules_import_id,omitempty"`
		// RedirectRulesImportID is the same for the
		// http_request_dynamic_redirect phase: adopt an existing Single
		// Redirects entry point ruleset by id, preserving its rule refs.
		// Requires `redirects` (the complete intended rule list). Remove it
		// once that apply is done.
		RedirectRulesImportID string `yaml:"redirect_rules_import_id,omitempty"`
		// AlwaysUseHTTPS is the zone setting always_use_https: true
		// redirects every plain-HTTP request to HTTPS. Nil leaves it
		// unmanaged.
		AlwaysUseHTTPS *bool `yaml:"always_use_https,omitempty"`
		// HSTS is the zone's Strict-Transport-Security header (the zone
		// setting security_header). Nil leaves it unmanaged.
		HSTS *HSTSCfg `yaml:"hsts,omitempty"`
		// BotManagement is the zone's AI bots rule and managed robots.txt
		// (the bot_management setting; only those two fields). Nil leaves
		// it unmanaged.
		BotManagement *BotManagementCfg `yaml:"bot_management,omitempty"`
		// ManagedWAF deploys the Cloudflare Managed Ruleset on the listed
		// hosts: the zone's WHOLE http_request_firewall_managed phase. Nil
		// leaves the phase unmanaged.
		ManagedWAF *ManagedWAFCfg `yaml:"managed_waf,omitempty"`
		// SecurityTxt is the zone's /.well-known/security.txt (Cloudflare
		// Security Center). Not a Pulumi resource -- the provider has none
		// -- so the edge stack ignores it and a tool of the estate applies it. Nil leaves it unmanaged.
		SecurityTxt *SecurityTxtCfg `yaml:"security_txt,omitempty"`
	}

	// PublicCacheCfg is the host whose public documents are cached and for how
	// long. The rule itself is rules.PublicPathsCache;
	// this block only chooses the host and the edge TTL.
	PublicCacheCfg struct {
		// Host is the exact, lowercase name of the installation, inside
		// the zone (access.example.com).
		Host string `yaml:"host"`
		// TTL is the edge TTL, a Go duration in whole seconds ("5m"). Empty
		// means the default (rules.DefaultPublicCacheTTL, 5m). At most an hour: a signing key removed
		// from the key set stays served for up to the TTL.
		TTL string `yaml:"ttl,omitempty"`
	}

	// HSTSCfg mirrors cloudflare/zonespec HSTS in this file's
	// snake_case. Preload is refused here: leaving the preload list takes months.
	HSTSCfg struct {
		Enabled           bool `yaml:"enabled"`
		MaxAge            int  `yaml:"max_age"`
		IncludeSubdomains bool `yaml:"include_subdomains,omitempty"`
		Nosniff           bool `yaml:"nosniff,omitempty"`
		Preload           bool `yaml:"preload,omitempty"`
	}

	// BotManagementCfg mirrors cloudflare/zonespec BotManagement in this
	// file's snake_case. AIBots is block, disabled or only_on_ad_pages.
	BotManagementCfg struct {
		AIBots           string `yaml:"ai_bots,omitempty"`
		ManagedRobotsTxt *bool  `yaml:"managed_robots_txt,omitempty"`
	}

	// ManagedWAFCfg mirrors cloudflare/zonespec ManagedWAF in this
	// file's snake_case: the hosts the Cloudflare Managed Ruleset runs on.
	//
	// THE WAY TO WRITE IT is `managed_waf: {enabled: true}`: the hosts are
	// derived, explicit and exact, from the names the edge catalog serves in
	// the zone (and the zone's proxied records), so the WAF's coverage lives
	// next to where each name is declared and a new name is covered with no
	// edit here. A name is left out with `waf: off` and a `waf_reason` on
	// its catalog domain (`waf: false` and `waf_reason` on a record).
	//
	// Hosts and WholeZone are the older, hand-written forms. They stay
	// for a zone that has not moved, are exclusive with Enabled, and Hosts
	// refuses a wildcard.
	ManagedWAFCfg struct {
		// Enabled derives the host list (see above). Exclusive with hosts
		// and whole_zone.
		Enabled   bool     `yaml:"enabled,omitempty"`
		Hosts     []string `yaml:"hosts,omitempty"`
		WholeZone bool     `yaml:"whole_zone,omitempty"`
	}

	// SecurityTxtCfg is the zone's security.txt (RFC 9116), served by
	// Cloudflare on every proxied name of the zone.
	SecurityTxtCfg struct {
		// Contact is one or more mailto: or https: URIs.
		Contact []string `yaml:"contact"`
		// Expires is an RFC 3339 timestamp; RFC 9116 wants it less than
		// a year ahead, which the tool that applies the file should check.
		Expires string `yaml:"expires"`
		// PreferredLanguages is a comma-separated list of language tags.
		PreferredLanguages string `yaml:"preferred_languages,omitempty"`
	}

	// RecordCfg mirrors cloudflare/zonespec Record in this file's
	// snake_case: one A, AAAA, CNAME or child-zone NS record.
	RecordCfg struct {
		Name    string `yaml:"name"`
		Type    string `yaml:"type"`
		Content string `yaml:"content"`
		Proxied bool   `yaml:"proxied,omitempty"`
		TTL     int    `yaml:"ttl,omitempty"`
		Comment string `yaml:"comment,omitempty"`
		// WAF is false to leave a proxied record's name out of a zone's
		// derived managed WAF; absent is covered. False needs WAFReason.
		// Never set on NS.
		WAF       *bool  `yaml:"waf,omitempty"`
		WAFReason string `yaml:"waf_reason,omitempty"`
	}

	// RedirectCfg is one canonical-host Single Redirect in this file's
	// snake_case. The source is matched exactly on HTTP and HTTPS for every
	// path; To is an HTTPS origin and the request's path is appended. A
	// source whose DNS record is proxied makes that record wait for this
	// rule's ruleset (the explicit rule-before-proxy dependency); a DNS-only
	// source stages the rule without effect.
	RedirectCfg struct {
		Ref  string `yaml:"ref"`
		From string `yaml:"from"`
		To   string `yaml:"to"`
		// Status is 301 (default), 302, 307 or 308.
		Status int `yaml:"status,omitempty"`
		// PreserveQueryString keeps the query string; absent means true.
		PreserveQueryString *bool `yaml:"preserve_query_string,omitempty"`
		// Disabled keeps the rule and its ref but makes it inert: the second
		// stage of the rollback.
		Disabled bool `yaml:"disabled,omitempty"`
	}

	// RateLimitsCfg mirrors cloudflare/zonespec RateLimits in
	// this file's snake_case. The plan decides how many rules fit (Pro:
	// two) and what they can count (Pro: the address only, no method
	// field, no separate counting expression), and the library refuses a
	// rule the plan does not offer -- validate runs that same check at
	// load, so a bad rule fails CI and not an apply.
	//
	// SHARED COUNTERS: every endpoint listed in ONE rule feeds one counter
	// per client address (per Cloudflare data center). Put endpoints with
	// the same caller profile in one rule -- browsers posting telemetry in
	// one, machine-to-machine ingest in another -- because the Pro quota
	// is two rules for the whole zone.
	RateLimitsCfg struct {
		Plan  string             `yaml:"plan"`
		Rules []RateLimitRuleCfg `yaml:"rules"`
	}

	// RateLimitRuleCfg is one rate limiting rule (cloudflare/zonespec RateLimitRule).
	RateLimitRuleCfg struct {
		Name               string        `yaml:"name"`
		Endpoints          []EndpointCfg `yaml:"endpoints,omitempty"`
		Expression         string        `yaml:"expression,omitempty"`
		CountingExpression string        `yaml:"counting_expression,omitempty"`
		Characteristics    []string      `yaml:"characteristics,omitempty"`
		Period             int           `yaml:"period"`
		Requests           int           `yaml:"requests"`
		MitigationTimeout  int           `yaml:"mitigation_timeout,omitempty"`
		Action             string        `yaml:"action"`
	}

	// EndpointCfg is one host and path a rule protects (cloudflare/zonespec Endpoint).
	EndpointCfg struct {
		Host       string   `yaml:"host"`
		Path       string   `yaml:"path,omitempty"`
		PathPrefix string   `yaml:"path_prefix,omitempty"`
		Methods    []string `yaml:"methods,omitempty"`
	}

	// TrustedClientsCfg is ONE zone custom-firewall rule with action
	// `skip`: a request to one of Hosts from an address in Ranges skips
	// the bot and rate-limit checks (rate limiting, Super Bot Fight Mode,
	// the legacy bic, hot, rateLimit, securityLevel and uaBlock products)
	// and nothing else: the managed rules and the rest of the custom rules
	// still apply. Skip changes that. Insurance, not a
	// workaround: it keeps a known machine-to-machine caller (a
	// connector platform's published egress range) from ever meeting a
	// challenge a later rule might introduce. The zone owns the whole
	// http_request_firewall_custom phase, so rules added in the
	// dashboard are replaced; add them here instead.
	TrustedClientsCfg struct {
		// Hosts are exact hostnames inside this zone.
		Hosts []string `yaml:"hosts"`
		// Ranges are CIDR blocks (IPv4 or IPv6).
		Ranges []string `yaml:"ranges"`
		// Skip names what is skipped, replacing the default (bot and
		// rate-limit checks): phases http_ratelimit, http_request_sbfm,
		// http_request_firewall_managed; products bic, hot, rateLimit,
		// securityLevel, uaBlock, waf, zoneLockdown; and "current" (the
		// rest of the custom rules). The pre-Skip behaviour is all of them.
		Skip []string `yaml:"skip,omitempty"`
	}

	// CacheCfg turns a zone's caching into a policy. It carries no host
	// list on purpose: the hosts are derived from the edge catalog, so a
	// project exposed today is cacheable today and a hand-written list
	// cannot fall behind one.
	CacheCfg struct {
		// RespectOriginBrowserTTL sets the zone's Browser Cache TTL to
		// "Respect Existing Headers".
		//
		// Separate from the rules because it covers what they do not: a
		// response no rule caches still leaves the zone with a
		// `Cache-Control` Cloudflare wrote, and unmanaged that is four
		// hours for every host of the zone.
		RespectOriginBrowserTTL bool `yaml:"respect_origin_browser_ttl,omitempty"`
	}

	// TotalTLSCfg is a zone's Total TLS switch (Advanced Certificate
	// Manager).
	TotalTLSCfg struct {
		Enabled              bool   `yaml:"enabled"`
		CertificateAuthority string `yaml:"certificate_authority,omitempty"`
	}

	// FleetConfig is the Service every tunnel ingress rule delivers to.
	FleetConfig struct {
		Internal FleetEntry `yaml:"internal"`
	}

	// FleetEntry holds the Service name + namespace for one fleet.
	FleetEntry struct {
		Service   string `yaml:"service"`
		Namespace string `yaml:"namespace"`
	}

	// R2Bucket is one bucket the estate's R2 stack owns (the resource
	// itself: bucket, jurisdiction, lifecycle; cloudflare/r2). Removal
	// is `enabled: false` (or deleting the entry) plus an apply -- the
	// switch this estate's removal guides ask for.
	//
	// The bucket's own account-owned, bucket-scoped parent token is
	// minted SEPARATELY, by the root stack's own
	// `r2-parent-<name>` child (internal/components/cloudflareroot
	// Deploy, via account.NewChildTokenSet and account.
	// R2BucketParentPolicies) -- the R2 stack never mints a token itself
	// (see its package doc). Token below still configures that grant's
	// shape even though the R2 stack ignores it entirely when creating
	// the bucket (it leaves pkg/r2's Token at its zero value there).
	R2Bucket struct {
		// Enabled turns the bucket AND its root-stack parent token
		// on. False (or the entry missing) means neither is provisioned;
		// a bucket already live is torn down by flipping this rather
		// than by deleting the entry, so the tombstone stays reviewable.
		Enabled bool `yaml:"enabled"`
		// Account this bucket lives in (accounts.<name> above). Required.
		Account string `yaml:"account"`
		// Bucket is the R2 bucket name Cloudflare sees. Required; must
		// meet Cloudflare's own naming rules (cloudflare/r2
		// Validate checks these at apply time).
		Bucket string `yaml:"bucket"`
		// Jurisdiction is the bucket's data-residency jurisdiction: "eu",
		// "fedramp" or "us". Empty (or "default") is an ordinary,
		// non-jurisdictional bucket. It also segments the resource key
		// the root stack's own r2-parent-<name> child is scoped to
		// (cloudflare/account R2BucketScope.Jurisdiction), so
		// the bucket and its parent token can never disagree about which
		// jurisdiction they mean.
		Jurisdiction string `yaml:"jurisdiction,omitempty"`
		// Lifecycle expires objects after a fixed age. Nil creates none.
		Lifecycle *R2Lifecycle `yaml:"lifecycle,omitempty"`
		// Token describes the account-owned, bucket-scoped parent token
		// the root stack's own r2-parent-<name> child hands out for this
		// bucket. Required: a bucket with no token is not what this
		// registry is for -- every entry here is a credential test bed or
		// a future broker parent key, never a bucket nobody reads.
		Token R2Token `yaml:"token"`
	}

	// R2Lifecycle expires objects after a fixed age (cloudflare/r2.Lifecycle).
	R2Lifecycle struct {
		// ExpireAfterDays is the object age, in days, at which R2 deletes
		// it. Required; must be positive.
		ExpireAfterDays int `yaml:"expire_after_days"`
	}

	// R2Token is the bucket's account-owned parent token, minted by cloudflare/tokensplit.
	R2Token struct {
		// Permission is object-read-write or object-read-only. Required.
		Permission string `yaml:"permission"`
		// ExpiresOn is an RFC3339 timestamp after which Cloudflare
		// refuses the token. Optional; strongly recommended for a
		// scratch or test bucket so a forgotten token does not outlive
		// its errand.
		ExpiresOn string `yaml:"expires_on,omitempty"`
		// Rotation forces a fresh token (new id and value) when edited to
		// any new value -- a date, a counter, a reason. Optional; empty
		// never rotates on its own.
		Rotation string `yaml:"rotation,omitempty"`
	}

	// RootChildren is the root stack's own config: one rotation knob per
	// child it mints (cloudflare/tokensplit). Every field has a value after
	// Parse: one left out is the default rotation, "1".
	RootChildren struct {
		// Edge covers what the edge stack manages: tunnels (account scope)
		// and, per zone in Zones above that shares the root token's
		// account, DNS/SSL/Settings/Cache.
		Edge ChildRotation `yaml:"edge"`
		// Status covers the status stack's narrower token: tunnels plus
		// DNS only, same zones as Edge.
		Status ChildRotation `yaml:"status"`
		// R2Admin covers account-wide R2 bucket administration
		// (Workers R2 Storage), for whichever stack eventually creates
		// buckets under it.
		R2Admin ChildRotation `yaml:"r2_admin"`
		// R2Parent is the rotation every enabled R2 bucket's own parent
		// child token shares (cloudflare/r2-parent-{bucket}, one child
		// per bucket in R2 above with Enabled true). Ignored, but still
		// required, while R2 declares no enabled bucket.
		R2Parent ChildRotation `yaml:"r2_parent"`
	}

	// ChildRotation is one child token's rotation knob
	// (cloudflare/account ChildTokenConfig.Rotation):
	// changing it to any new value forces Pulumi to REPLACE that child
	// (a fresh id and secret). A free string — a date, a counter, a
	// reason — never parsed, only compared for change.
	ChildRotation struct {
		Rotation string `yaml:"rotation"`
	}

	// Tunnel is one cluster's tunnel into one account, derived. Its
	// object names are methods, so the naming rule has one home.
	Tunnel struct {
		Cluster string
		Account string
		// LegacyNames is the account's legacy_names, copied so the name
		// methods need no registry.
		LegacyNames bool
		// Zone is the zone of the first host (edgespec.Tunnel.Zone).
		Zone string
		// Zones is every zone the hosts lie in, sorted, all of the tunnel's
		// account: the library creates each host's CNAME in its own zone
		// (cloudflare/tunnel DNS.Zones). A tunnel whose
		// hosts lie in one zone has Zones = {Zone} and registers exactly what
		// it did before. Empty means {Zone}, as in the library (InZones).
		Zones []string
		// Hosts is the tunnel's hostnames in ingress order: one ingress
		// rule each, and one CNAME each unless the catalog says
		// dns_elsewhere (tunnel.OrderHosts).
		Hosts []string
		// DNSElsewhere is the part of Hosts whose DNS record something else
		// writes (the catalog's dns_elsewhere): the name keeps its ingress
		// rule and the tunnel creates no CNAME for it. Ingress order.
		DNSElsewhere []string
	}
)

// Name is the tunnel's name in Cloudflare and the name its Pulumi
// resources are registered under: the cluster for the legacy_names account,
// <cluster>-<account> for any other.
func (t Tunnel) Name() string { return edgespec.TunnelName(t.Cluster, t.Account, t.LegacyNames) }

// CarriesIn reports whether the tunnel carries host as a name of zone: each
// host's CNAME lives in the zone the host lies in (Zones), not only in Zone.
func (t Tunnel) CarriesIn(zone, host string) bool {
	return slices.Contains(t.Hosts, host) && edgespec.ZoneOf(t.InZones(), host) == zone
}

// InZones is every zone the tunnel's hosts lie in: Zones, or Zone alone
// when Zones is empty (cloudflare/edge reads it the same way).
func (t Tunnel) InZones() []string {
	if len(t.Zones) > 0 {
		return t.Zones
	}

	return []string{t.Zone}
}

// Release is the cloudflared Helm release, and the suffix of its Argo CD
// Application's name: cloudflared, or cloudflared-<account>.
func (t Tunnel) Release() string { return edgespec.ReleaseName(t.Account, t.LegacyNames) }

// SecretName is the Kubernetes Secret holding the tunnel token in
// cloudflare-system, and the ExternalSecret writing it.
func (t Tunnel) SecretName() string { return edgespec.SecretName(t.Account, t.LegacyNames) }

// DNSNames are the hosts the tunnel creates a CNAME for: Hosts without
// DNSElsewhere, in ingress order.
func (t Tunnel) DNSNames() []string {
	return edgespec.Tunnel{Hosts: t.Hosts, DNSElsewhere: t.DNSElsewhere}.DNSNames()
}

// OwnsRecord reports whether the tunnel writes the CNAME of host in zone:
// it carries the host (CarriesIn) and the host is not dns_elsewhere.
func (t Tunnel) OwnsRecord(zone, host string) bool {
	return t.CarriesIn(zone, host) && !slices.Contains(t.DNSElsewhere, host)
}

// CompanyPattern is the shape of Account.Company.
var CompanyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Parse decodes cloudflare.yaml and validates the registry: everything
// authored in the file, which needs no catalog. Decoding is strict, an unknown
// key at any depth fails. The result has no Tunnels and no CacheHosts; Load
// derives them.
func Parse(data []byte) (*Config, error) {
	var cfg Config

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	if err := dec.Decode(&cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("cfg: parse %s: the file is empty", FileName)
		}

		return nil, fmt.Errorf("cfg: parse %s: %w", FileName, err)
	}

	cfg.applyRootDefaults()
	cfg.nameZones()

	if err := cfg.validateRegistry(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Load reads cloudflare.yaml from fsys (Parse), derives the tunnels and each
// cache zone's hosts from the edge catalog, and validates both together.
//
// cat is the catalog with its generated groups already in place
// (catalog.Config.WithProjects), because the tunnels are derived from the
// groups. projects is the project list the catalog was generated from; a zone
// that declares `cache` caches the hosts of the business projects in it, so
// projects must not be nil when any zone does (an empty, non-nil slice is
// "no business project").
func Load(fsys fs.FS, cat *catalog.Config, projects []catalog.Project) (*Config, error) {
	data, err := fs.ReadFile(fsys, FileName)
	if err != nil {
		return nil, fmt.Errorf("cfg: read %s: %w", FileName, err)
	}

	cfg, err := Parse(data)
	if err != nil {
		return nil, err
	}

	if cat == nil {
		return nil, fmt.Errorf("%s: the edge catalog is required to derive the tunnels", FileName)
	}

	if projects == nil && cfg.cachesAnything() {
		return nil, fmt.Errorf("%s: a zone declares `cache`, whose hosts are derived from projects: pass the project list", FileName)
	}

	if err := cfg.derive(cat, projects); err != nil {
		return nil, err
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// nameZones fills each Zone's Name from its key.
func (c *Config) nameZones() {
	for name := range c.Zones {
		z := c.Zones[name]
		z.Name = name
		c.Zones[name] = z
	}
}

// TunnelsOn returns one cluster's tunnels, sorted by account.
func (c *Config) TunnelsOn(cluster string) []Tunnel {
	var out []Tunnel

	for i := range c.Tunnels {
		t := &c.Tunnels[i]

		if t.Cluster == cluster {
			out = append(out, *t)
		}
	}

	return out
}

// Clusters returns every cluster with a tunnel, sorted.
func (c *Config) Clusters() []string {
	var out []string

	for i := range c.Tunnels {
		t := &c.Tunnels[i]

		if !slices.Contains(out, t.Cluster) {
			out = append(out, t.Cluster)
		}
	}

	sort.Strings(out)

	return out
}

// ZoneOf returns the zone a hostname lies in (edgespec.ZoneOf), or "".
//
// A zone managed for its security.txt only (Zone.SecurityTxtOnly) is not a
// zone a host can lie in: the registry serves no name there.
func (c *Config) ZoneOf(host string) string {
	return edgespec.ZoneOf(c.managedZoneNames(), host)
}

// managedZoneNames is the sorted names of the zones that are managed beyond
// their security.txt.
func (c *Config) managedZoneNames() []string {
	names := make([]string, 0, len(c.Zones))

	for _, name := range sortedKeys(c.Zones) {
		if !c.Zones[name].SecurityTxtOnly() {
			names = append(names, name)
		}
	}

	return names
}

// derive hands the edge catalog, the zones and the accounts to
// cloudflare/edgespec, which derives the tunnels (one per cluster and
// account, hosts in ingress order) and the cache hosts of every zone that
// declares a cache policy; it fills Tunnels and each cache zone's CacheHosts.
func (c *Config) derive(cat *catalog.Config, projects []catalog.Project) error {
	in := edgespec.DeriveInput{
		Zones:       make(map[string]string, len(c.Zones)),
		LegacyNames: make(map[string]bool, len(c.Accounts)),
	}

	for name := range c.Zones {
		if c.Zones[name].SecurityTxtOnly() {
			continue
		}

		in.Zones[name] = c.Zones[name].Account

		if c.Zones[name].Cache != nil {
			in.CacheZones = append(in.CacheZones, name)
		}

		if m := c.Zones[name].ManagedWAF; m != nil && m.Enabled {
			in.WAFZones = append(in.WAFZones, name)
		}
	}

	sort.Strings(in.CacheZones)
	sort.Strings(in.WAFZones)

	for name, a := range c.Accounts {
		in.LegacyNames[name] = a.LegacyNames
	}

	in.Business = make(map[string]struct{}, len(projects))
	for i := range projects {
		in.Business[projects[i].Name] = struct{}{}
	}

	for _, name := range sortedKeys(cat.Exposures) {
		e := cat.Exposures[name]
		if e.Arrival != catalog.ArrivalCloudflareTunnel {
			continue
		}

		exposure := edgespec.Exposure{Name: name, Accounts: e.CloudflareAccounts}

		for _, cluster := range sortedKeys(e.Clusters) {
			for _, g := range cat.Groups(name, cluster) {
				group := edgespec.Group{
					Name: g.Name, Cluster: cluster, Hosts: g.Hosts(), RouteNamespaces: g.RouteNamespaces,
				}

				for _, d := range g.Domains {
					if d.DNSElsewhere {
						group.DNSElsewhere = append(group.DNSElsewhere, d.Host)
					}

					if !d.WAFCovered() {
						group.WAFOff = append(group.WAFOff, d.Host)
					}
				}

				exposure.Groups = append(exposure.Groups, group)
			}
		}

		in.Exposures = append(in.Exposures, exposure)
	}

	derived, err := edgespec.Derive(in)
	if err != nil {
		return fmt.Errorf("%s: the edge catalog %w", FileName, err)
	}

	c.Tunnels = nil

	for i := range derived.Tunnels {
		t := &derived.Tunnels[i]
		c.Tunnels = append(c.Tunnels, Tunnel{
			Cluster: t.Cluster, Account: t.Account, LegacyNames: c.Accounts[t.Account].LegacyNames,
			Zone: t.Zone, Zones: t.Zones, Hosts: t.Hosts, DNSElsewhere: t.DNSElsewhere,
		})
	}

	for name, hosts := range derived.CacheHosts {
		z := c.Zones[name]
		z.CacheHosts = hosts
		c.Zones[name] = z
	}

	return c.deriveWAF(derived.WAFHosts)
}

// deriveWAF fills each enabled zone's WAFHosts: the hosts the catalog
// derived plus the zone's proxied records that have not opted out. A proxied
// wildcard record must opt out, for the reason a wildcard catalog host must.
func (c *Config) deriveWAF(fromCatalog map[string][]string) error {
	for _, name := range sortedKeys(c.Zones) {
		z := c.Zones[name]
		if z.ManagedWAF == nil || !z.ManagedWAF.Enabled {
			continue
		}

		hosts := append([]string(nil), fromCatalog[name]...)

		for _, r := range z.Records {
			if r.Proxied && r.WAF != nil && !*r.WAF && strings.TrimSpace(r.WAFReason) == "" {
				return fmt.Errorf("%s: zones[%s].records[%s].waf_reason: required with waf: false", FileName, name, r.Name)
			}

			if r.WAFReason != "" && (r.WAF == nil || *r.WAF) {
				return fmt.Errorf("%s: zones[%s].records[%s].waf_reason: only meaningful with waf: false", FileName, name, r.Name)
			}

			// A delegation is DNS only and never a WAF host, even if an
			// invalid row asked for both: reject it rather than derive it.
			if r.Type == "NS" {
				if r.Proxied || r.WAF != nil {
					return fmt.Errorf("%s: zones[%s].records[%s]: an NS delegation is DNS only and takes no WAF", FileName, name, r.Name)
				}

				continue
			}

			if !(cfzone.Record{Proxied: r.Proxied, WAF: r.WAF}).WAFCovered() {
				continue
			}

			if strings.HasPrefix(r.Name, "*.") {
				return fmt.Errorf("%s: zones[%s].records[%s] is a proxied wildcard, and managed_waf lists exact names only: "+
					"give the record waf: false and a waf_reason, and add records for the exact names", FileName, name, r.Name)
			}

			hosts = append(hosts, r.Name)
		}

		z.WAFHosts = sortedUnique(hosts)
		c.Zones[name] = z
	}

	return nil
}

func sortedUnique(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)

	return slices.Compact(out)
}

// AddWAFHosts adds exact hosts to the managed WAF of zone: for a name the
// edge catalog and the zone's records do not declare, such as a status page
// another stack creates in this zone. Call it after Load and before
// Contributions or the zone stack. Each host must be an exact lower-case name
// inside the zone (no wildcard, no scheme or path), and the zone must have
// managed_waf: {enabled: true}. Hosts already covered are not repeated.
//
// Apply the zone stack before the stack that creates the name: a WAF entry for
// a name that does not exist yet is harmless, the reverse leaves it unprotected.
func (c *Config) AddWAFHosts(zone string, hosts ...string) error {
	z, ok := c.Zones[zone]
	if !ok {
		return fmt.Errorf("%s: AddWAFHosts: no zone %q", FileName, zone)
	}

	if z.ManagedWAF == nil || !z.ManagedWAF.Enabled {
		return fmt.Errorf("%s: zones[%s]: AddWAFHosts needs managed_waf: {enabled: true}", FileName, zone)
	}

	for _, h := range hosts {
		if err := validateExactWAFHost(zone, h); err != nil {
			return fmt.Errorf("%s: zones[%s]: AddWAFHosts: %w", FileName, zone, err)
		}
	}

	z.WAFHosts = sortedUnique(append(append([]string(nil), z.WAFHosts...), hosts...))
	c.Zones[zone] = z

	return nil
}

// validateExactWAFHost holds a host to what the derived WAF accepts: an exact
// lower-case name that is the zone or inside it.
func validateExactWAFHost(zone, host string) error {
	if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, `*/:"\ `) || strings.HasSuffix(host, ".") ||
		(host != zone && !strings.HasSuffix(host, "."+zone)) {
		return fmt.Errorf("%q must be an exact lower-case hostname inside %s (no wildcard)", host, zone)
	}

	return nil
}

// objectName is what an account name must be: it becomes part of tunnel,
// Helm release, Secret and Pulumi resource names.
var objectName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}[a-z0-9]$`)

// validateRegistry checks accounts, zones and fleets: everything authored
// in cloudflare.yaml, before anything is derived from it.
func (c *Config) validateRegistry() error {
	if len(c.Accounts) == 0 {
		return fmt.Errorf("%s: at least one account is required", FileName)
	}

	var legacy []string

	for _, name := range sortedKeys(c.Accounts) {
		a := c.Accounts[name]
		where := fmt.Sprintf("%s: accounts[%s]", FileName, name)

		if !objectName.MatchString(name) {
			return fmt.Errorf("%s: the name becomes part of tunnel, release and Secret names, so it must match %s", where, objectName)
		}

		if a.AccountID == "" {
			return fmt.Errorf("%s.account_id is required", where)
		}

		if a.LegacyNames && a.Root != nil {
			return fmt.Errorf("%s.root: the legacy_names account's rotations are the top-level `root` block", where)
		}

		if c := a.Company; c != "" && !CompanyPattern.MatchString(c) {
			return fmt.Errorf("%s.company: %q must match %s", where, c, CompanyPattern)
		}

		if a.LegacyNames {
			legacy = append(legacy, name)
		}
	}

	if len(legacy) > 1 {
		return fmt.Errorf("%s: accounts %v all set legacy_names; at most one account keeps the unsuffixed names", FileName, legacy)
	}

	if len(c.Zones) == 0 {
		return fmt.Errorf("%s: at least one zone is required", FileName)
	}

	for _, name := range sortedKeys(c.Zones) {
		z := c.Zones[name]
		where := fmt.Sprintf("%s: zones[%s]", FileName, name)

		if name != strings.ToLower(name) || !strings.Contains(name, ".") || strings.Contains(name, "*") {
			return fmt.Errorf("%s: a zone is a lowercase domain name with no wildcard", where)
		}

		if _, ok := c.Accounts[z.Account]; !ok {
			return fmt.Errorf("%s.account: no account %q (known: %s)", where, z.Account, strings.Join(sortedKeys(c.Accounts), ", "))
		}

		if err := z.validateManage(where); err != nil {
			return err
		}

		if z.SecurityTxtOnly() {
			if err := z.validateHardening(where, name); err != nil {
				return err
			}

			continue
		}

		switch {
		case z.ZoneIDPending && z.ZoneID != "":
			return fmt.Errorf("%s: zone_id_pending beside a zone_id; drop zone_id_pending", where)
		case z.ZoneIDPending && c.Accounts[z.Account].LegacyNames:
			return fmt.Errorf("%s: zone_id_pending on the legacy_names account %q, whose stacks must never meet a zone without an id", where, z.Account)
		case !z.ZoneIDPending && z.ZoneID == "":
			return fmt.Errorf("%s.zone_id is required (or zone_id_pending: true while the owner looks it up)", where)
		}

		if z.SSLMode == "" {
			return fmt.Errorf("%s.ssl_mode is required", where)
		}

		// The zone's default toward its origins: the same encrypting modes a
		// per-host rule may set. `strict` suits a zone whose origins all
		// serve a publicly trusted certificate (the legacy ALBs' ACM ones).
		if !sslModeRuleModes[z.SSLMode] {
			return fmt.Errorf("%s.ssl_mode: %q is not allowed (valid: full, strict)", where, z.SSLMode)
		}

		if err := z.TrustedClients.validate(where+".trusted_clients", name); err != nil {
			return err
		}

		if err := z.RateLimits.validate(where+".rate_limits", name); err != nil {
			return err
		}

		if err := z.validateEdge(where, name); err != nil {
			return err
		}
	}

	if c.Fleets.Internal.Service == "" {
		return fmt.Errorf("%s: fleets.internal.service is required", FileName)
	}

	if c.Fleets.Internal.Namespace == "" {
		return fmt.Errorf("%s: fleets.internal.namespace is required", FileName)
	}

	return c.validateR2()
}

// applyRootDefaults fills in defaultChildRotation for any child the
// registry leaves at "" — cloudflare.yaml need not carry a `root` block
// at all until the root stack's first apply.
func (c *Config) applyRootDefaults() {
	c.Root.applyDefaults()

	for name, a := range c.Accounts {
		if a.Root != nil {
			a.Root.applyDefaults()
			c.Accounts[name] = a
		}
	}
}

func (r *RootChildren) applyDefaults() {
	for _, cr := range []*ChildRotation{&r.Edge, &r.Status, &r.R2Admin, &r.R2Parent} {
		if cr.Rotation == "" {
			cr.Rotation = defaultChildRotation
		}
	}
}

// validateR2 checks every declared bucket: its account exists, its token is
// configured.
func (c *Config) validateR2() error {
	for _, name := range sortedKeys(c.R2) {
		b := c.R2[name]
		where := fmt.Sprintf("%s: r2[%s]", FileName, name)

		if !objectName.MatchString(name) {
			return fmt.Errorf("%s: the name becomes a token-store key and the Pulumi child names, so it must match %s", where, objectName)
		}

		if !b.Enabled {
			continue
		}

		if _, ok := c.Accounts[b.Account]; !ok {
			return fmt.Errorf("%s.account: no account %q (known: %s)", where, b.Account, strings.Join(sortedKeys(c.Accounts), ", "))
		}

		if b.Bucket == "" {
			return fmt.Errorf("%s.bucket is required", where)
		}

		if !cfnames.ValidJurisdiction(b.Jurisdiction) {
			return fmt.Errorf("%s.jurisdiction: %q is not allowed (valid: \"\", default, eu, fedramp, us)", where, b.Jurisdiction)
		}

		if b.Lifecycle != nil && b.Lifecycle.ExpireAfterDays <= 0 {
			return fmt.Errorf("%s.lifecycle.expire_after_days must be greater than zero", where)
		}

		if !slices.Contains(cfnames.R2Permissions(), b.Token.Permission) {
			return fmt.Errorf("%s.token.permission: %q is not allowed (valid: %s)", where, b.Token.Permission, strings.Join(cfnames.R2Permissions(), ", "))
		}
	}

	return nil
}

// Validate checks the registry and its tunnels. Tunnels are checked as
// given, so a hand-built config is held to the same ingress invariants as
// a derived one.
func (c *Config) Validate() error {
	if err := c.validateRegistry(); err != nil {
		return err
	}

	if len(c.Tunnels) == 0 {
		return fmt.Errorf("%s: no tunnel — the edge catalog has no group on a cloudflare-tunnel exposure", FileName)
	}

	seen := map[string]bool{}

	for i := range c.Tunnels {
		t := &c.Tunnels[i]

		where := fmt.Sprintf("%s: tunnel %s/%s", FileName, t.Cluster, t.Account)

		if seen[t.Cluster+"/"+t.Account] {
			return fmt.Errorf("%s: declared twice — one tunnel per cluster and account", where)
		}

		seen[t.Cluster+"/"+t.Account] = true

		account, ok := c.Accounts[t.Account]
		if !ok {
			return fmt.Errorf("%s: no such account", where)
		}

		if t.LegacyNames != account.LegacyNames {
			return fmt.Errorf("%s: legacy_names disagrees with the account's", where)
		}

		if !slices.Contains(t.InZones(), t.Zone) {
			return fmt.Errorf("%s: zone %q is not among the tunnel's zones %v", where, t.Zone, t.Zones)
		}

		for _, name := range t.InZones() {
			zone, ok := c.Zones[name]
			if !ok {
				return fmt.Errorf("%s: no zone %q", where, name)
			}

			if zone.Account != t.Account {
				return fmt.Errorf("%s: zone %s belongs to account %q, and a tunnel cannot cross accounts", where, name, zone.Account)
			}
		}

		if len(t.Hosts) == 0 {
			return fmt.Errorf("%s: carries no hostname", where)
		}

		for i, host := range t.Hosts {
			if strings.TrimSpace(host) == "" {
				return fmt.Errorf("%s: hosts[%d] is empty", where, i)
			}

			if got := c.ZoneOf(host); !slices.Contains(t.InZones(), got) {
				return fmt.Errorf("%s: hosts[%d] %q lies in zone %q, not the tunnel's zones %v", where, i, host, got, t.InZones())
			}
		}
	}

	if err := c.validateOriginTunnels(); err != nil {
		return err
	}

	if err := c.validateRecordTunnels(); err != nil {
		return err
	}

	return c.validateDomainRouting()
}

// validateDomainRouting enforces the tunnel ingress invariants cloudflared
// cannot check itself (unique hostnames, no shadowed rule): edgespec.ValidateRouting.
func (c *Config) validateDomainRouting() error {
	tunnels := make([]edgespec.Tunnel, 0, len(c.Tunnels))
	for i := range c.Tunnels {
		t := &c.Tunnels[i]

		tunnels = append(tunnels, edgespec.Tunnel{Cluster: t.Cluster, Account: t.Account, Hosts: t.Hosts})
	}

	if err := edgespec.ValidateRouting(tunnels); err != nil {
		return fmt.Errorf("%s: %w", FileName, err)
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

// cachesAnything reports whether any zone declares a cache policy.
func (c *Config) cachesAnything() bool {
	for name := range c.Zones {
		if c.Zones[name].Cache != nil {
			return true
		}
	}

	return false
}

// validate refuses a trusted-clients block whose rule would match nothing or too
// much, with the library's own check (cloudflare/zonespec).
func (t *TrustedClientsCfg) validate(where, zone string) error {
	if t == nil {
		return nil
	}

	if err := (&cfzone.Args{ZoneID: zone, TrustedClients: t.Library(zone)}).Validate(); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	return nil
}

// Library is the block as cloudflare/zonespec's input for zone zoneName, or nil.
func (t *TrustedClientsCfg) Library(zoneName string) *cfzone.TrustedClients {
	if t == nil {
		return nil
	}

	return &cfzone.TrustedClients{Zone: zoneName, Hosts: t.Hosts, Ranges: t.Ranges, Skip: t.Skip}
}

// Library is the block as cloudflare/zonespec's input.
func (r *RateLimitsCfg) Library() *cfzone.RateLimits {
	if r == nil {
		return nil
	}

	out := &cfzone.RateLimits{Plan: r.Plan, Rules: make([]cfzone.RateLimitRule, 0, len(r.Rules))}

	for i := range r.Rules {
		rule := &r.Rules[i]
		endpoints := make([]cfzone.Endpoint, 0, len(rule.Endpoints))
		for _, e := range rule.Endpoints {
			endpoints = append(endpoints, cfzone.Endpoint{Host: e.Host, Path: e.Path, PathPrefix: e.PathPrefix, Methods: e.Methods})
		}

		out.Rules = append(out.Rules, cfzone.RateLimitRule{
			Name:               rule.Name,
			Endpoints:          endpoints,
			Expression:         rule.Expression,
			CountingExpression: rule.CountingExpression,
			Characteristics:    rule.Characteristics,
			Period:             rule.Period,
			Requests:           rule.Requests,
			MitigationTimeout:  rule.MitigationTimeout,
			Action:             rule.Action,
		})
	}

	return out
}

// validate refuses a rate limit block the zone's plan does not allow (the
// library's own check, so the limits live in one place) and an endpoint
// outside the zone, whose rule would be applied and match nothing here.
func (r *RateLimitsCfg) validate(where, zoneName string) error {
	if r == nil {
		return nil
	}

	for i := range r.Rules {
		rule := &r.Rules[i]

		for j, e := range rule.Endpoints {
			if e.Host != zoneName && !strings.HasSuffix(e.Host, "."+zoneName) {
				return fmt.Errorf("%s.rules[%d].endpoints[%d].host %q must be the zone %s or a name inside it", where, i, j, e.Host, zoneName)
			}
		}
	}

	if err := (&cfzone.Args{ZoneID: "validate", RateLimits: r.Library()}).Validate(); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	return nil
}

// originTargetPattern is the only shape an origin_cnames value may take: an
// API Gateway regional domain name (`d-xxxxxxxxxx.execute-api.<region>.
// amazonaws.com`, the access stack's DomainTarget). A placeholder, a tunnel
// name or a typo fails at load, so a render or a test can never apply one.
var originTargetPattern = regexp.MustCompile(`^d-[a-z0-9]{6,}\.execute-api\.[a-z0-9-]+\.amazonaws\.com$`)

// ActiveOrigins is the zone's origin_cnames with the empty (tunnel) values
// dropped: the hosts whose CNAME leaves the tunnel.
func (z Zone) ActiveOrigins() map[string]string {
	if z.SecurityTxtOnly() {
		return nil
	}

	out := map[string]string{}

	for host, origin := range z.OriginCNAMEs {
		if origin != "" {
			out[host] = origin
		}
	}

	return out
}

func (z Zone) validateOriginCNAMEs(where, zone string) error {
	for _, host := range sortedKeys(z.OriginCNAMEs) {
		if host != strings.ToLower(host) || strings.ContainsAny(host, `*/:" `) || !strings.HasSuffix(host, "."+zone) {
			return fmt.Errorf("%s[%s]: the key must be a lowercase exact hostname inside zone %s", where, host, zone)
		}

		if origin := z.OriginCNAMEs[host]; origin != "" && !originTargetPattern.MatchString(origin) {
			return fmt.Errorf("%s[%s]: %q is not an API Gateway regional domain name "+
				"(d-<id>.execute-api.<region>.amazonaws.com); leave it empty to keep the tunnel", where, host, origin)
		}
	}

	return nil
}

// sslModeRuleModes are the SSL modes a configuration rule may set: the ones
// that encrypt toward the origin. `strict` also verifies its certificate.
var sslModeRuleModes = map[string]bool{"full": true, "strict": true}

// rulesetIDPattern is a Cloudflare ruleset id: 32 lowercase hex digits.
var rulesetIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// validateEdge holds the access edge's rows: the records, the origin
// switch, the HTTPS/HSTS/managed-WAF/security.txt hardening and the
// per-host SSL mode rules.
func (z Zone) validateEdge(where, zone string) error {
	if err := z.validateRecords(where+".records", zone); err != nil {
		return err
	}

	if err := z.validateRedirects(where+".redirects", zone); err != nil {
		return err
	}

	if err := z.validateOriginCNAMEs(where+".origin_cnames", zone); err != nil {
		return err
	}

	if err := z.validateHardening(where, zone); err != nil {
		return err
	}

	if err := z.PublicCache.validate(where+".public_cache", zone, z.Cache != nil); err != nil {
		return err
	}

	return z.validateSSLModeRules(where, zone)
}

// maxPublicCacheTTL mirrors rules.MaxPublicCacheTTL, which
// Contributions enforces again.
var maxPublicCacheTTL = time.Hour

// EdgeTTL is the parsed TTL; zero (the default) when unset.
func (p *PublicCacheCfg) EdgeTTL() time.Duration {
	if p == nil || p.TTL == "" {
		return 0
	}

	d, _ := time.ParseDuration(p.TTL)

	return d
}

func (p *PublicCacheCfg) validate(where, zone string, hasCache bool) error {
	if p == nil {
		return nil
	}

	if !hasCache {
		return fmt.Errorf("%s: set without cache, there is no cache ruleset to add the rule to", where)
	}

	if p.Host == "" || p.Host != strings.ToLower(p.Host) || strings.ContainsAny(p.Host, `*/:" `) || !strings.HasSuffix(p.Host, "."+zone) {
		return fmt.Errorf("%s.host: %q must be a lowercase exact hostname inside zone %s", where, p.Host, zone)
	}

	if p.TTL == "" {
		return nil
	}

	d, err := time.ParseDuration(p.TTL)
	if err != nil {
		return fmt.Errorf("%s.ttl: %w", where, err)
	}

	if d < time.Second || d > maxPublicCacheTTL || d%time.Second != 0 {
		return fmt.Errorf("%s.ttl: %s must be whole seconds between 1s and %s", where, d, maxPublicCacheTTL)
	}

	return nil
}

func (z Zone) validateSSLModeRules(where, zone string) error {
	for _, host := range sortedKeys(z.SSLModeRules) {
		if host != strings.ToLower(host) || strings.ContainsAny(host, `*/:" `) || !strings.HasSuffix(host, "."+zone) {
			return fmt.Errorf("%s.ssl_mode_rules[%s]: the key must be a lowercase exact hostname inside zone %s", where, host, zone)
		}

		if mode := z.SSLModeRules[host]; !sslModeRuleModes[mode] {
			return fmt.Errorf("%s.ssl_mode_rules[%s]: %q is not allowed (valid: full, strict)", where, host, mode)
		}
	}

	for _, host := range sortedKeys(z.SSLModeRuleRefs) {
		if _, ok := z.SSLModeRules[host]; !ok {
			return fmt.Errorf("%s.ssl_mode_rule_refs[%s]: no such host in ssl_mode_rules", where, host)
		}

		if !rulesetIDPattern.MatchString(z.SSLModeRuleRefs[host]) {
			return fmt.Errorf("%s.ssl_mode_rule_refs[%s]: %q is not a rule ref (32 lowercase hex digits)", where, host, z.SSLModeRuleRefs[host])
		}
	}

	if id := z.ConfigRulesImportID; id != "" {
		if len(z.SSLModeRules) == 0 {
			return fmt.Errorf("%s.config_rules_import_id: set without ssl_mode_rules, nothing would adopt it", where)
		}

		if !rulesetIDPattern.MatchString(id) {
			return fmt.Errorf("%s.config_rules_import_id: %q is not a ruleset id (32 lowercase hex digits)", where, id)
		}
	}

	return nil
}

// RecordsLibrary is the zone's records as the library takes them, or nil
// when the zone declares none.
func (z Zone) RecordsLibrary(zone string) *cfzone.Records {
	if z.SecurityTxtOnly() {
		return nil
	}

	if len(z.Records) == 0 {
		return nil
	}

	out := &cfzone.Records{Zone: zone, Entries: make([]cfzone.Record, 0, len(z.Records))}
	for _, r := range z.Records {
		out.Entries = append(out.Entries, cfzone.Record{
			Name: r.Name, Type: r.Type, Content: r.Content, Proxied: r.Proxied, TTL: r.TTL, Comment: r.Comment, WAF: r.WAF,
		})
	}

	return out
}

// RedirectsLibrary is the zone's canonical-host redirects as the library
// takes them, or nil when the zone declares none. The zone name is filled
// here because the file's `redirects` list sits under its zone key.
func (z Zone) RedirectsLibrary(zone string) *cfzone.Redirects {
	if z.SecurityTxtOnly() || len(z.Redirects) == 0 {
		return nil
	}

	out := &cfzone.Redirects{Zone: zone, Entries: make([]cfzone.Redirect, 0, len(z.Redirects))}
	for _, r := range z.Redirects {
		out.Entries = append(out.Entries, cfzone.Redirect{
			Ref: r.Ref, From: r.From, To: r.To, Status: r.Status,
			PreserveQueryString: r.PreserveQueryString, Disabled: r.Disabled,
		})
	}

	return out
}

// validateRecords holds the zone's records to the library's own rules, and
// keeps an NS delegation out of the WAF's vocabulary entirely.
func (z Zone) validateRecords(where, zone string) error {
	for _, r := range z.Records {
		if r.Type == "NS" && (r.WAF != nil || r.WAFReason != "") {
			return fmt.Errorf("%s[%s]: an NS delegation takes no WAF options; remove waf and waf_reason", where, r.Name)
		}
	}

	recs := z.RecordsLibrary(zone)
	if recs == nil {
		return nil
	}

	if err := (&cfzone.Args{ZoneID: zone, Records: recs}).Validate(); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	return nil
}

// validateRedirects holds the zone's redirects to the library's own rules,
// and keeps an import id to a phase the zone actually declares.
func (z Zone) validateRedirects(where, zone string) error {
	if id := z.RedirectRulesImportID; id != "" {
		if len(z.Redirects) == 0 {
			return fmt.Errorf("%s.redirect_rules_import_id: set without redirects, nothing would adopt it", where)
		}

		if !rulesetIDPattern.MatchString(id) {
			return fmt.Errorf("%s.redirect_rules_import_id: %q is not a ruleset id (32 lowercase hex digits)", where, id)
		}
	}

	redirects := z.RedirectsLibrary(zone)
	if redirects == nil {
		return nil
	}

	if err := (&cfzone.Args{ZoneID: zone, Redirects: redirects}).Validate(); err != nil {
		return fmt.Errorf("%s: %w", where, err)
	}

	return nil
}

// validateRecordTunnels refuses a record on a name a tunnel carries: the
// tunnel already owns that name's CNAME, and two owners of one record
// fight on every apply. A name the catalog marks dns_elsewhere is the
// exception: the tunnel writes no record for it, so a record here is its
// one writer.
//
// An NS delegation is stronger: the child zone cut takes the whole name and
// everything below it, so a tunnel host exactly at or anywhere under the
// delegation would be hidden from the parent zone. The tunnel would keep an
// ingress rule and lose its CNAME.
func (c *Config) validateRecordTunnels() error {
	for _, name := range sortedKeys(c.Zones) {
		for _, r := range c.Zones[name].Records {
			for i := range c.Tunnels {
				t := &c.Tunnels[i]

				if r.Type == "NS" {
					for _, host := range t.Hosts {
						if t.OwnsRecord(name, host) && atOrBelow(host, r.Name) {
							return fmt.Errorf("%s: zones[%s].records[%s]: tunnel %s writes %s at or below this NS delegation; "+
								"the child zone cut would hide the parent tunnel record", FileName, name, r.Name, t.Name(), host)
						}
					}

					continue
				}

				if t.OwnsRecord(name, r.Name) {
					return fmt.Errorf("%s: zones[%s].records[%s]: tunnel %s already carries this host (the edge catalog)",
						FileName, name, r.Name, t.Name())
				}
			}
		}
	}

	return nil
}

// atOrBelow reports whether host is the NS owner or a name under it. A
// leading "*." is removed first: a wildcard record below a delegation is
// hidden by the child zone cut just as an exact one is.
func atOrBelow(host, owner string) bool {
	host = strings.TrimPrefix(host, "*.")

	return host == owner || strings.HasSuffix(host, "."+owner)
}

// validateOriginTunnels holds every origin_cnames key to a host a tunnel
// carries and writes the record of: the switch moves a tunnel-managed
// record, and the rollback is the tunnel still serving the name. A
// dns_elsewhere host has no tunnel record to move.
func (c *Config) validateOriginTunnels() error {
	for _, name := range sortedKeys(c.Zones) {
		for _, host := range sortedKeys(c.Zones[name].OriginCNAMEs) {
			found := false

			for i := range c.Tunnels {
				t := &c.Tunnels[i]

				if t.OwnsRecord(name, host) {
					found = true
				}
			}

			if !found {
				return fmt.Errorf("%s: zones[%s].origin_cnames[%s]: no tunnel carries this host (the edge catalog), or its record is dns_elsewhere, "+
					"so there is no record to move and nothing to roll back to", FileName, name, host)
			}
		}
	}

	return nil
}

// Library is the block as cloudflare/zonespec's input, or nil.
func (h *HSTSCfg) Library() *cfzone.HSTS {
	if h == nil {
		return nil
	}

	return &cfzone.HSTS{
		Enabled: h.Enabled, MaxAge: h.MaxAge, IncludeSubdomains: h.IncludeSubdomains, Nosniff: h.Nosniff, Preload: h.Preload,
	}
}

// Library is the block as cloudflare/zonespec's input, or nil.
func (b *BotManagementCfg) Library() *cfzone.BotManagement {
	if b == nil {
		return nil
	}

	return &cfzone.BotManagement{AIBots: b.AIBots, ManagedRobotsTxt: b.ManagedRobotsTxt}
}

// Library is the block as cloudflare/zonespec's input for zone zoneName, or nil.
// For an enabled block it carries no hosts (they are derived: use
// Zone.ManagedWAFLibrary).
func (m *ManagedWAFCfg) Library(zoneName string) *cfzone.ManagedWAF {
	if m == nil {
		return nil
	}

	return &cfzone.ManagedWAF{Zone: zoneName, Hosts: m.Hosts, WholeZone: m.WholeZone}
}

// ManagedWAFLibrary is the zone's managed WAF as the library takes it, or nil:
// the derived WAFHosts when managed_waf is enabled, else the authored block.
func (z Zone) ManagedWAFLibrary(zoneName string) *cfzone.ManagedWAF {
	if z.ManagedWAF == nil || !z.ManagedWAF.Enabled {
		return z.ManagedWAF.Library(zoneName)
	}

	return &cfzone.ManagedWAF{Zone: zoneName, Hosts: z.WAFHosts}
}

// validateManagedWAF refuses the shapes managed_waf must not take: enabled
// beside a hand-written host list or whole_zone (one source), and a wildcard
// in hosts.
func (z Zone) validateManagedWAF(where string) error {
	m := z.ManagedWAF
	if m == nil {
		return nil
	}

	if m.Enabled && (len(m.Hosts) > 0 || m.WholeZone) {
		return fmt.Errorf("%s.managed_waf: enabled derives the hosts from the edge catalog; it is exclusive with hosts and whole_zone (one source)", where)
	}

	for i, h := range m.Hosts {
		if strings.Contains(h, "*") {
			return fmt.Errorf("%s.managed_waf.hosts[%d] %q: a wildcard is refused; the WAF lists exact names. "+
				"Use managed_waf: {enabled: true} and list the names on their catalog domains", where, i, h)
		}
	}

	return nil
}

// securityTxtContact is a contact URI security.txt accepts here: mail or a
// web page, nothing else (RFC 9116 allows tel:, which nobody here answers).
var securityTxtContact = regexp.MustCompile(`^(mailto:[^@\s]+@[a-z0-9.-]+\.[a-z]+|https://[a-z0-9.-]+(/\S*)?)$`)

// validateHardening holds the zone's HTTPS, HSTS, managed WAF and
// security.txt blocks to the library's rules, and to the estate's: HSTS is
// never preloaded, and is never sent by a zone that does not
// redirect plain HTTP first.
func (z Zone) validateHardening(where, zone string) error {
	if err := z.validateManagedWAF(where); err != nil {
		return err
	}

	// An enabled block's hosts are derived after validation; the rule is
	// validated, with its hosts, when the contribution is built.
	var wafLib *cfzone.ManagedWAF
	if z.ManagedWAF != nil && !z.ManagedWAF.Enabled {
		wafLib = z.ManagedWAF.Library(zone)
	}

	if z.AlwaysUseHTTPS != nil || z.HSTS != nil || wafLib != nil || z.BotManagement != nil {
		if err := (&cfzone.Args{
			ZoneID: zone, AlwaysUseHTTPS: z.AlwaysUseHTTPS, HSTS: z.HSTS.Library(), ManagedWAF: wafLib,
			BotManagement: z.BotManagement.Library(),
		}).Validate(); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
	}

	if z.HSTS != nil {
		if z.HSTS.Preload {
			return fmt.Errorf("%s.hsts.preload: refused (no preload; leaving the preload list takes months)", where)
		}

		if z.HSTS.Enabled && (z.AlwaysUseHTTPS == nil || !*z.AlwaysUseHTTPS) {
			return fmt.Errorf("%s.hsts: enabled needs always_use_https: true first (HSTS follows a clean HTTPS redirect)", where)
		}
	}

	if t := z.SecurityTxt; t != nil {
		if len(t.Contact) == 0 {
			return fmt.Errorf("%s.security_txt.contact: at least one contact is required (RFC 9116)", where)
		}

		for i, c := range t.Contact {
			if !securityTxtContact.MatchString(c) {
				return fmt.Errorf("%s.security_txt.contact[%d] %q: must be a mailto: address or an https:// URL", where, i, c)
			}
		}

		if _, err := time.Parse(time.RFC3339, t.Expires); err != nil {
			return fmt.Errorf("%s.security_txt.expires %q: must be an RFC 3339 timestamp (2026-10-13T00:00:00Z)", where, t.Expires)
		}
	}

	return nil
}
