// Package zonespec holds the pure side of a zone's settings: the declaration
// types (Args and its blocks), their validation, and the Cloudflare filter
// expressions rendered from them. No Cloudflare API call, no Pulumi
// resource, no SDK import of any kind: an estate's config layer that only
// validates a zone's declaration links this package and not the Pulumi and
// Cloudflare SDKs that cloudflare/zone imports to apply it. cloudflare/zone re-exports
// every type here as an alias, so existing callers keep compiling.
//
// TestNoNonStdlibImports fails the moment this package imports anything
// outside the standard library.
package zonespec

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/truvity/edge/v4/cloudflare/rulespec"
)

// Shortest prefixes a trusted range may have: anything broader is close to
// "the internet" and defeats the rule's purpose.
const (
	minTrustedBitsV4 = 8
	minTrustedBitsV6 = 16
)

// DefaultTrustedDescription describes the trusted-clients rule when
// TrustedClients.Description is empty.
const DefaultTrustedDescription = "Trusted clients: skip bot and rate-limit checks"

// LegacyTrustedDescription describes the trusted-clients rule when Skip is
// set and Description is empty.
const LegacyTrustedDescription = "Trusted clients: skip selected security features"

// What the trusted-clients rule can skip, by the name Cloudflare's skip
// action takes. TrustedSkipCurrent is the rest of the zone's custom rules
// (`ruleset: current`); the phases and products are the skip action's own.
const (
	TrustedSkipCurrent = "current"

	TrustedSkipRateLimit = "http_ratelimit"
	TrustedSkipManaged   = "http_request_firewall_managed"
	TrustedSkipSBFM      = "http_request_sbfm"

	TrustedSkipBIC           = "bic"
	TrustedSkipHot           = "hot"
	TrustedSkipLegacyRate    = "rateLimit"
	TrustedSkipSecurityLevel = "securityLevel"
	TrustedSkipUABlock       = "uaBlock"
	TrustedSkipLegacyWAF     = "waf"
	TrustedSkipZoneLockdown  = "zoneLockdown"
)

// trustedSkipPhases and trustedSkipProducts are every name Skip accepts.
var (
	trustedSkipPhases   = []string{TrustedSkipRateLimit, TrustedSkipManaged, TrustedSkipSBFM}
	trustedSkipProducts = []string{
		TrustedSkipBIC, TrustedSkipHot, TrustedSkipLegacyRate, TrustedSkipSecurityLevel,
		TrustedSkipUABlock, TrustedSkipLegacyWAF, TrustedSkipZoneLockdown,
	}
)

// DefaultTrustedSkip is what a trusted client skips when Skip is empty: the
// bot and rate-limit checks only. The managed rules, the legacy WAF, the
// zone lockdown and the rest of the custom rules still apply to it.
func DefaultTrustedSkip() []string {
	return []string{
		TrustedSkipRateLimit, TrustedSkipSBFM,
		TrustedSkipBIC, TrustedSkipHot, TrustedSkipLegacyRate, TrustedSkipSecurityLevel, TrustedSkipUABlock,
	}
}

// LegacyTrustedSkip is what a trusted client skipped before Skip existed:
// everything. Set Skip to it to keep that behaviour.
func LegacyTrustedSkip() []string {
	out := []string{TrustedSkipCurrent}
	out = append(out, trustedSkipPhases...)

	return append(out, trustedSkipProducts...)
}

// TrustedSkipSplit sorts Skip (empty means DefaultTrustedSkip) into the skip
// action's ruleset, phases and products, each in a fixed order. It reports
// the first name that is none of them.
func TrustedSkipSplit(skip []string) (ruleset string, phases, products []string, err error) {
	if len(skip) == 0 {
		skip = DefaultTrustedSkip()
	}

	set := map[string]bool{}
	for _, name := range skip {
		if name != TrustedSkipCurrent && !slices.Contains(trustedSkipPhases, name) && !slices.Contains(trustedSkipProducts, name) {
			return "", nil, nil, fmt.Errorf("skip %q is not a phase (%s), a product (%s) or %q",
				name, strings.Join(trustedSkipPhases, ", "), strings.Join(trustedSkipProducts, ", "), TrustedSkipCurrent)
		}

		if set[name] {
			return "", nil, nil, fmt.Errorf("skip %q is listed twice", name)
		}

		set[name] = true
	}

	if set[TrustedSkipCurrent] {
		ruleset = TrustedSkipCurrent
	}

	for _, name := range trustedSkipPhases {
		if set[name] {
			phases = append(phases, name)
		}
	}

	for _, name := range trustedSkipProducts {
		if set[name] {
			products = append(products, name)
		}
	}

	return ruleset, phases, products, nil
}

