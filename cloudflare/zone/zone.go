// Package zone applies the handful of zone-level settings an estate
// actually decides: how Cloudflare talks to the origin, the floor it will
// negotiate with a browser, whether every proxied hostname gets a
// certificate without anyone asking for one, the DNS records (including
// DNS-only child-zone NS delegations), and canonical-host Single Redirects.
//
// This is deliberately small. A Cloudflare zone has scores of settings and
// most are defaults nobody should be managing from a deployment tool; the
// few here change what traffic is and is not accepted, and which of it is
// stored at the edge, so they belong in the same review as the routes that
// depend on them.
//
// Total TLS is the reason this package exists. Without it every proxied
// hostname needs its own Advanced Certificate pack, so adding a hostname
// means creating a certificate resource and waiting for validation. With
// it, the zone issues per-hostname certificates itself and adding a
// hostname is a DNS record and nothing else. It requires Advanced
// Certificate Manager on the zone's plan, which is why it is opt-in.
//
// Rate limiting rules protect public endpoints from a single noisy client.
// The plan decides how many rules a zone may hold and what they can count,
// so the plan is part of the declaration and every limit is checked before
// the apply, not after it.
//
// The library's contract, shared by every public truvity module:
//
//   - Mechanism only. Nothing here knows a zone id or a domain; Args is a
//     plain yaml-taggable struct a consumer unmarshals its own config into.
//   - Credentials come in. The provider is the caller's — pass it with
//     pulumi.Provider, or account.Use() from this module's account package.
//   - Nothing implicit. An unset field is not managed: a zone whose SSL
//     mode this estate does not decide keeps whatever it has.
package zone

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/zonespec"
)

// Setting identifiers, as Cloudflare names them.
const (
	settingSSL             = "ssl"
	settingMinTLSVersion   = "min_tls_version"
	settingBrowserCacheTTL = "browser_cache_ttl"
	settingAlwaysUseHTTPS  = "always_use_https"
	settingSecurityHeader  = "security_header"
	settingTLSClientAuth   = "tls_client_auth"
)

// Every phase's ruleset is the zone's whole entry point ruleset for that
// phase (see rulesets.go): a zone that manages a phase owns it, and rules
// added beside these in the dashboard are not merged with them, they are
// replaced.
const (
	rulesetKind = "zone"

	// Browser Cache TTL, the zone setting: 0 is Cloudflare's "Respect
	// Existing Headers".
	browserCacheTTLRespectHeaders = 0
)

// The declaration types and their validation live in cloudflare/zonespec, which
// links no Pulumi or Cloudflare SDK; they are aliased here so existing
// callers keep compiling. A caller that only validates a declaration should
// import cloudflare/zonespec directly.
type (
	// TotalTLS asks the zone to issue a certificate for every proxied A,
	// AAAA or CNAME record. See zonespec.TotalTLS.
	TotalTLS = zonespec.TotalTLS
	// Cache decides WHICH of a zone's hostnames may be cached at all. See
	// zonespec.Cache.
	Cache = zonespec.Cache
	// TrustedClients exempts client address ranges on named hosts. See
	// zonespec.TrustedClients.
	TrustedClients = zonespec.TrustedClients
	// Records are DNS records the estate owns in the zone. See
	// zonespec.Records.
	Records = zonespec.Records
	// Record is one of them. See zonespec.Record.
	Record = zonespec.Record
	// Redirects is the zone's canonical-host Single Redirects phase. See
	// zonespec.Redirects.
	Redirects = zonespec.Redirects
	// Redirect is one of them. See zonespec.Redirect.
	Redirect = zonespec.Redirect
	// Args is one zone's settings. See zonespec.Args.
	Args = zonespec.Args
	// RateLimits is the zone's whole http_ratelimit ruleset. See
	// zonespec.RateLimits.
	RateLimits = zonespec.RateLimits
	// RateLimitRule is one rate limiting rule. See zonespec.RateLimitRule.
	RateLimitRule = zonespec.RateLimitRule
	// Endpoint is one host and path a rate limit protects. See
	// zonespec.Endpoint.
	Endpoint = zonespec.Endpoint
	// HSTS is the zone's Strict-Transport-Security header. See
	// zonespec.HSTS.
	HSTS = zonespec.HSTS

	// BotManagement is the zone's AI bots rule and managed robots.txt. See
	// zonespec.BotManagement.
	BotManagement = zonespec.BotManagement
	// ManagedWAF deploys the Cloudflare Managed Ruleset on named hosts.
	// See zonespec.ManagedWAF.
	ManagedWAF = zonespec.ManagedWAF
)

// CloudflareManagedRulesetID is the ruleset ManagedWAF executes. See
// zonespec.CloudflareManagedRulesetID.
const CloudflareManagedRulesetID = zonespec.CloudflareManagedRulesetID

