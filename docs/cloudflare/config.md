# The Cloudflare config model (`cloudflare/config`)

`cloudflare/config` is the typed form of an estate's Cloudflare registry: the
accounts it uses, the zones inside them and the settings each zone carries, the
fleet Service every tunnel delivers to, the R2 buckets, and the rotation knobs
of the token split. It is one YAML file, `cloudflare.yaml`, and the schema is
this package's: an estate loads the file and calls the package, it does not
redefine the types.

The package links no Pulumi and no Cloudflare SDK. Its dependencies are the
standard library, the YAML decoder, and this module's SDK-free packages
(`catalog`, `cloudflare/edgespec`, `cloudflare/zonespec`, `cloudflare/rules`,
`cloudflare/cfnames`); `TestNoSDKImports` fails if that changes. A tool that
only validates the file, or renders from it, does not pull the provider in.

```go
cat, err := catalog.Load(fsys)               // gateways.yaml
err = cat.WithProjects(projects)             // generated groups, if any
cfg, err := config.Load(fsys, cat, projects) // cloudflare.yaml + derived tunnels
```

`Load` reads `cloudflare.yaml` (`config.FileName`), validates the registry,
derives the tunnels and each cache zone's hosts from the catalog, and validates
both together. `Parse(data)` is the first half alone: it decodes and validates
what the file says and derives nothing. Decoding is strict: an unknown key at
any depth fails, so a misspelled setting cannot silently do nothing.

`projects` is the project list the catalog was generated from. A zone that
declares `cache` caches the exact hosts of the business projects among them
(a group is a business project's when every namespace it admits routes from is
a project), so `Load` refuses a nil list when any zone declares `cache`; an
empty, non-nil list means no business project.

## Schema

```yaml
accounts:
  <name>:                    # lowercase, becomes part of tunnel, release and Secret names
    account_id: <id>         # required
    legacy_names: true       # at most one account; keeps the unsuffixed names
    breakglass_item: <name>  # the password-manager item holding the account's root token; a name, not a secret
    company: <label>         # optional, lowercase label; the library does not interpret it
    root: {edge: {rotation: "1"}, ...}   # per-account rotations; not on the legacy_names account
zones:
  <domain>:                  # lowercase, no wildcard
    account: <name>
    owner: platform | <company>   # optional; who owns the zone (see below)
    zone_id: <id>            # or zone_id_pending: true, never both
    manage: full | security_txt   # default full; security_txt manages the zone's security.txt ONLY (see below)
    ssl_mode: full | strict  # the zone's default toward its origins
    min_tls_version: "1.2"
    total_tls: {enabled: true, certificate_authority: lets_encrypt}
    always_use_https: true
    hsts: {enabled, max_age, include_subdomains, nosniff}   # preload is refused; needs always_use_https
    bot_management: {ai_bots: block, managed_robots_txt: true}   # ai_bots: block | disabled | only_on_ad_pages; only these two fields
    cache: {respect_origin_browser_ttl: true}               # hosts are derived, never written
    public_cache: {host: <name>, ttl: 5m}                   # needs cache
    trusted_clients: {hosts: [...], ranges: [...], skip: [...]}   # skip default: bot and rate-limit checks only
    rate_limits: {plan: pro, rules: [{name, endpoints: [{host, path | path_prefix, methods}], characteristics, period, requests, mitigation_timeout, action}]}
    managed_waf: {enabled: true}                            # hosts are derived, never written; or the older {hosts: [exact...]} | {whole_zone: true}
    ssl_mode_rules: {<host>: full | strict}
    ssl_mode_rule_refs: {<host>: <32 hex>}
    config_rules_import_id: <32 hex>
    origin_cnames: {<host>: "" | d-<id>.execute-api.<region>.amazonaws.com}
    records: [{name, type: A | AAAA | CNAME | NS, content, proxied, ttl, comment, waf: false, waf_reason}]
    redirects: [{ref, from, to, status: 301, preserve_query_string: true, disabled}]
    redirect_rules_import_id: <32 hex>     # adopt an existing Single Redirects entry point
    security_txt: {contact: [mailto: | https:], expires: <RFC 3339>, preferred_languages}
fleets:
  internal: {service: <name>, namespace: <ns>}   # required
r2:
  <name>: {enabled, account, bucket, jurisdiction, lifecycle: {expire_after_days}, token: {permission, expires_on, rotation}}
root:                         # the legacy_names account's rotations; a child left out is "1"
  edge: {rotation: "1"}
  status: {rotation: "1"}
  r2_admin: {rotation: "1"}
  r2_parent: {rotation: "1"}
```

