# The Cloudflare stack entry points

Three packages are whole Pulumi programs' worth of Cloudflare, one per kind of
stack. Each takes the loaded registry (`cloudflare/config`) and registers
resources; none reads a secret or names an estate. Credentials come in
(`Tokens`, already resolved by the caller) and secrets go out (a tunnel's token
to the caller's sink).

| Package | Stack | Registers |
| --- | --- | --- |
| `cloudflare/zonestack` | zone base, one per company | a provider per account of the company, and per zone of those accounts the zone component: settings, rulesets, records |
| `cloudflare/services` | services, one per company | a provider per account of the company, and per cluster and account one tunnel with its ingress rules and CNAMEs |
| `cloudflare/instancetunnel` | one per instance | a provider, a secret and a tunnel whose connector runs on a single host |

`cloudflare/edge.Deploy` stays: it is the one-stack form (zones and tunnels in
one program) and the compatibility facade over the same components.
`edge.DeployTunnel` is its tunnel half, which `services` calls.

## Which stack runs which

A company is `accounts.<name>.company` in `cloudflare.yaml`, else the account's
own name (`Config.CompanyOf`, `Config.AccountsOf`). A zone stack and a services
stack are per company and see the accounts of that company only; a tunnel serves
names in its own account's zones (the registry refuses anything else), so the
split along companies never cuts a tunnel from its zone.

```go
// zone stack of company "example"
err := zonestack.Deploy(ctx, zonestack.Args{
    Config: cfg, Company: "example", Tokens: tokens,
})

// services stack of the same company
err := services.Deploy(ctx, services.Args{
    Config: cfg, Company: "example", Tokens: tokens,
    Clusters: []services.Cluster{{Name: "kernel", DNSDomain: "cluster.local"}},
    Sink: func(ctx *pulumi.Context, t services.Tunnel, token pulumi.StringOutput) error { /* store it */ },
})

// a box outside any cluster
tun, err := instancetunnel.Deploy(ctx, instancetunnel.Args{
    Name: "status", AccountID: id, Token: token, Zones: zones,
    Routes: []instancetunnel.Route{{Host: "status.example.com", Service: "http://127.0.0.1:8081", Zone: "example.com"}},
})
```

### A zone stack of one owner

`zonestack.Args.Owners` narrows a zone stack to the zones whose `owner`
(`zones.<zone>.owner`) is listed: `config.OwnerPlatform`, a company label, or
both. Left empty it deploys every zone of the company's accounts, as before.

```go
// only the zones the platform owns, though the company's account holds more
err := zonestack.Deploy(ctx, zonestack.Args{
    Config: cfg, Company: "example", Tokens: tokens,
    Owners: []string{config.OwnerPlatform},
})
```

When `Owners` is set, a zone of the company's accounts that names no owner is
refused: the filter would otherwise guess which stack carries it. A zone with
`manage: security_txt` registers nothing and is exempt. An entry that is neither
`platform` nor a company label, or one listed twice, is refused.

The filter changes which zones a stack holds and nothing about what it
registers: a zone has the same resource names (the zone slug is the key) in a
stack of its owner and in an unfiltered one. Handing a zone from one stack to
another is therefore a state move (`pulumi state delete` in one stack,
`pulumi import` in the other), not a replacement. `services` has no such filter:
the tunnels and the CNAMEs of a company's zones stay in the company's services
stack whoever owns the zone.

The tunnels are `Config.Tunnels`, derived from the edge catalog by `config.Load`
(one per cluster and account, hosts in ingress order), so `services` takes no
catalog of its own: a second copy could only disagree. A name the catalog marks
`dns_elsewhere` keeps its ingress rule and gets no CNAME. A host listed in its
zone's `origin_cnames` has its CNAME's content rewritten to that origin.

`Clusters` only supplies each cluster's Kubernetes DNS domain (default
`cluster.<name>`); every tunnel of the company is deployed whether or not its
cluster is listed. `Origin` defaults to the registry's `fleets.internal`.

The hooks (`AccountOptions`, `ZoneOptions`, `TunnelOptions`, `Contributions`)
add resource options and rules. An estate adopting existing resources passes
aliases through them: the legacy provider names for the `legacy_names` account
(`cloudflare`, a former account key) belong in `AccountOptions`, the moves of a
record between a zone and a tunnel in `TunnelOptions` and `ZoneOptions`.

### Stack names

The per-company stacks are named `cloudflare-<company>-<kind>`, with the company
from `Config.CompanyOf`. The helpers in `cloudflare/config` build them, so no
estate spells the pattern by hand:

| Stack | Helper | Name for company `acme` |
|---|---|---|
| zone | `config.StackZone(company)` | `cloudflare-acme-zone` |
| services | `config.StackServices(company)` | `cloudflare-acme-services` |
| sts (token minting) | `config.StackSTS(company)` | `cloudflare-acme-sts` |
| any | `config.StackName(kind, company)` | kinds `config.StackKindZone`, `StackKindServices`, `StackKindSTS` |

These are not the per-account names of the stacks above (`cloudflare-<kind>`,
`cloudflare-<kind>-<account>`, method `(*Config).StackName(kind, account)`): the
company comes first and the kind last. The tokens of the zone and services
stacks are the `zone` and `services` preset kinds of
[presets.md](presets.md#kinds).

## The names contract

Splitting a live stack into a zone stack and a services stack with
`pulumi state move` replaces nothing only if the new stacks register what the old
one did. The test `TestSplitStacksRegisterWhatTheSingleStackRegisters`
(`cloudflare/zonestack`) runs the single stack (`edge.Deploy` plus the glue an
estate wrote around it) and both new stacks, once per company, on the
`cloudflare/config` fixture, and requires the union of the new stacks' resources
to equal the old stack's: type, logical name, parent, provider and inputs. The
names:

- Providers: `provider-<account>` in each stack (the legacy account key, and
  the name before the registry, are aliases the caller passes in
  `AccountOptions`); `provider-aop-provider-<zone slug>` (the component named
  `aop-provider-<zone slug>`) for a zone with active `origin_cnames` or
  `ssl_mode_rules`, which serves exactly `tls-client-auth-<slug>` and
  `ssl-mode-<slug>`.
- Zone component `truvity:cloudflare:Zone`, named `tunnel.Slug(zone)`, with
  children `setting-<slug>-ssl`, `-min-tls-version`, `-browser-cache-ttl`,
  `-always-use-https`, `-security-header`; `total-tls-<slug>`;
  `cache-rules-<slug>`; `firewall-custom-<slug>`; `firewall-managed-<slug>`;
  `ratelimit-<slug>`; `redirect-rules-<slug>`; `ssl-mode-<slug>`;
  `tls-client-auth-<slug>`; and records `record-<slug>-<host slug>-cname` or
  `record-<slug>-<host slug>-<type>-<content slug>` (A, AAAA and NS).
- Tunnel component `truvity:cloudflare:Tunnel`, named by the tunnel (`<cluster>`
  for the `legacy_names` account, `<cluster>-<account>` otherwise), with children
  `tunnel-<name>`, `ingress-<name>`, `cname-<name>-<host slug>` and
  `acm-<name>-<host slug>`, and `random:RandomBytes` `tunnel-secret-<name>`. The
  stack exports `tunnelIds`.
- Instance tunnel: component `<Name>`, secret `<Name>-tunnel-secret`, provider
  `provider-<Name>`, records `cname-<Name>-<host slug>`.

### The one parent change

`tls-client-auth-<slug>` and `ssl-mode-<slug>` are top-level resources of the
single stack and children of the zone component in the zone stack. Up to
v3.x the zone component aliased them to the no-parent URN
(`zonespec.Compat.TopLevelAliases`, which `zonestack` set), so the first
preview after the move updated a URN and replaced nothing. v4.0.0 removes that
alias: apply the zone stack with a v3 release first, so state holds the
component URNs. The test asserts this one difference by name.

### One-time in-place ruleset updates (owner decision D499 a)

The zone stack registers every rule the registry's blocks ask for in the form
the `cloudflare/rules` builders give it (`config.Zone.Contributions`): with its
`ref`, enabled, and the builder's description and expression. The single stack
registers some of them in an older form, so the first preview of the zone stack
after the split shows these in-place updates of rulesets, and nothing else (no
replacement, no create, no delete). On the fixture, the test lists them:

- `firewall-custom-<slug>` (trusted clients): `ref` `trusted-clients` and
  `enabled` true added.
- `firewall-managed-<slug>` (managed WAF): `ref` `managed-waf` added.
- `ratelimit-<slug>`: `ref` `ratelimit-<rule name as a slug>` (for example `ratelimit-browser-telemetry-ingest`) added to each rule.
- `ssl-mode-<slug>`: per rule, the description `<host> SSL <mode>` becomes
  `<host>: SSL mode <mode>`, the expression loses its outer parentheses, and a
  `ref` is added where `ssl_mode_rule_refs` pins none (`ssl-mode-<host>`).

The public documents' cache rule already has its `ref` (it is a builder
contribution in the single stack too) and does not appear. The test fails on any
other difference: it compares every resource's type, name, parent, provider and
inputs, and allows only these rule fields, on rulesets, with the rule count and
every other input equal.