var (
	validSSL           = []string{"off", "flexible", "full", "strict"}
	validMinTLSVersion = []string{"1.0", "1.1", "1.2", "1.3"}
	validCA            = []string{"google", "lets_encrypt", "ssl_com"}
)

type (
	// TotalTLS asks the zone to issue a certificate for every proxied A,
	// AAAA or CNAME record, so a new hostname needs no certificate
	// resource of its own. Requires Advanced Certificate Manager.
	TotalTLS struct {
		// Enabled turns per-hostname issuance on. Required.
		Enabled bool `json:"enabled" yaml:"enabled"`
		// CertificateAuthority to issue through: google, lets_encrypt or
		// ssl_com. Empty leaves Cloudflare's default.
		CertificateAuthority string `json:"certificateAuthority,omitempty" yaml:"certificateAuthority,omitempty"`
	}

	// Cache decides WHICH of a zone's hostnames may be cached at all, and
	// leaves HOW LONG to the origin that serves them.
	//
	// The default it replaces is the reason it exists. A zone caches by
	// file extension: a response under a name ending in .js or .css is
	// stored at the edge whether or not its origin asked for that, and
	// where the origin sent no Cache-Control the zone tells the browser
	// to keep it for four hours. An application that answers its own
	// unknown paths — a single-page console serving its shell under every
	// name — then has one page cached under another page's name, and
	// nobody wrote that down anywhere.
	//
	// So a zone with this block caches the hostnames it lists and nothing
	// else, and each of those only as far as its own Cache-Control goes.
	// A response with no Cache-Control is not cached. Which means the
	// decision belongs where it can be read: in the application that
	// serves the bytes and knows whether they are content-addressed.
	Cache struct {
		// Hosts whose responses may be cached: an exact name
		// ("app.example") or one leading wildcard label
		// ("*.app.example").
		//
		// An EMPTY list is a policy and not an omission — the zone
		// caches nothing at all, which is what a zone serving only
		// applications that authenticate every request wants. To leave
		// caching unmanaged, omit the block.
		Hosts []string `json:"hosts" yaml:"hosts"`
		// RespectOriginBrowserTTL sets the zone setting Browser Cache
		// TTL to "Respect Existing Headers".
		//
		// The rules above decide what the EDGE stores. This is the
		// `Cache-Control` Cloudflare writes on the way out when the
		// origin sent none, and left alone it claims four hours for
		// every response of the zone — including the ones no rule here
		// caches, which is a browser holding what the edge would not.
		// False leaves the zone's setting as it is.
		RespectOriginBrowserTTL bool `json:"respectOriginBrowserTtl,omitempty" yaml:"respectOriginBrowserTtl,omitempty"`
	}

	// TrustedClients exempts client address ranges from the zone's
	// Cloudflare security features on named hosts: ONE zone custom-firewall
	// rule with action `skip`. A request to one of Hosts from an address in
	// Ranges skips the bot and rate-limit checks: rate limiting, Super Bot
	// Fight Mode and the legacy bic, hot, rateLimit, securityLevel and
	// uaBlock products (see Skip). It still meets the managed rules, the
	// legacy WAF, the zone lockdown and the rest of the custom rules.
	//
	// For a known machine-to-machine caller whose egress is published (a
	// connector platform's address range): it keeps that caller from ever
	// meeting a challenge a later rule might introduce. It is not a
	// workaround for a rule that blocks it today.
	//
	// Both lists empty manages no ruleset at all. One empty and the other
	// not is refused: the rule would match nothing.
	TrustedClients struct {
		// Zone is the zone's domain ("example.com"). Required when Hosts
		// is not empty: every host must be the zone itself or a name
		// inside it.
		Zone string `json:"zone,omitempty" yaml:"zone,omitempty"`
		// Hosts are exact lower-case hostnames inside Zone. No wildcards.
		Hosts []string `json:"hosts" yaml:"hosts"`
		// Ranges are CIDR blocks, IPv4 or IPv6, with no host bits set.
		// Shorter than /8 (IPv4) or /16 (IPv6) is refused as too broad.
		Ranges []string `json:"ranges" yaml:"ranges"`
		// Description of the rule, shown in the dashboard and in security
		// events. Empty uses "Trusted clients: skip bot and rate-limit
		// checks" (or "...skip selected security features" when Skip is
		// set).
		Description string `json:"description,omitempty" yaml:"description,omitempty"`
		// Skip names what a trusted client skips, from the skip action's
		// vocabulary: the phases http_ratelimit, http_request_sbfm and
		// http_request_firewall_managed, the products bic, hot, rateLimit,
		// securityLevel, uaBlock, waf and zoneLockdown, and "current" (the
		// rest of the custom rules). Empty is DefaultTrustedSkip: bot and
		// rate-limit checks only. A non-empty list replaces the default;
		// LegacyTrustedSkip() is the old everything. An unknown or repeated
		// name is refused.
		Skip []string `json:"skip,omitempty" yaml:"skip,omitempty"`
	}

	// Args is one zone's settings. Every field is optional: an unset
	// field is a setting this estate does not manage.
	Args struct {
		// ZoneID the settings apply to. Required.
		ZoneID string `json:"zoneId" yaml:"zoneId"`
		// SSL is how Cloudflare connects to the origin: off, flexible,
		// full or strict. "full" encrypts to the origin and accepts any
		// certificate; "strict" also verifies it, which needs a
		// certificate the public CAs or Cloudflare's own origin CA
		// signed.
		SSL string `json:"ssl,omitempty" yaml:"ssl,omitempty"`
		// MinTLSVersion a browser must offer: 1.0, 1.1, 1.2 or 1.3.
		MinTLSVersion string `json:"minTlsVersion,omitempty" yaml:"minTlsVersion,omitempty"`
		// TotalTLS issues a certificate per proxied hostname. Nil leaves
		// the zone alone.
		TotalTLS *TotalTLS `json:"totalTls,omitempty" yaml:"totalTls,omitempty"`
		// Cache is which hostnames may be cached at all. Nil leaves the
		// zone's caching as it is, extensions and all.
		Cache *Cache `json:"cache,omitempty" yaml:"cache,omitempty"`
		// TrustedClients is the zone's one skip rule for known client
		// ranges. Nil, or both lists empty, manages no firewall ruleset.
		TrustedClients *TrustedClients `json:"trustedClients,omitempty" yaml:"trustedClients,omitempty"`
		// RateLimits is the zone's whole http_ratelimit ruleset. Nil
		// leaves the phase alone; non-nil owns it, replacing any rule
		// created elsewhere.
		RateLimits *RateLimits `json:"rateLimits,omitempty" yaml:"rateLimits,omitempty"`
		// Records are DNS records the zone serves for something other than
		// a tunnel (a load balancer, a CDN). Nil, or no entries, manages
		// no record; records not listed here are never touched.
		Records *Records `json:"records,omitempty" yaml:"records,omitempty"`
		// Redirects are the zone's canonical-host Single Redirects: the
		// whole http_request_dynamic_redirect phase. Nil, or no entries,
		// leaves the phase alone; a block owns it.
		Redirects *Redirects `json:"redirects,omitempty" yaml:"redirects,omitempty"`
		// AlwaysUseHTTPS is the zone setting always_use_https: true
		// answers every plain-HTTP request with a redirect to HTTPS,
		// false turns that off. Nil leaves the setting alone.
		AlwaysUseHTTPS *bool `json:"alwaysUseHttps,omitempty" yaml:"alwaysUseHttps,omitempty"`
		// HSTS is the zone setting security_header (Strict-Transport-
		// Security). Nil leaves the setting alone.
		HSTS *HSTS `json:"hsts,omitempty" yaml:"hsts,omitempty"`
		// ManagedWAF deploys the Cloudflare Managed Ruleset on the listed
		// hosts. Nil leaves the http_request_firewall_managed phase
		// alone; non-nil owns it.
		ManagedWAF *ManagedWAF `json:"managedWaf,omitempty" yaml:"managedWaf,omitempty"`
		// BotManagement is the zone's AI bots rule and managed robots.txt
		// (the bot_management setting, only those two fields). Nil leaves
		// the setting alone.
		BotManagement *BotManagement `json:"botManagement,omitempty" yaml:"botManagement,omitempty"`
		// Contributions are further rules the zone's rulesets take, from
		// the builders in cloudflare/rules (public-paths caching, an SSL
		// mode for one host, ...). They are assembled together with the
		// blocks above, one ruleset per phase. Set in code, not read from
		// configuration.
		Contributions []rulespec.Contribution `json:"-" yaml:"-"`
		// AuthenticatedOriginPulls is the zone setting tls_client_auth:
		// true makes Cloudflare present its shared origin-pull client
		// certificate to every origin; false turns that off. Nil leaves
		// the setting alone.
		AuthenticatedOriginPulls *bool `json:"authenticatedOriginPulls,omitempty" yaml:"authenticatedOriginPulls,omitempty"`
		// ImportRulesets adopts a phase's existing entry point ruleset by
		// its id instead of creating it, for a zone whose ruleset was made
		// by hand. A phase listed here must be one the zone manages. Drop
		// the entry once the import is applied.
		ImportRulesets map[rulespec.Phase]string `json:"importRulesets,omitempty" yaml:"importRulesets,omitempty"`
	}

	// Records is a set of DNS records the estate owns inside one zone.
	// Only the listed records are managed: the zone's other records, made
	// by hand, by an import or by a tunnel, stay as they are.
	Records struct {
		// Zone is the zone's domain ("example.com"). Required when
		// Entries is not empty: every name must be the zone itself or a
		// name inside it.
		Zone string `json:"zone,omitempty" yaml:"zone,omitempty"`
		// Entries are the records, in any order.
		Entries []Record `json:"entries" yaml:"entries"`
	}

	// Record is one DNS record.
	Record struct {
		// Name is the record's lower-case hostname inside the zone; a
		// leading "*." makes it a wildcard. An NS record is a child-zone
		// delegation owner: an exact name below the zone, never the apex
		// and never a wildcard.
		Name string `json:"name" yaml:"name"`
		// Type is A, AAAA, CNAME or NS. NS is DNS-only: it delegates a
		// child zone and takes no proxy, WAF or certificate.
		Type string `json:"type" yaml:"type"`
		// Content is the IPv4 address (A), IPv6 address (AAAA), or the
		// lower-case target hostname without a trailing dot (CNAME and
		// NS). An NS content is one nameserver hostname; the same owner
		// may carry several, each its own record.
		Content string `json:"content" yaml:"content"`
		// Proxied serves the name through Cloudflare's edge (orange
		// cloud); false answers DNS only. NS is always DNS only.
		Proxied bool `json:"proxied,omitempty" yaml:"proxied,omitempty"`
		// WAF says whether a proxied record's name is covered by the
		// zone's derived managed WAF. Nil is on; only false opts the name
		// out (WAFCovered). Meaningless on a DNS-only record, and refused
		// on NS.
		WAF *bool `json:"waf,omitempty" yaml:"waf,omitempty"`
		// TTL in seconds for a DNS-only record, 60 to 86400; 0 is
		// Cloudflare's automatic TTL. A proxied record is always
		// automatic, so it must leave TTL at 0. An NS record needs an
		// explicit TTL, and every target of one owner shares it.
		TTL int `json:"ttl,omitempty" yaml:"ttl,omitempty"`
		// Comment is shown beside the record in the dashboard.
		Comment string `json:"comment,omitempty" yaml:"comment,omitempty"`
	}

	// Redirects is a zone's canonical-host redirects: the WHOLE
	// http_request_dynamic_redirect phase. Nil or no entries leaves the phase
	// unmanaged; a block owns it, so a rule added in the dashboard is
	// replaced. A zone has one entry point ruleset per phase: adopt an
	// existing one with Args.ImportRulesets rather than creating a second.
	//
	// Each match is exactly one host, on HTTP and HTTPS and every path. The
	// redirect answers with To plus the request's path and, by default, its
	// query string. It needs no regex and no WAF feature.
	Redirects struct {
		// Zone is the zone's domain ("example.com"). Required when Entries
		// is not empty: every From must be the zone itself or a name inside
		// it.
		Zone string `json:"zone,omitempty" yaml:"zone,omitempty"`
		// Entries are the redirects, in any order. They are rendered sorted
		// by From, then Ref, so the plan never depends on authoring order.
		Entries []Redirect `json:"entries" yaml:"entries"`
	}

	// Redirect is one exact-host redirect. Its expression is
	// http.host eq From; it therefore covers HTTP and HTTPS, and every
	// path.
	Redirect struct {
		// Ref is the rule's stable ref, unique in the phase
		// (www_to_apex). A hand-made rule's ref is preserved when its
		// ruleset is adopted with Args.ImportRulesets.
		Ref string `json:"ref" yaml:"ref"`
		// From is the exact lower-case source hostname inside Zone. No
		// wildcard, no scheme, no path.
		From string `json:"from" yaml:"from"`
		// To is the HTTPS destination origin: https://<host>, with no
		// path, port, query or fragment. The request's path is appended
		// to it, so the destination keeps the path.
		To string `json:"to" yaml:"to"`
		// Status is the redirect status: 301 (the default), 302, 307 or
		// 308.
		Status int `json:"status,omitempty" yaml:"status,omitempty"`
		// PreserveQueryString keeps the request's query string. Nil
		// means true.
		PreserveQueryString *bool `json:"preserveQueryString,omitempty" yaml:"preserveQueryString,omitempty"`
		// Disabled keeps the rule and its ref while making it inert: the
		// staged rollback (restore the source DNS, then disable the
		// rule, then remove it) does not have to delete the ruleset.
		Disabled bool `json:"disabled,omitempty" yaml:"disabled,omitempty"`
	}
)