Every hostname a zone block names must be a lowercase exact name inside that
zone (and trusted clients, rate limit endpoints, managed WAF hosts and records
are held to the zone component's own checks, so a bad row fails at load and not
at apply). Trusted-client ranges must have no host bits and may not be the
whole address space; HSTS is never preloaded and is refused until
`always_use_https` is on.

## Zone owner

`owner` records who owns a zone: `platform`, or one company of the estate.
It is optional, and an empty owner says nothing about ownership.

```yaml
accounts:
  main: {account_id: <id>, legacy_names: true}
  acme: {account_id: <id>}            # company acme (its name; or set company: acme)
zones:
  example.net: {account: main, owner: platform, zone_id: <id>, ssl_mode: full}
  example.org: {account: acme, owner: acme, zone_id: <id>, ssl_mode: strict}
```

The library knows no company. It checks two things. First, the owner is
`platform` (`config.OwnerPlatform`, reserved) or a lowercase label of the
same shape as an account's `company` (`config.CompanyPattern`). Second, a
company's zone lives in an account of that company: `CompanyOf(account)`,
which is the account's `company`, or else its name. A platform zone may live
in any account. The estate checks that a company owner is one it
registers.

Nothing the library builds reads the owner. Tunnels, records, rulesets,
presets and stack names are the same with or without it. `ZonesOwnedBy(owner)`
lists an owner's zones, sorted. A zone with `manage: security_txt` may carry
an owner.

## A zone managed for its security.txt only

A hand-managed zone of an account can be brought into the registry so that
`edge-securitytxt` renews its security.txt, with nothing else of it managed:

```yaml
zones:
  example.org:
    account: main
    zone_id: <id>
    manage: security_txt
    security_txt: {contact: ["mailto:security@example.com"], expires: "2027-01-01T00:00:00Z"}
```

The zone must carry `account`, `zone_id` and `security_txt`, and may carry
`owner`; every other zone
field (`ssl_mode`, `records`, `cache`, `hsts`, `bot_management`, `trusted_clients`,
`zone_id_pending`, ...) is refused at load, and `manage` accepts only `full`
(the default) and `security_txt`. `Zone.SecurityTxtOnly()` is the test.

Such a zone creates nothing. `zonestack.Deploy` and `services.Deploy` skip it
(`zonestack.ZoneArgs` returns `zonestack.ErrSecurityTxtOnly`), `Contributions`,
`RecordsLibrary` and `ActiveOrigins` are empty, and the catalog derives no
host, tunnel route or record in it: `ZoneOf` does not return it, so a catalog
domain inside it is refused as in no zone. `securitytxt.Targets` includes it.
The `zone` preset grants its token Zone Settings Write on the zone and no other
group; see [presets](presets.md).

## Tunnels are derived

There is no `tunnels` key. Every domain on a `cloudflare-tunnel` exposure of
the catalog resolves to a zone by longest suffix, and so to that zone's
account; each cluster gets one `Tunnel` per account its names live in, sorted
by cluster then account, hosts in ingress order (exact names first, then
wildcards deeper-first). The exposure's `cloudflare_accounts` lists the
accounts its tunnels may use; a name in a zone of another account, or in no
zone, is refused where the exposure is still known. `Validate` holds any
tunnel set, derived or hand-built, to the same routing invariants: a hostname
declared once across all tunnels, no rule shadowed by an earlier one, every
host inside a zone of the tunnel's account.

