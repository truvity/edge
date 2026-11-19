// Package edge is the edge stack factory: per Cloudflare account a
// provider, per zone its settings (cloudflare/zone), and per cluster and account
// one remotely managed tunnel (cloudflare/tunnel) with its ingress rules and DNS
// records, from one plain Args value.
//
// The library's contract holds here too. Mechanism only: nothing names an
// account, a zone, a host or a cluster, Args carries them. Credentials come
// in (each account's token is resolved by the caller, because reading it is
// a plain call at program time that configures a provider, not an Output)
// and secrets go out: each tunnel's token is handed to the caller's
// [TokenSink], which decides where it lives.
//
// An estate adopting this over resources it already has passes the
// *Options hooks, which add pulumi.Aliases (or any other resource option)
// to the accounts, zones and tunnels, so that the first preview is empty.
package edge

import (
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"

	"github.com/pulumi/pulumi-random/sdk/v4/go/random"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/account"
	"github.com/truvity/edge/v4/cloudflare/edgespec"
	"github.com/truvity/edge/v4/cloudflare/tunnel"
	"github.com/truvity/edge/v4/cloudflare/zone"
)

// Tunnel is one tunnel: the hosts one cluster serves through one account's
// zones. It is edgespec.Tunnel, so the type is shared with the Pulumi-free
// Derive.
type Tunnel = edgespec.Tunnel

const (
	// DefaultCAPoolPath is where the cloudflared chart mounts the origin CA
	// root (charts/cloudflared). The remote tunnel config names it, so the
	// mount must exist before a path that uses it ships.
	DefaultCAPoolPath = "/etc/cloudflared/certs/ca.pem"
	// DefaultOriginPort is the HTTPS port of an origin service: what
	// Origin.Port means when it is zero.
	DefaultOriginPort = 443
)

type (
	// Account is one Cloudflare account and the already-resolved token its
	// provider authenticates with.
	Account struct {
		Name      string
		AccountID string
		Token     string
	}

	// Zone is one zone's settings, in the library's own shape. The account
	// that owns it is named by Account.
	Zone struct {
		// Name is the zone's own name; its slug names the Pulumi resources.
		Name    string
		Account string
		Args    zone.Args
	}

	// Origin is the in-cluster service every ingress rule routes to.
	Origin struct {
		Service   string
		Namespace string
		// Port is the HTTPS port of Service that every ingress rule dials.
		// Zero is DefaultOriginPort. Set it when the tunnel's hosts are
		// served by a listener on another port of the same Service; the
		// rule's hostname stays the origin SNI.
		Port int
	}

	// TokenSink stores one tunnel's token where its cluster reads it. The
	// token is the library's secret Output.
	TokenSink func(ctx *pulumi.Context, t Tunnel, token pulumi.StringOutput) error

	// Args is the whole edge stack.
	Args struct {
		Accounts []Account
		Zones    []Zone
		Tunnels  []Tunnel
		Origin   Origin
		// CAPoolPath defaults to DefaultCAPoolPath.
		CAPoolPath string
		// Sink receives each tunnel's token. Required.
		Sink TokenSink

		// AccountOptions, ZoneOptions and TunnelOptions add resource
		// options (typically aliases) to what is registered for each.
		AccountOptions func(Account) []pulumi.ResourceOption
		ZoneOptions    func(Zone) []pulumi.ResourceOption
		TunnelOptions  func(Tunnel) []pulumi.ResourceOption

		Logger *slog.Logger
	}
)

// Deploy registers the stack and exports "tunnelIds", the tunnel ids by
// name.
func Deploy(ctx *pulumi.Context, args Args) error {
	if args.Sink == nil {
		return fmt.Errorf("edge: a TokenSink is required: where a tunnel token lives is the caller's decision")
	}

	if err := args.Origin.check(); err != nil {
		return err
	}

	logger := args.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	accountConfigs := make(map[string]Account, len(args.Accounts))
	for _, a := range args.Accounts {
		accountConfigs[a.Name] = a
	}

	accounts := make(map[string]*account.Account, len(args.Accounts))

	for _, a := range sortedAccounts(args.Accounts) {
		acct, err := newAccount(ctx, logger, args, a)
		if err != nil {
			return fmt.Errorf("account %s: %w", a.Name, err)
		}

		accounts[a.Name] = acct
	}

	zoneIDs := make(map[string]string, len(args.Zones))
	zoneAccounts := make(map[string]string, len(args.Zones))

	zones := sortedZones(args.Zones)
	for i := range zones {
		z := zones[i]
		zoneIDs[z.Name] = z.Args.ZoneID
		zoneAccounts[z.Name] = z.Account

		acct, ok := accounts[z.Account]
		if !ok {
			return fmt.Errorf("zone %s: account %q is not declared", z.Name, z.Account)
		}

		opts := []pulumi.ResourceOption{acct.Use()}
		if args.ZoneOptions != nil {
			opts = append(opts, args.ZoneOptions(z)...)
		}

		if _, err := zone.New(ctx, tunnel.Slug(z.Name), z.Args, opts...); err != nil {
			return fmt.Errorf("zone %s: %w", z.Name, err)
		}
	}

	tunnelIDs := make(pulumi.StringMap)

	for i := range args.Tunnels {
		t := &args.Tunnels[i]

		acct, ok := accounts[t.Account]
		if !ok {
			return fmt.Errorf("tunnel %s: account %q is not declared", t.Name, t.Account)
		}

		if err := DeployTunnel(ctx, args, acct, accountConfigs[t.Account].AccountID, zoneIDs, zoneAccounts, *t, tunnelIDs); err != nil {
			return fmt.Errorf("deploy tunnel %s: %w", t.Name, err)
		}
	}

	ctx.Export("tunnelIds", tunnelIDs)

	logger.InfoContext(ctx.Context(), "cloudflare tunnel stack deployed",
		slog.Int("accounts", len(args.Accounts)),
		slog.Int("zones", len(args.Zones)),
		slog.Int("tunnels", len(args.Tunnels)),
	)

	return nil
}