// WAFCovered reports whether the record's name belongs in the derived
// managed WAF host set: it is proxied and has not opted out.
func (r Record) WAFCovered() bool { return r.Proxied && (r.WAF == nil || *r.WAF) }

// Record types Records accepts, and the DNS-only TTL range.
var validRecordTypes = []string{"A", "AAAA", "CNAME", "NS"}

const (
	minRecordTTL = 60
	maxRecordTTL = 86400
)

// Managed reports whether r declares any record.
func (r *Records) Managed() bool {
	return r != nil && len(r.Entries) > 0
}

// validate reports the first problem with a records block: a name outside
// the zone, content that does not fit its type, or two records Cloudflare
// would refuse to hold together (a CNAME shares its name with nothing, and
// an NS delegation is the only record set at its owner).
func (r *Records) validate(zoneID string) error {
	if !r.Managed() {
		return nil
	}

	if r.Zone == "" || r.Zone != strings.ToLower(r.Zone) {
		return fmt.Errorf("zone %q: records.zone must be the zone's lower-case domain", zoneID)
	}

	seen := map[string]struct{}{}
	cname := map[string]bool{}
	named := map[string]bool{}
	nsOwner := map[string]bool{}
	nsTTL := map[string]int{}

	for i := range r.Entries {
		e := &r.Entries[i]
		where := fmt.Sprintf("zone %q: records.entries[%d] %s %s", zoneID, i, e.Name, e.Type)

		bare := strings.TrimPrefix(e.Name, "*.")
		if e.Name != strings.ToLower(e.Name) || bare == "" || strings.ContainsAny(bare, `*/:"\ `) ||
			strings.HasSuffix(e.Name, ".") || (bare != r.Zone && !strings.HasSuffix(bare, "."+r.Zone)) {
			return fmt.Errorf("%s: name must be a lower-case hostname inside %s (a leading \"*.\" for a wildcard)", where, r.Zone)
		}

		if !oneOf(e.Type, validRecordTypes) {
			return fmt.Errorf("%s: type must be one of %v", where, validRecordTypes)
		}

		if err := validateRecordContent(e.Type, e.Content); err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}

		if e.Type == "NS" {
			if strings.Contains(e.Name, "*") {
				return fmt.Errorf("%s: a child-zone delegation cannot be a wildcard; NS owners are exact names", where)
			}

			if e.Name == r.Zone {
				return fmt.Errorf("%s: the zone apex's NS set is Cloudflare's own; a child-zone delegation must be a name below %s", where, r.Zone)
			}

			if e.Proxied {
				return fmt.Errorf("%s: an NS delegation is DNS only; leave proxied false", where)
			}

			if e.WAF != nil {
				return fmt.Errorf("%s: an NS delegation is not in the managed WAF; remove waf", where)
			}

			if e.TTL < minRecordTTL || e.TTL > maxRecordTTL {
				return fmt.Errorf("%s: an NS record needs an explicit ttl (%d to %d); automatic TTL is not a delegation", where, minRecordTTL, maxRecordTTL)
			}

			if prev, ok := nsTTL[e.Name]; ok && prev != e.TTL {
				return fmt.Errorf("%s: this NS set's ttl %d disagrees with %d set by an earlier target: one owner's targets share a ttl", where, e.TTL, prev)
			}

			nsTTL[e.Name] = e.TTL

			if named[e.Name] && !nsOwner[e.Name] {
				return fmt.Errorf("%s: an NS delegation cannot share its name with another record", where)
			}

			nsOwner[e.Name] = true
		} else if nsOwner[e.Name] {
			return fmt.Errorf("%s: an NS delegation cannot share its name with another record", where)
		}

		if e.Proxied && e.TTL != 0 {
			return fmt.Errorf("%s: a proxied record's TTL is automatic; leave ttl at 0", where)
		}

		if e.TTL != 0 && (e.TTL < minRecordTTL || e.TTL > maxRecordTTL) {
			return fmt.Errorf("%s: ttl %d must be 0 (automatic) or %d to %d", where, e.TTL, minRecordTTL, maxRecordTTL)
		}

		key := e.Name + " " + e.Type + " " + e.Content
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%s: %s is listed twice", where, e.Content)
		}

		seen[key] = struct{}{}

		if cname[e.Name] || (e.Type == "CNAME" && named[e.Name]) {
			return fmt.Errorf("%s: a CNAME cannot share its name with another record", where)
		}

		named[e.Name] = true
		cname[e.Name] = cname[e.Name] || e.Type == "CNAME"
	}

	return nil
}