A `Tunnel` names its objects: `Name()` (the cluster for the `legacy_names`
account, `<cluster>-<account>` otherwise), `Release()` (`cloudflared` or
`cloudflared-<account>`) and `SecretName()` (`...-tunnel-token`). `Zones` is
every zone its hosts lie in, so each host's CNAME lands in its own zone.

### dns_elsewhere

A catalog domain written as `{host: x.example.com, dns_elsewhere: true}` keeps
its ingress rule on the tunnel but is left out of the tunnel's DNS names:
`Tunnel.DNSElsewhere` lists it, `Tunnel.DNSNames()` is `Hosts` without it, and
`cloudflare/edge` creates no CNAME for it (a tunnel whose every record is
written elsewhere gets no DNS block). It is for a name whose record something
else writes, and it is refused on any arrival but `cloudflare-tunnel`. Two
consequences in this model: a `records` entry may name such a host (the zone is
then its one writer), and an `origin_cnames` key may not (there is no tunnel
record to move).

### managed_waf: derived, explicit, next to the name

`managed_waf: {enabled: true}` runs the Cloudflare Managed Ruleset on **every
proxied name, listed one by one**: no wildcard, no whole-zone rule, and no host
list in `cloudflare.yaml`. The list is derived by `Load` (as the cache hosts
are) and kept in `Zone.WAFHosts`:

- every exact host an exposure serves through a tunnel in the zone (a
  `dns_elsewhere` name included: its traffic still arrives through
  Cloudflare), plus
- every `records` entry of the zone that is `proxied`,

minus the names that opted out. So the coverage lives next to where each name
is declared: a new catalog domain or project endpoint is covered with no edit
to the zone, and a name is left out only where it is written, with a reason:

```yaml
# gateways.yaml
domains:
  - app.example.com                      # covered
  - {host: upload.example.com, waf: off, waf_reason: raw multipart bodies trip rule 1234}
  - {host: '*.preview.example.com', waf: off, waf_reason: per-install names; list them as exact domains}
```

`waf` takes only `off` (absent is on) and `waf_reason` is required with it and
refused without it; both are refused on an exposure that does not arrive by
`cloudflare-tunnel`. A record opts out with `waf: false` and `waf_reason`. A
generated project group takes the same from the project endpoint
(`catalog.Endpoint.WAFOff`, the reason) or from
`project_groups.overrides[cluster][project].names[host]` (`waf`, `waf_reason`).

**A wildcard never enters the list.** A proxied wildcard host (catalog domain
or record) in a zone with `enabled: true` must opt out with a reason, or the
config fails to load naming the host. `enabled` is exclusive with `hosts` and
`whole_zone` (one source), and `hosts` refuses a `*` (it is **breaking** for a
config that listed a wildcard: list the exact names on their domains and use
`enabled`). A zone that sets neither `enabled` nor wildcards behaves as before.

A name another stack creates in this zone (a status page, say) that neither the
catalog nor `records` declares is added by the consumer after `Load`:

```go
cfg, _ := config.Load(fsys, cat, projects)
err := cfg.AddWAFHosts("example.com", "status.example.com") // exact, inside the zone, zone has enabled: true
```

Exact lower-case names only, validated like the derived ones; a duplicate is
absorbed. The rule is one expression, `http.host in {"a" "b" ...}`, sorted, and
the build refuses one over Cloudflare's 4096-character limit
(`zonespec.MaxRuleExpression`): opt names out with a reason, or split the zone.

**Order of applies**: apply the zone stack before the services stack. A WAF
entry for a name that does not exist yet is harmless; the reverse (the name
served before the zone stack lists it) leaves a gap. Both stacks load the same
config at the same commit, so the zone stack sees every name the services stack
will create.

