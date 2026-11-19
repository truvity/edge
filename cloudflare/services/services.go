// Package services is the services stack of one company: per Cloudflare
// account of the company a provider, and per cluster and account one
// remotely managed tunnel (cloudflare/tunnel) with its ingress rules and the
// proxied CNAME of every name the tunnel writes DNS for. It registers no
// zone setting; those are the zone base stack (cloudflare/zonestack).
//
// The tunnels are the registry's (config.Config.Tunnels), which config.Load
// derived from the edge catalog: one per (cluster, account), hosts in
// ingress order. A name the catalog marks dns_elsewhere keeps its ingress
// rule and gets no CNAME. A host whose zone declares origin_cnames has its
// CNAME's content rewritten to that origin in place.
//
// The registered names are those of the single edge stack
// (cloudflare/edge plus an estate's glue): see docs/cloudflare/stacks.md.
// Each tunnel's token goes to the caller's [TokenSink], which decides where
// it lives.
package services

import (
	"fmt"
	"log/slog"

	cf "github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/account"
	"github.com/truvity/edge/v4/cloudflare/config"
	"github.com/truvity/edge/v4/cloudflare/edge"
	"github.com/truvity/edge/v4/cloudflare/tunnel"
)

const dnsRecordType = "cloudflare:index/dnsRecord:DnsRecord"

type (
	// Tunnel is one tunnel of the registry.
	Tunnel = config.Tunnel

	// Cluster describes one cluster the tunnels deliver to.
	Cluster struct {
		Name string
		// DNSDomain is the cluster's Kubernetes DNS domain, for the origin
		// service address. Empty is "cluster.<name>".
		DNSDomain string
	}

	// Origin is the in-cluster service every ingress rule routes to.
	Origin struct {
		Service   string
		Namespace string
		// Port is the HTTPS port of Service that every ingress rule dials.
		// Zero is edge.DefaultOriginPort.
		Port int
	}

	// TokenSink stores one tunnel's token where its cluster reads it. The
	// token is the library's secret Output.
	TokenSink func(ctx *pulumi.Context, t Tunnel, token pulumi.StringOutput) error

	// Args is the whole services stack of one company.
	Args struct {
		// Config is the loaded registry (config.Load); its derived tunnels are
		// the stack's tunnels. Required.
		Config *config.Config
		// Company selects the accounts, as in zonestack. Required.
		Company string
		// Clusters gives a cluster's Kubernetes DNS domain. Every tunnel of
		// the company is deployed whether or not its cluster is listed; an
		// unlisted cluster has the default "cluster.<name>".
		Clusters []Cluster
		// Origin is the service the ingress rules route to. With no Service
		// and no Namespace, both are the registry's fleets.internal, and Port
		// is kept.
		Origin Origin
		// CAPoolPath defaults to edge.DefaultCAPoolPath.
		CAPoolPath string
		// Tokens maps each of the company's accounts to its already-resolved
		// API token.
		Tokens map[string]string
		// Sink receives each tunnel's token. Required.
		Sink TokenSink

		// AccountOptions and TunnelOptions add resource options (typically
		// aliases) to an account's provider and to a tunnel component.
		AccountOptions func(account string) []pulumi.ResourceOption
		TunnelOptions  func(Tunnel) []pulumi.ResourceOption

		Logger *slog.Logger
	}
)