// DeployTunnel registers one tunnel exactly as Deploy does: its secret, the
// tunnel component with its ingress and CNAMEs, the token handed to
// args.Sink, and its id added to tunnelIDs. acct is the provider of the
// tunnel's account; zoneIDs maps every zone the tunnel may serve names in to
// its id and zoneAccounts to the account that owns it. Only the tunnel
// fields of args are read (Origin, CAPoolPath, Sink, TunnelOptions, Logger).
// It is the half of Deploy a stack that has no zones of its own (a services
// stack, cloudflare/services) calls.
func DeployTunnel(
	ctx *pulumi.Context,
	args Args,
	acct *account.Account,
	accountID string,
	zoneIDs, zoneAccounts map[string]string,
	t Tunnel,
	tunnelIDs pulumi.StringMap,
) error {
	if args.Sink == nil {
		return fmt.Errorf("edge: a TokenSink is required: where a tunnel token lives is the caller's decision")
	}

	if err := args.Origin.check(); err != nil {
		return err
	}

	logger := args.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	if err := checkTunnelZones(t, zoneAccounts); err != nil {
		return err
	}

	return deployTunnel(ctx, logger, args, acct, Account{Name: t.Account, AccountID: accountID}, zoneIDs, t, tunnelIDs)
}

func newAccount(ctx *pulumi.Context, logger *slog.Logger, args Args, a Account) (*account.Account, error) {
	if a.Token == "" {
		return nil, fmt.Errorf("no resolved API token (the caller must resolve it before Deploy)")
	}

	var opts []pulumi.ResourceOption
	if args.AccountOptions != nil {
		opts = args.AccountOptions(a)
	}

	acct, err := account.New(ctx, a.Name, account.Args{AccountID: a.AccountID}, pulumi.String(a.Token), opts...)
	if err != nil {
		return nil, err
	}

	logger.InfoContext(ctx.Context(), "cloudflare provider created from the resolved child token",
		slog.String("account", a.Name),
	)

	return acct, nil
}

// TunnelArgs maps one tunnel whose hosts all lie in one zone onto the
// library's Args: pure translation, no resources. For a tunnel whose hosts
// lie in several zones (Tunnel.Zones), use TunnelArgsInZones. It returns no
// error, so it writes Origin.Port as given: Deploy, DeployTunnel and
// TunnelArgsInZones refuse a port that is not a TCP port.
func TunnelArgs(args Args, accountID, zoneID string, t Tunnel) tunnel.Args {
	return tunnelArgs(args, accountID, &tunnel.DNS{ZoneID: zoneID, Names: t.DNSNames()}, t)
}

// TunnelArgsInZones maps one tunnel onto the library's Args, each host's DNS
// record in its own zone: zoneIDs maps each zone name to its id. A tunnel
// in one zone (Zones empty, or one entry) maps exactly as TunnelArgs does;
// one in several gets tunnel.DNS.Zones, from which cloudflare/tunnel picks each
// host's zone by longest suffix. A zone with no id is an error.
func TunnelArgsInZones(args Args, accountID string, zoneIDs map[string]string, t Tunnel) (tunnel.Args, error) {
	if err := args.Origin.check(); err != nil {
		return tunnel.Args{}, err
	}

	zones := tunnelZones(t)

	if len(zones) == 1 {
		zoneID, ok := zoneIDs[zones[0]]
		if !ok {
			return tunnel.Args{}, fmt.Errorf("zone %q is not declared", zones[0])
		}

		return TunnelArgs(args, accountID, zoneID, t), nil
	}

	dns := &tunnel.DNS{Names: t.DNSNames()}

	for _, name := range zones {
		zoneID, ok := zoneIDs[name]
		if !ok {
			return tunnel.Args{}, fmt.Errorf("zone %q is not declared", name)
		}

		dns.Zones = append(dns.Zones, tunnel.DNSZone{ZoneID: zoneID, Name: name})
	}

	return tunnelArgs(args, accountID, dns, t), nil
}