## DNS records and child-zone delegations

`records` is the one place a zone's addresses are written down rather than
derived from the catalog. Only the listed records are managed; a record the
zone already has (hand-made, imported, or a tunnel's CNAME) is left alone
unless a row names it, and naming a row does **not** adopt an existing
Cloudflare record: import the existing record by its Cloudflare id onto the
zone component's child name before the first apply. The child names are
`record-<zone>-<host-slug>-cname` for a CNAME and
`record-<zone>-<host-slug>-<type>-<content-slug>` for A, AAAA and NS
(`zone.RecordName`). The A/AAAA/CNAME names are unchanged.

`NS` is a child-zone delegation. Its content is one lower-case nameserver
hostname without a trailing dot; several rows with the same owner are one NS
set, one Cloudflare record per target, and every target must share the same
explicit TTL (60 to 86400). An NS delegation is DNS only and is refused with
`proxied: true` or any `waf`/`waf_reason`; it never joins the managed WAF and
needs no certificate. The owner must be a name below the zone — the apex NS
set is Cloudflare's — and must not be a wildcard. A tunnel host exactly at or
anywhere below an NS owner is refused: the child zone cut would hide that
parent-zone tunnel record. Child-zone contents stay with the child's own
provider; a delegation does not create or manage them.

```yaml
zones:
  example.com:
    records:
      - {name: team.example.com, type: NS, content: ns1.nameservers.example, ttl: 300}
      - {name: team.example.com, type: NS, content: ns2.nameservers.example, ttl: 300}
      - {name: team.example.com, type: NS, content: ns3.nameservers.example, ttl: 300}
      - {name: team.example.com, type: NS, content: ns4.nameservers.example, ttl: 300}
```

The four rows are four resources: `record-<zone>-team-example-com-ns-ns1-nameservers-example`
and so on. Reordering the rows does not rename them. A row is not an import:
to adopt an existing record, import `cloudflare:index/dnsRecord:DnsRecord` as
that zone component child (with the zone component as its parent) and the
provider id `<zone_id>/<dns_record_id>`.

## Canonical-host Single Redirects

`redirects` owns the zone's whole `http_request_dynamic_redirect` phase (the
dashboard's Single Redirects). Each entry matches one exact source host on
HTTP and HTTPS and every path, and answers with `to` plus the request's path;
`preserve_query_string` (default true) keeps the query string. `to` is an
HTTPS origin only (`https://example.com`, no path, port, query or fragment),
because the request path is appended. `status` is 301 (default), 302, 307 or
308. `ref` is stable and unique in the phase.

```yaml
zones:
  example.com:
    redirects:
      - ref: www_to_apex
        from: www.example.com
        to: https://example.com
        status: 301
        preserve_query_string: true
```

The rendered Cloudflare rule is exactly:

| Property | Value |
|---|---|
| Ruleset scope / kind | the zone / `zone` |
| Phase | `http_request_dynamic_redirect` |
| Ref | `www_to_apex` |
| Match | `http.host eq "www.example.com"` (HTTP and HTTPS, every path) |
| Action | `redirect` |
| Target URL expression | `concat("https://example.com", http.request.uri.path)` |
| Status | `301` |
| Preserve query string | `true` |

Validation refuses a source outside the zone, a wildcard, an HTTPS
destination with a path/port/query/fragment, a repeated ref or source,
an unsupported status, and a loop visible between the declared redirects (a
chain ending outside the declarations is allowed). No regex and no WAF
feature is involved.

**Token permission.** The zone stack's token needs `Dynamic URL Redirects
Write` (the dashboard's "Single Redirect > Edit"). It is not in
`account.ZonePolicies`/`EdgePolicies`: add `account.RedirectPolicies` for the
zones that manage redirects, use `account.EdgePoliciesWithRedirects`, or (for
the `zone` preset) set `presets.Row.RedirectZones`. Keep the zone in that list
until its last redirect ruleset has been deleted; the same grant authorizes
the delete. See [presets.md](presets.md#redirect-permission-lifecycle).

**Rule before proxy.** If one of the zone's `records` entries has the same
name as a redirect source and is `proxied: true`, that DNS record carries an
explicit Pulumi `DependsOn` on the redirect ruleset: the proxy update waits
for the ruleset's API apply, regardless of declaration order. A source record
that is still DNS-only is valid and stages the rule inert; enable it by making
the record proxied.

**Adoption and rollback.** A zone has one entry point ruleset per phase.
`redirect_rules_import_id` adopts the existing one by its 32-hex id (with its
rule refs) instead of creating a second; the provider id is
`zones/<zone_id>/<ruleset_id>`, and the field is removed once that apply is
done. The declared `redirects` list must be the complete intended rule list
for the phase. For rollback, first restore the source DNS to its legacy
DNS-only state while keeping the rule declared, then set `disabled: true` (or
remove the entry) in a second apply. A disabled entry keeps its ref and the
ruleset identity.

## Contributions

`Zone.Contributions()` returns the rules a zone's blocks ask for as
`[]rules.Contribution`, built with the `cloudflare/rules` builders, so a caller
hands them to the zone component (`zonespec.Args.Contributions`) and builds
none by hand:

| Block | Contribution |
|---|---|
| `public_cache` | `rules.PublicPathsCache(host, ttl).Contribution()` |
| `ssl_mode_rules` | one `rules.SSLMode` per host, sorted; the rule ref is `ssl_mode_rule_refs[host]` or `ssl-mode-<host>` (`Zone.SSLModeRef`) |
| `rate_limits` | `rules.RateLimits` |
| `trusted_clients` | `rules.TrustedClients` |
| `managed_waf` | `rules.ManagedWAF` |

Not included: the cacheable hosts (`cache` with the derived `Zone.CacheHosts`,
which the zone component takes as `zonespec.Args.Cache`) and the settings that
are not rules (`always_use_https`, `hsts`, `total_tls`, `min_tls_version`,
`records`), and `redirects`, which the zone component takes as its own block
(`zonespec.Args.Redirects`). A caller that passes these contributions must not also pass the
same blocks as `zonespec.Args.RateLimits`, `TrustedClients` or `ManagedWAF`.
The `Library` methods (`RateLimitsCfg`, `TrustedClientsCfg`, `HSTSCfg`,
`ManagedWAFCfg`, `Zone.RecordsLibrary`, `Zone.RedirectsLibrary`) convert one
block to the zone component's input.

## Per-account view and stack names

`ForAccount(account)` is the registry as one account's stacks see it: that
account alone, its zones, tunnels and R2 buckets, the fleet, and its own
rotations as `Root` (`RootFor`). It refuses while one of the account's zones is
`zone_id_pending`. Stack names follow one convention: `cloudflare-<kind>` for
the `legacy_names` account and `cloudflare-<kind>-<account>` for any other
(`StackName`, and `StackAccount` back from a name), for the kinds `StackRoot`,
`StackEdge` and `StackR2`.

`ValidateForeignHosts(map[env][]hosts)` holds every environment's foreign
hosts (legacy names the estate adopts) to a zone the registry declares.

## What stays with the estate

The library owns the schema, the validation that needs only the file and the
catalog, the derivation, and the conversion into the library's own inputs. The
estate keeps what names its own layout:

- where a tunnel token is stored: the OpenBao KV key, the SSM path, the
  residue stores of a cluster that predates the secret store;
- where a child token's root lives (the key prefix and root-token key of each
  account);
- the check that those keys are rows of the estate's secret layout, and the
  rule that an account may not be named so that its token key collides with
  the legacy account's;
- stacks of another kind than `root`, `edge` and `r2` (the prototypes stack of
  a sluis preset, for instance), and the Pulumi program that turns the model
  into resources;
- the file's contents: accounts, zones and ids are the estate's.