// Plan names, as Cloudflare names the zone plans this library models.
const (
	PlanFree     = zonespec.PlanFree
	PlanPro      = zonespec.PlanPro
	PlanBusiness = zonespec.PlanBusiness
)

type (
	// Zone is the applied settings for one zone.
	Zone struct {
		pulumi.ResourceState

		// ZoneID the settings were applied to.
		ZoneID pulumi.StringOutput `pulumi:"zoneId"`
	}
)

// New applies one zone's settings. Children are named "setting-<name>-ssl",
// "setting-<name>-min-tls-version", "setting-<name>-browser-cache-ttl",
// "setting-<name>-always-use-https", "setting-<name>-security-header",
// "total-tls-<name>", "cache-rules-<name>", "firewall-custom-<name>",
// "firewall-managed-<name>", "ratelimit-<name>", "redirect-rules-<name>"
// and one "record-<name>-..." per record (RecordName), a documented
// contract: consumers alias existing resources onto these names.
func New(ctx *pulumi.Context, name string, args Args, opts ...pulumi.ResourceOption) (*Zone, error) {
	if err := args.Validate(); err != nil {
		return nil, err
	}

	comp := &Zone{}
	if err := ctx.RegisterComponentResource("truvity:cloudflare:Zone", name, comp, opts...); err != nil {
		return nil, err
	}

	child := []pulumi.ResourceOption{pulumi.Parent(comp)}
	zoneID := pulumi.String(args.ZoneID)

	if args.SSL != "" {
		if _, err := cloudflare.NewZoneSetting(ctx, "setting-"+name+"-ssl", &cloudflare.ZoneSettingArgs{
			ZoneId:    zoneID,
			SettingId: pulumi.String(settingSSL),
			Value:     pulumi.String(args.SSL),
		}, child...); err != nil {
			return nil, fmt.Errorf("zone %q: ssl: %w", name, err)
		}
	}

	if args.MinTLSVersion != "" {
		if _, err := cloudflare.NewZoneSetting(ctx, "setting-"+name+"-min-tls-version", &cloudflare.ZoneSettingArgs{
			ZoneId:    zoneID,
			SettingId: pulumi.String(settingMinTLSVersion),
			Value:     pulumi.String(args.MinTLSVersion),
		}, child...); err != nil {
			return nil, fmt.Errorf("zone %q: minTlsVersion: %w", name, err)
		}
	}

	if args.AlwaysUseHTTPS != nil {
		value := "off"
		if *args.AlwaysUseHTTPS {
			value = "on"
		}

		if _, err := cloudflare.NewZoneSetting(ctx, "setting-"+name+"-always-use-https", &cloudflare.ZoneSettingArgs{
			ZoneId:    zoneID,
			SettingId: pulumi.String(settingAlwaysUseHTTPS),
			Value:     pulumi.String(value),
		}, child...); err != nil {
			return nil, fmt.Errorf("zone %q: alwaysUseHttps: %w", name, err)
		}
	}

	if args.HSTS != nil {
		if _, err := cloudflare.NewZoneSetting(ctx, "setting-"+name+"-security-header", &cloudflare.ZoneSettingArgs{
			ZoneId:    zoneID,
			SettingId: pulumi.String(settingSecurityHeader),
			Value:     hstsValue(args.HSTS),
		}, child...); err != nil {
			return nil, fmt.Errorf("zone %q: hsts: %w", name, err)
		}
	}

	if b := args.BotManagement; b != nil {
		// Only the managed fields are sent; the rest of the object (Bot
		// Fight Mode, SBFM, JS detections) is left unset.
		bArgs := &cloudflare.BotManagementArgs{ZoneId: zoneID}
		if b.AIBots != "" {
			bArgs.AiBotsProtection = pulumi.String(b.AIBots)
		}

		if b.ManagedRobotsTxt != nil {
			bArgs.IsRobotsTxtManaged = pulumi.Bool(*b.ManagedRobotsTxt)
		}

		if _, err := cloudflare.NewBotManagement(ctx, "bot-management-"+name, bArgs, child...); err != nil {
			return nil, fmt.Errorf("zone %q: botManagement: %w", name, err)
		}
	}

	if args.TotalTLS != nil {
		tArgs := &cloudflare.TotalTlsArgs{
			ZoneId:  zoneID,
			Enabled: pulumi.Bool(args.TotalTLS.Enabled),
		}
		if args.TotalTLS.CertificateAuthority != "" {
			tArgs.CertificateAuthority = pulumi.String(args.TotalTLS.CertificateAuthority)
		}

		if _, err := cloudflare.NewTotalTls(ctx, "total-tls-"+name, tArgs, child...); err != nil {
			return nil, fmt.Errorf("zone %q: totalTls: %w", name, err)
		}
	}

	if args.AuthenticatedOriginPulls != nil {
		value := "off"
		if *args.AuthenticatedOriginPulls {
			value = "on"
		}

		if _, err := cloudflare.NewZoneSetting(ctx, "tls-client-auth-"+name, &cloudflare.ZoneSettingArgs{
			ZoneId:    zoneID,
			SettingId: pulumi.String(settingTLSClientAuth),
			Value:     pulumi.String(value),
		}, child...); err != nil {
			return nil, fmt.Errorf("zone %q: authenticatedOriginPulls: %w", name, err)
		}
	}

	rulesets, err := newRulesets(ctx, name, &args, zoneID, child)
	if err != nil {
		return nil, err
	}

	if args.Cache != nil && args.Cache.RespectOriginBrowserTTL {
		if _, err := cloudflare.NewZoneSetting(ctx, "setting-"+name+"-browser-cache-ttl", &cloudflare.ZoneSettingArgs{
			ZoneId:    zoneID,
			SettingId: pulumi.String(settingBrowserCacheTTL),
			Value:     pulumi.Int(browserCacheTTLRespectHeaders),
		}, child...); err != nil {
			return nil, fmt.Errorf("zone %q: browser cache ttl: %w", name, err)
		}
	}

	if args.Records.Managed() {
		for i := range args.Records.Entries {
			r := &args.Records.Entries[i]

			rArgs := &cloudflare.DnsRecordArgs{
				ZoneId:  zoneID,
				Name:    pulumi.String(r.Name),
				Type:    pulumi.String(r.Type),
				Content: pulumi.String(r.Content),
				Proxied: pulumi.Bool(r.Proxied),
				// 1 = automatic, the only TTL a proxied record takes.
				Ttl: pulumi.Float64(recordTTL(r)),
			}
			if r.Comment != "" {
				rArgs.Comment = pulumi.String(r.Comment)
			}

			// The explicit rule-before-proxy dependency: a proxied source
			// record of a declared redirect waits for the redirect
			// ruleset's apply. A DNS-only source stages the rule inert and
			// needs no dependency.
			recordOpts := child
			if r.Proxied && rulesets.redirect != nil && args.Redirects.HasSource(r.Name) {
				recordOpts = append(append([]pulumi.ResourceOption(nil), child...),
					pulumi.DependsOn([]pulumi.Resource{rulesets.redirect}))
			}

			if _, err := cloudflare.NewDnsRecord(ctx, RecordName(name, r), rArgs, recordOpts...); err != nil {
				return nil, fmt.Errorf("zone %q: record %s %s: %w", name, r.Name, r.Type, err)
			}
		}
	}

	comp.ZoneID = zoneID.ToStringOutput()

	if err := ctx.RegisterResourceOutputs(comp, pulumi.Map{
		"zoneId": comp.ZoneID,
	}); err != nil {
		return nil, err
	}

	return comp, nil
}