// port is the port the ingress rules dial: Port, or DefaultOriginPort when
// it is zero.
func (o Origin) port() int {
	if o.Port == 0 {
		return DefaultOriginPort
	}

	return o.Port
}

// check refuses a Port that is not a TCP port. Zero is the default.
func (o Origin) check() error {
	if o.Port < 0 || o.Port > 65535 {
		return fmt.Errorf("edge: origin port %d is not a TCP port (1 to 65535, or 0 for the default %d)", o.Port, DefaultOriginPort)
	}

	return nil
}

// tunnelZones is every zone t's hosts lie in: Zones, or Zone alone when
// Zones is empty.
func tunnelZones(t Tunnel) []string {
	if len(t.Zones) > 0 {
		return t.Zones
	}

	return []string{t.Zone}
}

// checkTunnelZones refuses a tunnel with a zone that is not declared or
// belongs to another account, and a host that lies in none of its zones (a
// tunnel whose Zones were dropped on the way here would otherwise put every
// record in Zone).
func checkTunnelZones(t Tunnel, zoneAccounts map[string]string) error {
	zones := tunnelZones(t)

	if len(t.Zones) > 0 && t.Zone != "" && !slices.Contains(t.Zones, t.Zone) {
		return fmt.Errorf("zone %q is not among the tunnel's zones %v", t.Zone, t.Zones)
	}

	for _, name := range zones {
		owner, ok := zoneAccounts[name]
		if !ok {
			return fmt.Errorf("zone %q is not declared", name)
		}

		if owner != t.Account {
			return fmt.Errorf("zone %q belongs to account %q, not the tunnel's account %q: a tunnel serves names in its own account's zones only",
				name, owner, t.Account)
		}
	}

	for _, host := range t.Hosts {
		if edgespec.ZoneOf(zones, host) == "" {
			return fmt.Errorf("%q lies in none of the tunnel's zones (%s): list its zone in Tunnel.Zones",
				host, strings.Join(zones, ", "))
		}
	}

	return nil
}

func tunnelArgs(args Args, accountID string, dns *tunnel.DNS, t Tunnel) tunnel.Args {
	dnsDomain := t.DNSDomain
	if dnsDomain == "" {
		dnsDomain = "cluster." + t.Cluster
	}

	caPool := args.CAPoolPath
	if caPool == "" {
		caPool = DefaultCAPoolPath
	}

	origin := fmt.Sprintf("https://%s.%s.svc.%s:%d", args.Origin.Service, args.Origin.Namespace, dnsDomain, args.Origin.port())

	ingress := make([]tunnel.Ingress, 0, len(t.Hosts))
	for _, host := range t.Hosts {
		ingress = append(ingress, tunnel.Ingress{
			Hostname:         host,
			Service:          origin,
			OriginServerName: host,
			CAPool:           caPool,
		})
	}

	// Every host's record written elsewhere (Tunnel.DNSElsewhere): the
	// library takes no DNS block with no names.
	if dns != nil && len(dns.Names) == 0 {
		dns = nil
	}

	// No certificate packs: the zone's Total TLS issues a certificate for
	// every proxied hostname, including the ones deeper than Universal SSL's
	// wildcard.
	return tunnel.Args{
		AccountID: accountID,
		Name:      t.Name,
		Ingress:   ingress,
		DNS:       dns,
	}
}

func deployTunnel(
	ctx *pulumi.Context,
	logger *slog.Logger,
	args Args,
	acct *account.Account,
	acctCfg Account,
	zoneIDs map[string]string,
	t Tunnel,
	tunnelIDs pulumi.StringMap,
) error {
	tunArgs, err := TunnelArgsInZones(args, acctCfg.AccountID, zoneIDs, t)
	if err != nil {
		return err
	}

	// Random bytes held in Pulumi state, never derived from anything public.
	secret, err := random.NewRandomBytes(ctx, "tunnel-secret-"+t.Name, &random.RandomBytesArgs{
		Length: pulumi.Int(32),
	})
	if err != nil {
		return fmt.Errorf("tunnel secret for %s: %w", t.Name, err)
	}

	opts := []pulumi.ResourceOption{acct.Use()}
	if args.TunnelOptions != nil {
		opts = append(opts, args.TunnelOptions(t)...)
	}

	tun, err := tunnel.New(ctx, t.Name, tunArgs, secret.Base64, opts...)
	if err != nil {
		return err
	}

	tunnelIDs[t.Name] = tun.ID

	if err := args.Sink(ctx, t, tun.Token); err != nil {
		return err
	}

	logger.InfoContext(ctx.Context(), "cluster tunnel deployed",
		slog.String("cluster", t.Cluster),
		slog.String("account", t.Account),
		slog.String("tunnel_name", t.Name),
		slog.Int("hosts", len(t.Hosts)),
	)

	return nil
}

func sortedAccounts(in []Account) []Account {
	out := append([]Account(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}

func sortedZones(in []Zone) []Zone {
	out := append([]Zone(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}