// validateRecordContent holds a record's content to its type.
func validateRecordContent(typ, content string) error {
	switch typ {
	case "A", "AAAA":
		addr, err := netip.ParseAddr(content)
		if err != nil || addr.Is4() != (typ == "A") || addr.Zone() != "" {
			return fmt.Errorf("content %q is not an IPv%s address", content, map[bool]string{true: "4", false: "6"}[typ == "A"])
		}
	case "CNAME", "NS":
		what := "target hostname"
		if typ == "NS" {
			what = "nameserver hostname"

			if _, err := netip.ParseAddr(content); err == nil {
				return fmt.Errorf("content %q must be a lower-case %s without a trailing dot, not an address", content, what)
			}
		}

		if content == "" || content != strings.ToLower(content) || strings.HasSuffix(content, ".") ||
			strings.ContainsAny(content, `*/:"\ `) || !strings.Contains(content, ".") {
			return fmt.Errorf("content %q must be a lower-case %s without a trailing dot", content, what)
		}
	}

	return nil
}

func oneOf(value string, allowed []string) bool {
	for _, a := range allowed {
		if value == a {
			return true
		}
	}

	return false
}

// redirectRefPattern is a stable rule ref: lowercase, starting with a letter,
// never longer than Cloudflare's own limit.
var redirectRefPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// redirectStatuses are the statuses a Single Redirect takes.
var redirectStatuses = []int{301, 302, 307, 308}