// hstsValue is the security_header setting's value. The keys are the
// Cloudflare API's own (snake_case): the value is a dynamic object the
// provider passes through as written, and the camelCase spelling in the
// provider's generated docs never converges (pulumi/pulumi-cloudflare#1579).
func hstsValue(h *HSTS) pulumi.Map {
	return pulumi.Map{
		"strict_transport_security": pulumi.Map{
			"enabled":            pulumi.Bool(h.Enabled),
			"max_age":            pulumi.Int(h.MaxAge),
			"include_subdomains": pulumi.Bool(h.IncludeSubdomains),
			"nosniff":            pulumi.Bool(h.Nosniff),
			"preload":            pulumi.Bool(h.Preload),
		},
	}
}

// RecordName is the child name of one record: "record-<name>-<host-slug>-cname"
// for a CNAME (one per name), and "record-<name>-<host-slug>-<type>-<content-slug>"
// for an A, AAAA or NS (several may share a name; an NS set is one per
// target). A documented contract, like the other child names.
func RecordName(name string, r *Record) string {
	out := "record-" + name + "-" + recordSlug(r.Name) + "-" + strings.ToLower(r.Type)
	if r.Type != "CNAME" {
		out += "-" + recordSlug(r.Content)
	}

	return out
}

// recordSlug renders a hostname or address as a resource-name fragment
// ("*.apps.example.com" -> "star-apps-example-com", "2001:db8::1" ->
// "2001-db8--1").
func recordSlug(s string) string {
	return strings.NewReplacer("*", "star", ".", "-", ":", "-").Replace(s)
}

// recordTTL is the TTL Cloudflare is given: 1 (automatic) for a proxied
// record or an unset one, else the declared seconds.
func recordTTL(r *Record) float64 {
	if r.Proxied || r.TTL == 0 {
		return 1
	}

	return float64(r.TTL)
}
