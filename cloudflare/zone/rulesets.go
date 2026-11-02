package zone

import (
	"fmt"

	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	cfrules "github.com/truvity/edge/v4/cloudflare/rules"
)

// builtinKey identifies a rule the zone's own blocks (Cache, TrustedClients,
// ManagedWAF, RateLimits) contribute, as opposed to one in Args.Contributions.
type builtinKey struct {
	phase cfrules.Phase
	ref   string
}

// builtins are the rules the declaration blocks contribute, mapped to
// whether the rule is registered with enabled=true. Those registered before
// rules were assembled keep their inputs exactly: the cache and skip rules
// never set enabled, the managed and rate limit rules always did, and none
// carried a ref. A contributed rule is registered with its ref and enabled
// unless Disabled.
type builtins map[builtinKey]bool

// contributions is every contribution the zone assembles: the declaration
// blocks first, then Args.Contributions.
func contributions(args *Args) ([]cfrules.Contribution, builtins, error) {
	var out []cfrules.Contribution

	known := builtins{{cfrules.PhaseCacheSettings, cfrules.BypassRef}: false}

	add := func(c cfrules.Contribution, enabled bool) {
		for i := range c.Rules {
			known[builtinKey{c.Phase, c.Rules[i].Ref}] = enabled
		}

		out = append(out, c)
	}

	if args.Cache != nil {
		add(cfrules.CacheHosts(args.Cache.Hosts), false)
	}

	if args.TrustedClients.Managed() {
		c, err := cfrules.TrustedClients(args.TrustedClients)
		if err != nil {
			return nil, nil, fmt.Errorf("trusted clients: %w", err)
		}

		add(c, false)
	}

	if args.ManagedWAF.Managed() {
		c, err := cfrules.ManagedWAF(args.ManagedWAF)
		if err != nil {
			return nil, nil, fmt.Errorf("managed waf: %w", err)
		}

		add(c, true)
	}

	if args.RateLimits.Managed() {
		c, err := cfrules.RateLimits(args.RateLimits.Plan, args.RateLimits.Rules)
		if err != nil {
			return nil, nil, fmt.Errorf("rate limits: %w", err)
		}

		add(c, true)
	}

	if args.Redirects.Managed() {
		c, err := cfrules.Redirects(args.Redirects)
		if err != nil {
			return nil, nil, fmt.Errorf("redirects: %w", err)
		}

		// Not a builtin: a redirect keeps its stable ref and its enabled
		// flag in the provider's rule, so an adopted ruleset's refs are
		// preserved.
		out = append(out, c)
	}

	return append(out, args.Contributions...), known, nil
}

// rulesetChild is the child name of a phase's ruleset: a documented
// contract, like the other child names.
func rulesetChild(phase cfrules.Phase, name string) string {
	switch phase {
	case cfrules.PhaseCacheSettings:
		return "cache-rules-" + name
	case cfrules.PhaseFirewallCustom:
		return "firewall-custom-" + name
	case cfrules.PhaseFirewallManaged:
		return "firewall-managed-" + name
	case cfrules.PhaseRateLimit:
		return "ratelimit-" + name
	case cfrules.PhaseConfigSettings:
		return "ssl-mode-" + name
	case cfrules.PhaseDynamicRedirect:
		return "redirect-rules-" + name
	default:
		return "ruleset-" + string(phase) + "-" + name
	}
}

// rulesetResources are the phase resources a later part of the zone needs
// by identity. Only the dynamic redirect phase is needed today: a proxied
// DNS record of one of its source hosts must wait for it.
type rulesetResources struct {
	redirect *cloudflare.Ruleset
}

// newRulesets assembles every contribution and registers one ruleset per
// phase that has any.
func newRulesets(ctx *pulumi.Context, name string, args *Args, zoneID pulumi.String, child []pulumi.ResourceOption) (rulesetResources, error) {
	contribs, known, err := contributions(args)
	if err != nil {
		return rulesetResources{}, fmt.Errorf("zone %q: %w", name, err)
	}

	assembled, err := cfrules.Assemble(contribs)
	if err != nil {
		return rulesetResources{}, fmt.Errorf("zone %q: %w", name, err)
	}

	for phase, id := range args.ImportRulesets {
		if _, ok := assembled[phase]; !ok {
			return rulesetResources{}, fmt.Errorf("zone %q: importRulesets: the zone manages no %s ruleset to import %q into", name, phase, id)
		}
	}

	var resources rulesetResources

	for _, phase := range cfrules.Phases() {
		list, ok := assembled[phase]
		if !ok {
			continue
		}

		opts := child

		if id := args.ImportRulesets[phase]; id != "" {
			opts = append(append([]pulumi.ResourceOption(nil), opts...),
				pulumi.Import(pulumi.ID("zones/"+args.ZoneID+"/"+id)))
		}

		out := make(cloudflare.RulesetRuleArray, 0, len(list))
		for i := range list {
			enabled, builtin := known[builtinKey{phase, list[i].Ref}]
			out = append(out, providerRule(&list[i], builtin, enabled))
		}

		registered, err := cloudflare.NewRuleset(ctx, rulesetChild(phase, name), &cloudflare.RulesetArgs{
			ZoneId: zoneID,
			Kind:   pulumi.String(rulesetKind),
			Phase:  pulumi.String(string(phase)),
			Name:   pulumi.String("default"),
			Rules:  out,
		}, opts...)
		if err != nil {
			return rulesetResources{}, fmt.Errorf("zone %q: %s: %w", name, phase, err)
		}

		if phase == cfrules.PhaseDynamicRedirect {
			resources.redirect = registered
		}
	}

	return resources, nil
}