// Managed reports whether r declares any redirect. Nil and an empty block
// manage no phase.
func (r *Redirects) Managed() bool {
	return r != nil && len(r.Entries) > 0
}

// HasSource reports whether host is one of the block's exact source hosts.
// It is what the zone component uses to make a proxied source record wait
// for the redirect ruleset.
func (r *Redirects) HasSource(host string) bool {
	if r == nil {
		return false
	}

	for i := range r.Entries {
		if r.Entries[i].From == host {
			return true
		}
	}

	return false
}

// validate reports the first problem with a redirects block: a source host
// outside the zone or a wildcard, a destination that is not an HTTPS origin,
// a repeated ref or source, an unsupported status, or a loop between the
// declared redirects.
func (r *Redirects) validate(zoneID string) error {
	if !r.Managed() {
		return nil
	}

	if r.Zone == "" || r.Zone != strings.ToLower(r.Zone) {
		return fmt.Errorf("zone %q: redirects.zone must be the zone's lower-case domain", zoneID)
	}

	refs := map[string]int{}
	froms := map[string]int{}
	destinations := map[string]string{}

	for i := range r.Entries {
		e := &r.Entries[i]
		where := fmt.Sprintf("zone %q: redirects.entries[%d] %s", zoneID, i, e.Ref)

		if !redirectRefPattern.MatchString(e.Ref) {
			return fmt.Errorf("%s: ref must be lowercase letters, digits, \"_\" or \"-\", start with a letter and be at most 64 characters", where)
		}

		if prev, dup := refs[e.Ref]; dup {
			return fmt.Errorf("%s: ref is already used by entry %d: a ref is stable and unique in the phase", where, prev)
		}

		refs[e.Ref] = i

		if e.From == "" || e.From != strings.ToLower(e.From) || strings.HasSuffix(e.From, ".") ||
			strings.ContainsAny(e.From, `*/:"\ `) || (e.From != r.Zone && !strings.HasSuffix(e.From, "."+r.Zone)) {
			return fmt.Errorf("%s.from: %q must be a lower-case exact hostname inside %s (no wildcard)", where, e.From, r.Zone)
		}

		if prev, dup := froms[e.From]; dup {
			return fmt.Errorf("%s.from: %q is already used by entry %d: one redirect per source host", where, e.From, prev)
		}

		froms[e.From] = i

		dest, err := validateRedirectTo(e.To)
		if err != nil {
			return fmt.Errorf("%s.to: %w", where, err)
		}

		destinations[e.From] = dest

		if e.Status != 0 && !slices.Contains(redirectStatuses, e.Status) {
			return fmt.Errorf("%s.status: %d is not a redirect status (valid: %v)", where, e.Status, redirectStatuses)
		}
	}

	for i := range r.Entries {
		start := r.Entries[i].From
		if loop := redirectLoop(start, destinations); len(loop) > 0 {
			return fmt.Errorf("zone %q: redirects: %s", zoneID, strings.Join(loop, " -> "))
		}
	}

	return nil
}