// Deploy registers the stack and exports "tunnelIds", the tunnel ids by
// name.
func Deploy(ctx *pulumi.Context, args Args) error {
	if args.Sink == nil {
		return fmt.Errorf("services: a TokenSink is required: where a tunnel token lives is the caller's decision")
	}

	cfg := args.Config
	if cfg == nil {
		return fmt.Errorf("services: Config is required")
	}

	accounts := cfg.AccountsOf(args.Company)
	if len(accounts) == 0 {
		return fmt.Errorf("services: company %q has no account in the registry", args.Company)
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

		acct, err := account.New(ctx, name, account.Args{AccountID: cfg.Accounts[name].AccountID}, pulumi.String(token), opts...)
		if err != nil {
			return fmt.Errorf("account %s: %w", name, err)
		}

		providers[name] = acct
	}

	zoneIDs := map[string]string{}
	zoneAccounts := map[string]string{}

	for name := range cfg.Zones {
		z := cfg.Zones[name]
		if z.SecurityTxtOnly() {
			continue
		}

		if _, ok := providers[z.Account]; ok {
			zoneIDs[name] = z.ZoneID
			zoneAccounts[name] = z.Account
		}
	}

	domains := make(map[string]string, len(args.Clusters))
	for _, c := range args.Clusters {
		domains[c.Name] = c.DNSDomain
	}

	origin := args.Origin
	if origin.Service == "" && origin.Namespace == "" {
		origin.Service, origin.Namespace = cfg.Fleets.Internal.Service, cfg.Fleets.Internal.Namespace
	}

	tunnels := map[string]Tunnel{}
	edgeArgs := edge.Args{
		Origin:     edge.Origin{Service: origin.Service, Namespace: origin.Namespace, Port: origin.Port},
		CAPoolPath: args.CAPoolPath,
		Logger:     logger,
		Sink: func(ctx *pulumi.Context, t edge.Tunnel, token pulumi.StringOutput) error {
			return args.Sink(ctx, tunnels[t.Name], token)
		},
		TunnelOptions: func(t edge.Tunnel) []pulumi.ResourceOption {
			opts := []pulumi.ResourceOption{
				pulumi.Transformations([]pulumi.ResourceTransformation{originCNAMEs(t.Name, tunnelOrigins(cfg, t))}),
			}

			if args.TunnelOptions != nil {
				opts = append(opts, args.TunnelOptions(tunnels[t.Name])...)
			}

			return opts
		},
	}

	tunnelIDs := make(pulumi.StringMap)
	count := 0

	for i := range cfg.Tunnels {
		t := cfg.Tunnels[i]

		acct, ok := providers[t.Account]
		if !ok {
			continue
		}

		et := edge.Tunnel{
			Name: t.Name(), Cluster: t.Cluster, Account: t.Account, Zone: t.Zone, Zones: t.Zones,
			Hosts: t.Hosts, DNSElsewhere: t.DNSElsewhere, DNSDomain: domains[t.Cluster],
		}
		tunnels[et.Name] = t

		if err := edge.DeployTunnel(ctx, edgeArgs, acct, cfg.Accounts[t.Account].AccountID, zoneIDs, zoneAccounts, et, tunnelIDs); err != nil {
			return fmt.Errorf("deploy tunnel %s: %w", et.Name, err)
		}

		count++
	}

	ctx.Export("tunnelIds", tunnelIDs)

	logger.InfoContext(ctx.Context(), "cloudflare services stack deployed",
		slog.String("company", args.Company),
		slog.Int("accounts", len(accounts)),
		slog.Int("tunnels", count),
	)

	return nil
}

// originCNAMEs is the access-edge switch (zones.<zone>.origin_cnames): for
// each host with a non-empty origin it rewrites the content of the tunnel
// library's proxied CNAME, so the SAME record is updated in place from
// <tunnel-id>.cfargotunnel.com to the origin hostname. It edits the record's
// inputs where they stand and returns no result, so it composes with any
// other transformation of the caller, whatever their order.
func originCNAMEs(tunnelName string, origins map[string]string) pulumi.ResourceTransformation {
	byRecord := make(map[string]string, len(origins))
	for host, origin := range origins {
		byRecord["cname-"+tunnelName+"-"+tunnel.Slug(host)] = origin
	}

	return func(ta *pulumi.ResourceTransformationArgs) *pulumi.ResourceTransformationResult {
		origin, ok := byRecord[ta.Name]
		if !ok || ta.Type != dnsRecordType {
			return nil
		}

		args, ok := ta.Props.(*cf.DnsRecordArgs)
		if !ok {
			return nil
		}

		args.Content = pulumi.String(origin)

		return &pulumi.ResourceTransformationResult{Props: args, Opts: ta.Opts}
	}
}

// tunnelOrigins is the active origin_cnames of the tunnel's hosts, each read
// from the zone the host lies in.
func tunnelOrigins(cfg *config.Config, t edge.Tunnel) map[string]string {
	out := map[string]string{}

	for _, host := range t.Hosts {
		if origin, ok := cfg.Zones[cfg.ZoneOf(host)].ActiveOrigins()[host]; ok {
			out[host] = origin
		}
	}

	return out
}