// providerRule maps a rule onto the provider's. A built-in rule keeps the
// inputs it had before rules were assembled (no ref, enabled as recorded).
func providerRule(r *cfrules.Rule, builtin, builtinEnabled bool) cloudflare.RulesetRuleArgs {
	out := cloudflare.RulesetRuleArgs{
		Action:      pulumi.String(r.Action),
		Description: pulumi.String(r.Description),
		Expression:  pulumi.String(r.Expression),
	}

	switch {
	case !builtin:
		out.Ref = pulumi.String(r.Ref)
		out.Enabled = pulumi.Bool(!r.Disabled)
	case builtinEnabled:
		out.Enabled = pulumi.Bool(true)
	}

	out.ActionParameters = actionParameters(&r.Params)

	if r.Logging {
		out.Logging = &cloudflare.RulesetRuleLoggingArgs{Enabled: pulumi.Bool(true)}
	}

	if l := r.RateLimit; l != nil {
		limit := &cloudflare.RulesetRuleRatelimitArgs{
			Characteristics:   pulumi.ToStringArray(l.Characteristics),
			Period:            pulumi.Int(l.Period),
			RequestsPerPeriod: pulumi.Int(l.Requests),
			MitigationTimeout: pulumi.Int(l.MitigationTimeout),
		}
		if l.CountingExpression != "" {
			limit.CountingExpression = pulumi.String(l.CountingExpression)
		}

		out.Ratelimit = limit
	}

	return out
}

func actionParameters(p *cfrules.Params) *cloudflare.RulesetRuleActionParametersArgs {
	switch {
	case p.Cache != nil:
		c := p.Cache
		out := &cloudflare.RulesetRuleActionParametersArgs{Cache: pulumi.Bool(c.Cache)}

		if c.EdgeTTL != nil {
			ttl := &cloudflare.RulesetRuleActionParametersEdgeTtlArgs{Mode: pulumi.String(c.EdgeTTL.Mode)}
			if c.EdgeTTL.Default > 0 {
				ttl.Default = pulumi.Int(c.EdgeTTL.Default)
			}

			out.EdgeTtl = ttl
		}

		if c.BrowserTTL != nil {
			ttl := &cloudflare.RulesetRuleActionParametersBrowserTtlArgs{Mode: pulumi.String(c.BrowserTTL.Mode)}
			if c.BrowserTTL.Default > 0 {
				ttl.Default = pulumi.Int(c.BrowserTTL.Default)
			}

			out.BrowserTtl = ttl
		}

		if c.RespectStrongETags {
			out.RespectStrongEtags = pulumi.Bool(true)
		}

		return out
	case p.Config != nil:
		return &cloudflare.RulesetRuleActionParametersArgs{Ssl: pulumi.String(p.Config.SSL)}
	case p.Skip != nil:
		// A part that is empty is left out: the skip action takes each one
		// optionally, and an empty ruleset string is not "current".
		out := &cloudflare.RulesetRuleActionParametersArgs{}
		if p.Skip.Ruleset != "" {
			out.Ruleset = pulumi.String(p.Skip.Ruleset)
		}

		if len(p.Skip.Phases) > 0 {
			out.Phases = pulumi.ToStringArray(p.Skip.Phases)
		}

		if len(p.Skip.Products) > 0 {
			out.Products = pulumi.ToStringArray(p.Skip.Products)
		}

		return out
	case p.Execute != nil:
		return &cloudflare.RulesetRuleActionParametersArgs{Id: pulumi.String(p.Execute.ID)}
	case p.Redirect != nil:
		r := p.Redirect

		return &cloudflare.RulesetRuleActionParametersArgs{
			FromValue: &cloudflare.RulesetRuleActionParametersFromValueArgs{
				PreserveQueryString: pulumi.Bool(r.PreserveQueryString),
				StatusCode:          pulumi.Int(r.StatusCode),
				TargetUrl: cloudflare.RulesetRuleActionParametersFromValueTargetUrlArgs{
					Expression: pulumi.String(r.TargetURL),
				},
			},
		}
	}

	return nil
}