// redirectLoop walks the declared source-to-destination graph from start and
// returns the cycle it enters, or nil. A destination that is not another
// declared source ends the walk: that request leaves the declaration.
func redirectLoop(start string, destinations map[string]string) []string {
	seen := map[string]bool{start: true}
	path := []string{start}
	cur := start

	for {
		dest, ok := destinations[cur]
		if !ok {
			return nil
		}

		path = append(path, dest)

		if seen[dest] {
			return append(path, "redirect loop")
		}

		seen[dest] = true
		cur = dest
	}
}

// validateRedirectTo holds a destination to an HTTPS origin and returns its
// host: https://<host>, with no path, port, query or fragment. The request's
// path is appended to it by the rule, so a path here would be duplicated.
func validateRedirectTo(to string) (string, error) {
	u, err := url.Parse(to)
	if err != nil {
		return "", fmt.Errorf("%q is not a URL", to)
	}

	if u.Scheme != "https" {
		return "", fmt.Errorf("%q must be an HTTPS destination (https://<host>)", to)
	}

	if u.User != nil || u.Port() != "" || u.Path != "" || u.RawPath != "" || u.RawQuery != "" ||
		u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return "", fmt.Errorf("%q must be an HTTPS origin (https://<host>) with no path, port, query or fragment", to)
	}

	host := u.Hostname()
	if host == "" || host != strings.ToLower(host) || strings.HasSuffix(host, ".") ||
		strings.ContainsAny(host, `*/:"\ `) || !strings.Contains(host, ".") {
		return "", fmt.Errorf("%q must name a lower-case hostname", to)
	}

	return host, nil
}

