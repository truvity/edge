// Package zonestack is the zone base stack of one company: per Cloudflare
// account of the company a provider, and per zone of those accounts the
// zone component (cloudflare/zone) with every setting and ruleset the
// registry (cloudflare/config) declares for it. It registers no tunnel; the
// tunnels are the services stack (cloudflare/services).
//
// The names it registers are the names the single edge stack
// (cloudflare/edge plus an estate's glue) registers today, so that splitting
// that stack's state into a zone stack and a services stack replaces nothing.
// docs/cloudflare/stacks.md lists them; the golden test in this package runs
// both and compares every resource's type, name, parent and inputs.
//
// Mechanism only, like the rest of the library: the registry, the resolved
// tokens and the estate's own resource options come in through Args.
package zonestack

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/account"
	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/rules"
	"github.com/truvity/edge/v4/cloudflare/tunnel"
	"github.com/truvity/edge/v4/cloudflare/zone"
)

// AOPProviderPrefix names the provider of a zone's Authenticated Origin Pulls
// setting and configuration rules: "aop-provider-<zone slug>". The name
// predates the SSL mode rules and stays: renaming a provider replaces every
// resource it serves.
const AOPProviderPrefix = "aop-provider-"

// Args is the whole zone base stack of one company.
type Args struct {
	// Config is the loaded registry (config.Load). Required.
	Config *config.Config
	// Company selects the accounts: those whose Account.Company is Company,
	// or, for an account that sets none, whose own name is (config.CompanyOf).
	// Required.
	Company string
	// Tokens maps each of the company's accounts to its already-resolved API
	// token. Reading it is a plain call at program time, the caller's.
	Tokens map[string]string
	// Contributions are further rules for a zone, beside the ones its
	// registry blocks ask for (config.Zone.Contributions). Optional.
	Contributions func(zone string) []rules.Contribution
	// AccountOptions adds resource options (typically provider aliases for a
	// legacy provider name) to an account's provider. ZoneOptions adds them to
	// a zone component. Both optional.
	AccountOptions func(account string) []pulumi.ResourceOption
	ZoneOptions    func(zone string) []pulumi.ResourceOption

	// Owners restricts the stack to the zones whose config.Zone.Owner is
	// listed. Optional.
	Owners []string

	Logger *slog.Logger
}

// ErrSecurityTxtOnly is what ZoneArgs returns for a zone with manage:
// security_txt: such a zone is managed for its security.txt alone, by
// cloudflare/securitytxt, and registers no resource.
var ErrSecurityTxtOnly = errors.New("manage: security_txt zone registers no resource")

// Deploy registers the stack.
func Deploy(ctx *pulumi.Context, args Args) error {
	if args.Config == nil {
		return fmt.Errorf("zonestack: Config is required")
	}

	accounts := args.Config.AccountsOf(args.Company)
	if len(accounts) == 0 {
		return fmt.Errorf("zonestack: company %q has no account in the registry", args.Company)
	}

	logger := args.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	providers := make(map[string]*account.Account, len(accounts))

	for _, name := range accounts {
		token := args.Tokens[name]
		if token == "" {
			return fmt.Errorf("account %s: no resolved API token (the caller must resolve it before Deploy)", name)
		}

		var opts []pulumi.ResourceOption
		if args.AccountOptions != nil {
			opts = args.AccountOptions(name)
		}

		acct, err := account.New(ctx, name, account.Args{AccountID: args.Config.Accounts[name].AccountID}, pulumi.String(token), opts...)
		if err != nil {
			return fmt.Errorf("account %s: %w", name, err)
		}

		providers[name] = acct
	}

	zones := 0

	for _, name := range sortedZones(args.Config) {
		z := args.Config.Zones[name]

		acct, ok := providers[z.Account]
		if !ok {
			continue
		}

		// manage: security_txt: the zone is only named so that the
		// security.txt tool reaches it; nothing is registered for it.
		if z.SecurityTxtOnly() {
			continue
		}

		if z.ZoneIDPending {
			return fmt.Errorf("zone %s is zone_id_pending: the account %q's stacks need its zone id", name, z.Account)
		}

		if err := deployZone(ctx, args, name, z, acct); err != nil {
			return fmt.Errorf("zone %s: %w", name, err)
		}

		zones++
	}

	logger.InfoContext(ctx.Context(), "cloudflare zone stack deployed",
		slog.String("company", args.Company),
		slog.Int("accounts", len(accounts)),
		slog.Int("zones", zones),
	)

	return nil
}

