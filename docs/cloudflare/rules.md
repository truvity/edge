# Rule contributions (`cloudflare/rules`)

A Cloudflare zone holds ONE entry point ruleset per phase. Two features that
each want cache rules cannot each create "the cache ruleset": the second
replaces the first. `cloudflare/rules` fixes that at the model level. Each
feature returns a `Contribution`; `Assemble` merges every contribution into
one ordered rule list per phase, which a single resource owns.

The package links the standard library and `cloudflare/zonespec` only, so a
program can assemble and test a zone's rules without a Pulumi or Cloudflare
SDK. It models rules; the caller maps a `Rule` onto its provider's ruleset
rule (`Params`, `RateLimit` and `Logging` carry the action parameters).

## Phases

| Constant | Phase |
|---|---|
| `PhaseCacheSettings` | `http_request_cache_settings` |
| `PhaseConfigSettings` | `http_config_settings` |
| `PhaseDynamicRedirect` | `http_request_dynamic_redirect` |
| `PhaseFirewallCustom` | `http_request_firewall_custom` |
| `PhaseFirewallManaged` | `http_request_firewall_managed` |
| `PhaseRateLimit` | `http_ratelimit` |

A phase nobody contributes to is absent from the result: that is a phase the
caller does not manage, and an empty one would delete what is there. To apply
an empty ruleset on purpose, contribute with no rules (`RateLimits` with no
rows does).

## Contribution, Order, Claim

```go
type Contribution struct {
    Source string // who wants these rules
    Phase  Phase
    Order  int    // ascending; ties by Source
    Rules  []Rule
    Claim  string // cache phase only
}
```

`Assemble` sorts by `(Order, Source)`, so the result never depends on the
order contributions were passed in. In the cache phase the conventional
orders are 100 (`OrderHostsCache`: the cacheable-hosts rule), 500
(`OrderBypass`: the assembler's own catch-all) and 900 (`OrderPathCaches`:
rules that cache part of a host the hosts rule does not).

`Assemble` refuses a conflicting writer: a `Ref` used twice within a phase
(`cache-bypass` is reserved), a `Claim` outside the cache phase, a claim that
is not parenthesised, an unknown phase, a rule without ref, action or
expression. The dynamic redirect phase assembles like any other: every
declared redirect is one rule with its stable ref, and a phase with no
contribution stays unmanaged.

## The bypass fold

Cloudflare blends the settings of every cache rule that matches, so the cache
phase is kept a partition: any request matches exactly one rule. A
contribution that caches something states what it serves as its `Claim`, and
`Assemble` adds the catch-all that bypasses the cache for everything no claim
covers. Its expression folds over the claims in Order:

| Claims | Bypass expression |
|---|---|
| none | `true` |
| `(a)` | `not (a)` |
| `(a)`, `(b)` | `(not (a)) and not (b)` |
| `(a)`, `(b)`, `(c)` | `((not (a)) and not (b)) and not (c)` |

For a zone caching `www.example.com` plus the public documents of
`access.example.com`, the assembled phase is `[cacheable-hosts, cache-bypass,
public-paths-access.example.com]` and the bypass is

```
(not (http.host in {"www.example.com"})) and not (http.host eq "access.example.com" and ...)
```

## Builders

| Builder | Phase | Notes |
|---|---|---|
| `CacheHosts(hosts)` | cache | Order 100; claims `(<hosts>)`; no hosts: bypass alone |
| `PublicPathsCache(host, PublicPathsArgs)` | cache | the sluis edge package's `PublicCacheRule`, moved unchanged; returns a `CacheRule`, whose `Contribution()` is Order 900 and claims its expression |
| `StaticSiteCache(host, StaticSiteArgs)` | cache | Order 900; claims the host; origin Cache-Control and strong ETag decide, a long edge TTL only under `HashedPrefix` |
| `RateLimits(plan, rows)` | ratelimit | `zonespec` plan validation |
| `TrustedClients(t)` | firewall custom | one `skip` rule; unmanaged block: no rules |
| `ManagedWAF(m)` | firewall managed | one `execute` rule on `http.host in {...}`; `Hosts` may hold wildcards (suffix tests) in the library, but `cloudflare/config` feeds it only exact, derived hosts (`managed_waf: {enabled: true}`, see [config.md](config.md)); an expression over 4096 characters is refused |
| `SSLMode(host, mode, ref)` | config settings | one `set_config` rule |
| `Redirects(r)` | dynamic redirect | one `redirect` rule per exact source host, sorted by source then ref; target `concat("<https origin>", http.request.uri.path)`, default status 301 and query preservation on |

`PublicPathsCache` caches a key set (a JWKS or similar public document) at the edge for the TTL it is given. When a signing key is removed after a compromise, purge those paths at Cloudflare as part of the removal: a removed key is otherwise still served from the edge cache until the edge TTL expires, and a verifier that fetches the set keeps trusting it for that long.

There is no builder for Authenticated Origin Pulls: it is a zone or hostname
certificate setting, not a ruleset rule. `zone.Args.AuthenticatedOriginPulls`
sets it (see below).

## Applying contributions: `zone.Args`

`cloudflare/zone` builds its rulesets from contributions. The `cache`,
`trustedClients`, `managedWaf` and `rateLimits` blocks are expressed as
contributions internally (`CacheHosts`, `TrustedClients`, `ManagedWAF`,
`RateLimits`), joined by `Args.Contributions`, and the lot goes through
`Assemble`: one ruleset per phase. With no extra contributions the registered
inputs are byte-identical to before (the `edge` and `tunnel` goldens did not
move).

| Field | Meaning |
|---|---|
| `Contributions []rules.Contribution` | further rules, merged with the blocks above (code only, not read from YAML/JSON) |
| `AuthenticatedOriginPulls *bool` | zone setting `tls_client_auth`, child `tls-client-auth-<name>`; nil leaves it alone |
| `ImportRulesets map[rules.Phase]string` | adopt the phase's existing entry point ruleset by id (`pulumi.Import("zones/<zone>/<id>")`); the phase must be one the zone manages; drop the entry once applied |

Child names per phase: cache `cache-rules-<name>`, firewall custom
`firewall-custom-<name>`, firewall managed `firewall-managed-<name>`, rate
limit `ratelimit-<name>`, config settings `ssl-mode-<name>`, dynamic redirect
`redirect-rules-<name>`. Rules a contribution adds are registered with their
`Ref` as the rule's `ref` and enabled; the blocks' own rules keep the inputs
they always had (no `ref`). The redirect block is a contribution built by the
zone component (`zonespec.Args.Redirects`), so its rules keep their refs and
an adopted ruleset preserves them. A zone that declares an SSL-mode
contribution or a redirect owns the whole phase, like the others.

The redirect action parameters (`rules.RedirectParams`) map to the provider's
`fromValue`: `targetUrl.expression`, `statusCode` and
`preserveQueryString`. The zone component adds one explicit dependency: a
proxied DNS record whose name is a redirect source waits for the redirect
ruleset (`pulumi.DependsOn`), which no declaration order can replace.

`Contribution`, `Rule` and their parts are defined in `cloudflare/rulespec`
(standard library only) so that `zonespec.Args` can carry them; `rules`
aliases them, so `rules.Contribution` is the same type.