// Validate reports the first problem with args.
func (a *Args) Validate() error {
	if a.ZoneID == "" {
		return fmt.Errorf("zone: zoneId is required")
	}

	if a.SSL != "" && !oneOf(a.SSL, validSSL) {
		return fmt.Errorf("zone %q: ssl %q must be one of %v", a.ZoneID, a.SSL, validSSL)
	}

	if a.MinTLSVersion != "" && !oneOf(a.MinTLSVersion, validMinTLSVersion) {
		return fmt.Errorf("zone %q: minTlsVersion %q must be one of %v", a.ZoneID, a.MinTLSVersion, validMinTLSVersion)
	}

	if a.TotalTLS != nil {
		ca := a.TotalTLS.CertificateAuthority
		if ca != "" && !oneOf(ca, validCA) {
			return fmt.Errorf("zone %q: totalTls.certificateAuthority %q must be one of %v", a.ZoneID, ca, validCA)
		}
	}

	if a.Cache != nil {
		if err := a.Cache.validate(a.ZoneID); err != nil {
			return err
		}
	}

	if a.TrustedClients != nil {
		if err := a.TrustedClients.validate(a.ZoneID); err != nil {
			return err
		}
	}

	if a.RateLimits != nil {
		if err := a.RateLimits.validate(a.ZoneID); err != nil {
			return err
		}
	}

	if err := a.Records.validate(a.ZoneID); err != nil {
		return err
	}

	if err := a.Redirects.validate(a.ZoneID); err != nil {
		return err
	}

	if a.HSTS != nil {
		if err := a.HSTS.validate(a.ZoneID, a.AlwaysUseHTTPS); err != nil {
			return err
		}
	}

	if a.ManagedWAF != nil {
		if err := a.ManagedWAF.validate(a.ZoneID); err != nil {
			return err
		}
	}

	if a.BotManagement != nil {
		if err := a.BotManagement.validate(a.ZoneID); err != nil {
			return err
		}
	}

	for phase, id := range a.ImportRulesets {
		if phase == "" || id == "" || strings.ContainsAny(id, "/ \t\n") {
			return fmt.Errorf("zone %q: importRulesets[%q] %q must be a phase and a ruleset id", a.ZoneID, phase, id)
		}
	}

	if a.SSL == "" && a.MinTLSVersion == "" && a.TotalTLS == nil && a.Cache == nil &&
		!a.TrustedClients.Managed() && !a.RateLimits.Managed() && !a.Records.Managed() &&
		!a.Redirects.Managed() && a.AlwaysUseHTTPS == nil && a.HSTS == nil &&
		!a.ManagedWAF.Managed() && a.BotManagement == nil &&
		a.AuthenticatedOriginPulls == nil && len(a.Contributions) == 0 {
		return fmt.Errorf("zone %q: nothing to apply — omit the zone instead of declaring one that manages no setting", a.ZoneID)
	}

	return nil
}

// validate reports the first problem with a cache policy. Every rule here
// is a host that would be written into a Cloudflare filter expression and
// then match nothing, which is the failure this component exists to make
// impossible: a rule that is applied, reported healthy, and silent.
func (c *Cache) validate(zoneID string) error {
	seen := make(map[string]struct{}, len(c.Hosts))

	for i, host := range c.Hosts {
		switch {
		case host == "":
			return fmt.Errorf("zone %q: cache.hosts[%d] is empty", zoneID, i)

		case strings.ToLower(host) != host:
			return fmt.Errorf(
				"zone %q: cache.hosts[%d] %q must be lower case: Cloudflare compares a lower-cased host, so this rule would match nothing",
				zoneID, i, host)

		case strings.ContainsAny(host, `/:"\ `):
			return fmt.Errorf("zone %q: cache.hosts[%d] %q must be a hostname, not a URL", zoneID, i, host)

		case host == "*." || strings.Contains(strings.TrimPrefix(host, "*."), "*"):
			return fmt.Errorf(
				"zone %q: cache.hosts[%d] %q: a wildcard is one leading label and a name after it, as in \"*.app.example\"",
				zoneID, i, host)
		}

		if _, duplicate := seen[host]; duplicate {
			return fmt.Errorf("zone %q: cache.hosts[%d] %q is listed twice", zoneID, i, host)
		}

		seen[host] = struct{}{}
	}

	return nil
}