func deployZone(ctx *pulumi.Context, args Args, name string, z config.Zone, acct *account.Account) error {
	slug := tunnel.Slug(name)

	zoneArgs, err := ZoneArgs(name, z)
	if err != nil {
		return err
	}

	if args.Contributions != nil {
		zoneArgs.Contributions = append(zoneArgs.Contributions, args.Contributions(name)...)
	}

	opts := []pulumi.ResourceOption{acct.Use()}

	// The two resources that need an origin-pull mode or a configuration
	// rule are served by a provider of their own, beside the account's, as
	// they are today. A zone that needs neither declares no such provider.
	if aop := needsAOP(z); aop {
		token := args.Tokens[z.Account]

		p, err := account.New(ctx, AOPProviderPrefix+slug,
			account.Args{AccountID: args.Config.Accounts[z.Account].AccountID}, pulumi.String(token))
		if err != nil {
			return fmt.Errorf("cloudflare account: %w", err)
		}

		opts = append(opts, pulumi.Transformations([]pulumi.ResourceTransformation{aopProvider(slug, p)}))
	}

	if args.ZoneOptions != nil {
		opts = append(opts, args.ZoneOptions(name)...)
	}

	_, err = zone.New(ctx, slug, zoneArgs, opts...)

	return err
}

// needsAOP reports whether the zone registers an Authenticated Origin Pulls
// setting or configuration rules.
func needsAOP(z config.Zone) bool {
	return len(z.ActiveOrigins()) > 0 || len(z.SSLModeRules) > 0
}

// aopProvider binds the origin-pull setting and the configuration ruleset of
// the zone with the given slug to its provider p.
func aopProvider(slug string, p *account.Account) pulumi.ResourceTransformation {
	return func(ta *pulumi.ResourceTransformationArgs) *pulumi.ResourceTransformationResult {
		if ta.Name != "tls-client-auth-"+slug && ta.Name != "ssl-mode-"+slug {
			return nil
		}

		return &pulumi.ResourceTransformationResult{Props: ta.Props, Opts: append(ta.Opts, p.Use())}
	}
}

// ZoneArgs maps one zone of the registry onto the zone component's input:
// SSL mode, browser TLS floor, Total TLS, the cache policy with its derived
// hosts, the zone's own records and child-zone delegations, canonical-host
// redirects, the HTTPS redirect, HSTS, Authenticated Origin Pulls while some
// host has left its tunnel for another origin, the adopted configuration or
// redirect ruleset, and the rules of the registry's blocks as
// the library builds them (config.Zone.Contributions: public cache, SSL
// mode, rate limits, trusted clients, managed WAF), with their refs. A
// ruleset an estate registered in another form takes one in-place update
// when it adopts these (docs/cloudflare/stacks.md).
//
// A zone managed for its security.txt only (manage: security_txt) has no zone
// component: ZoneArgs returns ErrSecurityTxtOnly for it, and a caller that
// loops over the registry skips it (Deploy does).
func ZoneArgs(name string, z config.Zone) (zone.Args, error) {
	if z.SecurityTxtOnly() {
		return zone.Args{}, fmt.Errorf("zone %s: %w", name, ErrSecurityTxtOnly)
	}

	z.Name = name

	out := zone.Args{
		ZoneID:         z.ZoneID,
		SSL:            z.SSLMode,
		MinTLSVersion:  z.MinTLSVersion,
		AlwaysUseHTTPS: z.AlwaysUseHTTPS,
		HSTS:           z.HSTS.Library(),
		BotManagement:  z.BotManagement.Library(),
		Records:        z.RecordsLibrary(name),
		Redirects:      z.RedirectsLibrary(name),
	}

	if z.TotalTLS != nil {
		out.TotalTLS = &zone.TotalTLS{
			Enabled:              z.TotalTLS.Enabled,
			CertificateAuthority: z.TotalTLS.CertificateAuthority,
		}
	}

	// The hosts are derived from the catalog (config.Load). An empty list is
	// a policy: the zone caches nothing.
	if z.Cache != nil {
		out.Cache = &zone.Cache{Hosts: z.CacheHosts, RespectOriginBrowserTTL: z.Cache.RespectOriginBrowserTTL}
	}

	if len(z.ActiveOrigins()) > 0 {
		on := true
		out.AuthenticatedOriginPulls = &on
	}

	if z.ConfigRulesImportID != "" || z.RedirectRulesImportID != "" {
		out.ImportRulesets = map[rules.Phase]string{}

		if z.ConfigRulesImportID != "" {
			out.ImportRulesets[rules.PhaseConfigSettings] = z.ConfigRulesImportID
		}

		if z.RedirectRulesImportID != "" {
			out.ImportRulesets[rules.PhaseDynamicRedirect] = z.RedirectRulesImportID
		}
	}

	contributions, err := z.Contributions()
	if err != nil {
		return zone.Args{}, err
	}

	out.Contributions = contributions

	return out, nil
}

func sortedZones(c *config.Config) []string {
	out := make([]string, 0, len(c.Zones))
	for name := range c.Zones {
		out = append(out, name)
	}

	sort.Strings(out)

	return out
}