// Managed reports whether the block declares a rule. Nil and an empty
// block manage nothing.
func (t *TrustedClients) Managed() bool {
	return t != nil && (len(t.Hosts) > 0 || len(t.Ranges) > 0)
}

// validate reports the first problem with a trusted-clients block: a rule
// that would match nothing, or too much.
func (t *TrustedClients) validate(zoneID string) error {
	if !t.Managed() {
		return nil
	}

	if len(t.Hosts) == 0 || len(t.Ranges) == 0 {
		return fmt.Errorf("zone %q: trustedClients.hosts and ranges must both be non-empty (leave both empty to manage no rule)", zoneID)
	}

	if t.Zone == "" || t.Zone != strings.ToLower(t.Zone) {
		return fmt.Errorf("zone %q: trustedClients.zone must be the zone's lower-case domain", zoneID)
	}

	if _, _, _, err := TrustedSkipSplit(t.Skip); err != nil {
		return fmt.Errorf("zone %q: trustedClients.%w", zoneID, err)
	}

	seen := map[string]struct{}{}

	for i, host := range t.Hosts {
		if host != strings.ToLower(host) || strings.ContainsAny(host, `*/:"\ `) || host == "" ||
			(host != t.Zone && !strings.HasSuffix(host, "."+t.Zone)) {
			return fmt.Errorf("zone %q: trustedClients.hosts[%d] %q must be a lower-case exact hostname inside %s", zoneID, i, host, t.Zone)
		}

		if _, dup := seen[host]; dup {
			return fmt.Errorf("zone %q: trustedClients.hosts[%d] %q is listed twice", zoneID, i, host)
		}

		seen[host] = struct{}{}
	}

	seen = map[string]struct{}{}

	for i, r := range t.Ranges {
		prefix, err := netip.ParsePrefix(r)
		if err != nil || prefix.Masked() != prefix || prefix.Addr().Is4In6() {
			return fmt.Errorf("zone %q: trustedClients.ranges[%d] %q must be a CIDR block with no host bits set", zoneID, i, r)
		}

		if (prefix.Addr().Is4() && prefix.Bits() < minTrustedBitsV4) || (prefix.Addr().Is6() && prefix.Bits() < minTrustedBitsV6) {
			return fmt.Errorf("zone %q: trustedClients.ranges[%d] %q is too broad to trust (shortest allowed: /%d IPv4, /%d IPv6)",
				zoneID, i, r, minTrustedBitsV4, minTrustedBitsV6)
		}

		if _, dup := seen[r]; dup {
			return fmt.Errorf("zone %q: trustedClients.ranges[%d] %q is listed twice", zoneID, i, r)
		}

		seen[r] = struct{}{}
	}

	return nil
}

// TrustedExpression matches a request to one of the hosts from an address
// in one of the ranges. Both sets are sorted, so the expression never
// depends on authoring order.
func TrustedExpression(t *TrustedClients) string {
	return fmt.Sprintf("(http.host in {%s}) and (ip.src in {%s})", sortedSet(t.Hosts, true), sortedSet(t.Ranges, false))
}

// sortedSet renders a Cloudflare set literal body: host sets take quoted
// strings, IP sets bare addresses.
func sortedSet(values []string, quote bool) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)

	for i, v := range sorted {
		if quote {
			sorted[i] = fmt.Sprintf("%q", v)
		}
	}

	return strings.Join(sorted, " ")
}

// HostExpression is the Cloudflare filter that matches the listed hosts.
// Exact names go in one set literal; each wildcard becomes a suffix test,
// because a set literal takes no wildcards.
func HostExpression(hosts []string) string {
	var (
		exact []string
		terms []string
	)

	for _, host := range hosts {
		if suffix, isWildcard := strings.CutPrefix(host, "*."); isWildcard {
			terms = append(terms, fmt.Sprintf("ends_with(http.host, %q)", "."+suffix))

			continue
		}

		exact = append(exact, fmt.Sprintf("%q", host))
	}

	if len(exact) > 0 {
		terms = append([]string{fmt.Sprintf("http.host in {%s}", strings.Join(exact, " "))}, terms...)
	}

	return strings.Join(terms, " or ")
}
