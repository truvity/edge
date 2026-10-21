# Reference

Every chart value and every Go input and output. For the chart,
`charts/cloudflared/values.yaml` carries the same keys as commented
defaults and `values.schema.json` is the authority on types: an unknown
key fails the render. For the Go packages, the doc comments are the
authority; `Validate()` on each `Args` type returns the first problem, and
[safety.md](safety.md) lists every refusal.

## charts/cloudflared

The in-cluster end of a **remotely managed** tunnel: ingress rules live in
Cloudflare (`cloudflare/tunnel` writes them), and the pod needs only the tunnel
token. It renders a Deployment, a ServiceAccount and, unless disabled, a
PodDisruptionBudget. Nothing else: no Secret, no NetworkPolicy, no
Service.

### Values

| Value | Default | Type | Notes |
| --- | --- | --- | --- |
| `nameOverride` | `""` | string | replaces the chart name in object names and in the `app.kubernetes.io/name` label |
| `fullnameOverride` | `""` | string | replaces the whole object name; two installs in one namespace must not share one |
| `replicaCount` | `3` | integer, at least `0` | each replica is one connector of the same tunnel: one per zone in a three-zone cluster. Cloudflare allows 25 active replicas per tunnel, each opening four connections to the edge |
| `image.repository` | `cloudflare/cloudflared` | string, non-empty | |
| `image.tag` | pinned in `values.yaml` | string, non-empty | Renovate bumps it; `--no-autoupdate` is always passed, so the tag is the version that runs |
| `image.pullPolicy` | `IfNotPresent` | `Always`, `IfNotPresent` or `Never` | |
| `secretName` | `cloudflared-tunnel-token` | string, non-empty | the Secret holding the tunnel token; the caller creates it |
| `secretKey` | `tunnel-token` | string, non-empty | the key in that Secret; the Secret is mounted at `/secrets` and read with `--token-file /secrets/<secretKey>`, so the token is never in the pod spec or in `ps` |
| `caSecretName` | `""` | string | a Secret with the CA that signed the origins' certificates, mounted at `/etc/cloudflared/certs`; see [caSecretName](#casecretname-and-capool). Empty: no mount |
| `extraArgs` | `[]` | list of strings | appended after `run --token-file …` |
| `serviceAccount.create` | `true` | boolean | |
| `serviceAccount.name` | `""` | string | empty: named after the release, like every other object; the pod uses this name whether or not the chart creates it |
| `serviceAccount.annotations` | `{}` | map of strings | |
| `terminationGracePeriodSeconds` | `30` | integer, at least `0` | |
| `podAnnotations` | `{}` | map of strings | |
| `podLabels` | `{}` | map of strings | on the pod only, never in the selector |
| `priorityClassName` | `""` | string | |
| `nodeSelector` | `{}` | map of strings | |
| `tolerations` | `[]` | list | passed through |
| `affinity` | `{}` | object | passed through |
| `topologySpreadConstraints` | `[]` | list | passed through, before the `zoneSpread` constraint |
| `zoneSpread` | `""` | `""`, `DoNotSchedule` or `ScheduleAnyway` | one replica per zone: `maxSkew: 1` on `topology.kubernetes.io/zone`, selected by the release's pod selector and `matchLabelKeys: [pod-template-hash]` (a rolling update spreads the new pods by themselves). Empty: no constraint of the chart's own, because `DoNotSchedule` refuses every node without a zone label |
| `podDisruptionBudget.enabled` | `true` | boolean | renders a PodDisruptionBudget with the pod selector |
| `podDisruptionBudget.maxUnavailable` | `1` | integer, string or `null` | a count or a percentage: one replica at a time, whatever `replicaCount` is |
| `podDisruptionBudget.minAvailable` | `null` | integer, string or `null` | set: rendered instead of `maxUnavailable` (a budget takes one of the two) |
| `resources` | requests `cpu: 50m`, `memory: 64Mi`; limit `memory: 256Mi` | object | passed through |
| `global` | unset | object | accepted so a parent chart's globals do not fail the schema; unused |

### What the chart fixes, not values

- **Command:** `cloudflared tunnel --no-autoupdate --metrics 0.0.0.0:2000
  run --token-file /secrets/<secretKey>`, then `extraArgs`.
- **Probes:** liveness and readiness on `GET /ready` at the `metrics` port
  (2000/TCP).
- **Security context:** non-root, user 65532, `RuntimeDefault` seccomp,
  no privilege escalation, every capability dropped.

### Names and labels

| | Rendered as |
| --- | --- |
| object name (Deployment, ServiceAccount, PodDisruptionBudget) | `fullnameOverride`; otherwise the release name if it contains the chart name (or `nameOverride`), else `<release>-<chart name>`; 63 characters at most |
| pod selector | `app.kubernetes.io/name`, `app.kubernetes.io/instance: <release>`, `app.kubernetes.io/component: tunnel` |
| labels on every object | the selector labels, `app.kubernetes.io/version` (the stamped chart version) and `app.kubernetes.io/managed-by` |

A release named `cloudflared` renders objects named `cloudflared`; a
release named `cloudflared-partner` renders `cloudflared-partner`; a
release named `tunnel` renders `tunnel-cloudflared`.

### caSecretName and caPool

`caPool` is not a chart value. It is a field of each ingress rule in the
**remote** tunnel configuration (`tunnel.Ingress.CAPool` in Go,
`originRequest.caPool` in Cloudflare's terms): a file path inside the
cloudflared container. The chart's part is to put the file there:

- `caSecretName` names a Kubernetes Secret the caller creates and owns.
- The whole Secret is mounted, read-only, at `/etc/cloudflared/certs`;
  each key becomes a file of that name.
- The documented path, `/etc/cloudflared/certs/ca.pem`, therefore assumes
  the key `ca.pem`. A Secret with a different key works when `caPool`
  names that file instead.

With `caPool` set on a rule, cloudflared verifies the origin's
certificate against that CA instead of skipping verification. The mount
has to be in the running pods before any rule points `caPool` at it; see
[safety.md](safety.md#the-ca-mount-comes-before-the-capool).

```sh
kubectl -n cloudflare-system create secret generic cloudflared-origin-ca \
  --from-file=ca.pem=./origin-ca.pem
```

## cloudflare/account

`github.com/truvity/edge/v4/cloudflare/account`

```go
func New(ctx *pulumi.Context, name string, args Args, token pulumi.StringInput, opts ...pulumi.ResourceOption) (*Account, error)
func (a *Account) Use() pulumi.ResourceOption
func (a *Account) Invoke() pulumi.InvokeOption
func InvokeOptionsFromResourceOptions(opts ...pulumi.ResourceOption) ([]pulumi.InvokeOption, error)
```

### Args

| Field | YAML key | Required | Notes |
| --- | --- | --- | --- |
| `AccountID` | `accountId` | yes | the account the token is scoped to |
| `BaseURL` | `baseUrl` | no | overrides the API endpoint; empty is Cloudflare's own. For a proxy or a test double |

`token` is an argument, not a field: a credential never travels in the
data. It is required and is marked secret before it reaches the provider,
so it never lands in plain state.

### Outputs and names

| | |
| --- | --- |
| `Account.AccountID` | the id as supplied, for resources that take it as an argument (`tunnel.Args.AccountID`) |
| `Account.Provider` | the `cloudflare.Provider` scoped to the token |
| `Use()` | `pulumi.Provider(a.Provider)`: pass it to every zone, tunnel and record in this account; a component's children inherit it |
| `Invoke()` | `Use()`'s invoke-side counterpart: pass it to a `Lookup*`/`Get*` data-source call directly. A component RESOURCE inherits its parent's provider automatically; a plain invoke does not, so it needs this (or `InvokeOptionsFromResourceOptions`, below) explicitly — see [safety.md](safety.md#an-invoke-does-not-inherit-a-provider-the-way-a-resource-does) |
| `InvokeOptionsFromResourceOptions(opts...)` | extracts the explicit provider already present in a `New`-style `opts` list (`pulumi.Provider(...)`, or `Account.Use()`) and returns it as an `[]pulumi.InvokeOption`, empty if `opts` carried none. What `cloudflare/r2` and `cloudflare/tunnel` call internally so their own `New`'s single `opts` list drives both the resources it registers and any invoke it makes on the side |
| child | one provider named `provider-<name>`; `opts` are passed to it, so an existing provider can be aliased onto that name |

An Account is not a component resource and creates nothing but the
provider.

### NewChildToken

```go
func NewChildToken(ctx *pulumi.Context, name string, acct *Account, cfg ChildTokenConfig, opts ...pulumi.ResourceOption) (*ChildToken, error)
```

Mints one account-owned Cloudflare API token
(`cloudflare.AccountToken`, `/accounts/{account_id}/tokens`) scoped to
exactly `cfg.Policies`, under `acct`'s account and provider — the
generic form of the token `cloudflare/r2` has minted for its own bucket since
v2.2.0. `name` is the Pulumi resource name the underlying
`AccountToken` is registered under; pass `opts` (`pulumi.Parent(...)`,
in particular) the way you would to any other resource in `acct`'s
account — `acct.Use()` needs no separate mention in `opts`, because
`NewChildToken` binds `acct`'s own provider explicitly on both the
token resource and the permission-group lookup invoke described below.

```go
ct, err := account.NewChildToken(ctx, "ci-token", acct, account.ChildTokenConfig{
	Name: "ci-dns-token",
	Policies: []account.ChildTokenPolicy{
		{
			PermissionGroups: []string{"DNS Write"},
			Scope:            account.ZoneScope{ZoneID: "0123456789abcdef0123456789abcdef"},
		},
	},
})
```

`ChildTokenConfig`:

| Field | Required | Notes |
| --- | --- | --- |
| `Name` | yes | the token's Cloudflare-visible name. When `Rotation` is set, `NewChildToken` embeds it as `"<Name>-<Rotation>"` |
| `Rotation` | no | changing it to any new value forces the token to be REPLACED — a fresh id and value, never an in-place rename. See [safety.md](safety.md#rotation-is-a-replace-not-an-update) |
| `ExpiresOn` | no | an RFC3339 timestamp after which Cloudflare refuses the token; must be in the future when set |
| `Policies` | yes, non-empty | see below |

`ChildTokenPolicy` (one per entry in `Policies`; Cloudflare unions every
`allow` policy on a token, so there is no ordering between them):

| Field | Required | Notes |
| --- | --- | --- |
| `PermissionGroups` | yes, non-empty | permission group names, resolved BY NAME per account at apply time (see below); neither `"Account API Tokens Read"` nor `"Account API Tokens Write"` may appear here — see [safety.md](safety.md#child-tokens-never-hold-account-api-tokens) |
| `Scope` | yes | the one resource `PermissionGroups` applies to: `WholeAccountScope{}`, `ZoneScope{ZoneID: "..."}` or `R2BucketScope{Jurisdiction: "...", Bucket: "..."}` |

The permission groups in one policy are resolved by name through the
provider's ACCOUNT-scoped `getAccountApiTokenPermissionGroupsList` data
source at apply time — the SAME lookup, and the same exact-match-or-refuse
behaviour, `cloudflare/r2` has always used for its own bucket token (see
[safety.md](safety.md#cloudflareaccount) for the full refusal table) — with
`acct`'s explicit Cloudflare provider carried into the invoke, never the
default one.

Each `Scope` resolves to Cloudflare's own resource key for a token
policy's `Resources` map
(https://developers.cloudflare.com/fundamentals/api/how-to/create-via-api/,
accessed 2026-09-28):

| Scope | Resource key |
| --- | --- |
| `WholeAccountScope{}` | `com.cloudflare.api.account.<ACCOUNT_ID>` — every zone and account-level resource `acct` owns |
| `ZoneScope{ZoneID}` | `com.cloudflare.api.account.zone.<ZONE_ID>` — exactly one zone. `ZoneID` must be Cloudflare's own 32-character lowercase hex zone id (confirmed against https://developers.cloudflare.com/api/resources/zones/, accessed 2026-09-28: the `zone_id` path parameter is `maxLength: 32`, and every example value on that page is a 32-character lowercase hex string) |
| `R2BucketScope{Jurisdiction, Bucket}` | `com.cloudflare.edge.r2.bucket.<ACCOUNT_ID>_<JURISDICTION>_<BUCKET_NAME>` — the same key `cloudflare/r2` has always used (see [its own reference section](#cloudflarer2)); `Jurisdiction` empty (or `"default"`) is an ordinary, non-jurisdictional bucket |

`ChildToken` outputs:

| | |
| --- | --- |
| `ChildToken.Token` | the underlying `*cloudflare.AccountToken`, for a caller that needs more than `ID`/`Value` (`Status`, `IssuedOn`, and so on) |
| `ChildToken.ID` | the child token's id |
| `ChildToken.Value` | the child token's secret value; a secret output |

`ChildToken` is deliberately NOT a Pulumi component resource: `NewChildToken`
registers exactly one resource, and the `AccountToken` is registered
directly under whatever `Parent` (or none) the caller's own `opts` give
it — its type and name in Pulumi's state are exactly what they would be
had the caller registered the `AccountToken` itself. This is what makes
`cloudflare/r2`'s own use of `NewChildToken` (since v2.4.0) invisible to an
existing deployment: no new parent type joins the resource's URN, so
nothing is replaced.

### NewChildTokenSet

```go
func NewChildTokenSet(ctx *pulumi.Context, name string, acct *Account, cfgs map[string]ChildTokenConfig, opts ...pulumi.ResourceOption) (map[string]*ChildToken, error)
```

Mints one child token per entry of `cfgs`, and returns them keyed by the
same name — "one root token mints N least-privilege children, one stack
per child", made reusable instead of hand-written once per estate (see
[docs/cloudflare/layout.md](layout.md#recommended-layout)).

```go
tokens, err := account.NewChildTokenSet(ctx, "root", acct, map[string]account.ChildTokenConfig{
	"edge": {
		Name:     "cloudflare-edge",
		Policies: account.EdgePolicies("0123456789abcdef0123456789abcdef"),
	},
	"r2-admin": {
		Name:     "cloudflare-r2-admin",
		Policies: account.R2AdminPolicies(),
	},
	"r2-parent-example-bucket": {
		Name:     "cloudflare-r2-parent-example-bucket",
		Policies: account.R2BucketParentPolicies("", "example-bucket"),
	},
})
```

It is a plain iteration convenience over `NewChildToken`, nothing more:
for each key `k` (in **sorted** order — `cfgs`' keys are sorted before
minting, so two runs register resources in the same order regardless of
Go's randomized map iteration), it calls
`NewChildToken(ctx, k, acct, cfgs[k], opts...)` exactly as a caller would
writing out one such call per child by hand.

**This is the naming-preservation contract.** `name` (`NewChildTokenSet`'s
own second argument) plays NO part in any minted resource's own Pulumi
name — every resource's name, type and provider in Pulumi's state come
entirely from `cfgs`' own keys and `acct`, exactly as they would from the
equivalent hand-written `NewChildToken` calls; `name` exists only to
prefix this function's own error messages. An estate migrating N
hand-written `NewChildToken` calls onto one `NewChildTokenSet` call — for
example, four calls named `"edge"`, `"status"`, `"r2-admin"` and
`"r2-parent-<bucket>"` becoming one map with those same four keys — is
therefore a **zero-diff** change in Pulumi's state: nothing is replaced,
nothing is renamed, nothing new joins any URN, as long as the map's keys
are exactly the names the original calls used.

A nil or empty `cfgs` mints nothing and returns an empty, non-nil map.
Every `ChildTokenConfig`'s own `Validate` (refusing `Account API Tokens`
groups, an empty `Policies` list, and so on — see
[`NewChildToken`](#newchildtoken) above) applies per entry, unchanged; the
first entry (in sorted order) that fails stops the whole call before
anything later in that order is registered.

`NewChildTokenSet` knows nothing about where the returned tokens are
stored — exactly like `NewChildToken`, it returns each `ChildToken`'s `ID`
and secret `Value` and stops there. Delivering them to a consumer (a
secret store, a chart, a Pulumi export) is the caller's job.

### Role presets

Four optional functions build a `[]ChildTokenPolicy` for a common child
role, using the exact live Cloudflare permission-group names documented
above — nothing broader, nothing hidden:

```go
func EdgePolicies(zoneIDs ...string) []ChildTokenPolicy
func EdgePoliciesWithWAF(wafZoneIDs []string, zoneIDs ...string) []ChildTokenPolicy
func BotManagementPolicies(zoneIDs ...string) []ChildTokenPolicy
func R2AdminPolicies() []ChildTokenPolicy
func R2BucketParentPolicies(jurisdiction, bucket string) []ChildTokenPolicy
```

| Preset | Grants |
| --- | --- |
| `EdgePolicies(zoneIDs...)` | `Cloudflare Tunnel Write` on the whole account, plus `DNS Write`, `SSL and Certificates Write`, `Zone Settings Write`, `Cache Settings Write` and `Config Settings Write` (the dashboard's "Config Rules: Edit") on each zone in `zoneIDs` — a stack that manages zones (`cloudflare/zone`) and runs a tunnel (`cloudflare/tunnel`) |
| `EdgePoliciesWithWAF(wafZoneIDs, zoneIDs...)` | Everything `EdgePolicies(zoneIDs...)` grants, plus one policy per zone in `wafZoneIDs` granting only `Zone WAF Write` there — what `cloudflare/zone`'s `Args.TrustedClients` needs. Opt-in: an empty `wafZoneIDs` returns `EdgePolicies` unchanged. `wafZoneIDs` is independent of `zoneIDs`; list a zone in both when the token also manages it |
| `BotManagementPolicies(zoneIDs...)` | one policy per zone granting only `Bot Management Write` (`PermBotManagementWrite`) there — what `cloudflare/zone`'s `Args.BotManagement` needs. Append it to `EdgePolicies` or `EdgePoliciesWithWAF`; the preset does so for `Row.BotManagementZones`. No zones returns nil |
| `R2AdminPolicies()` | `Workers R2 Storage Write` on the whole account — a stack that creates and administers R2 buckets (`cloudflare/r2`) |
| `R2BucketParentPolicies(jurisdiction, bucket)` | `Workers R2 Storage Bucket Item Write` on exactly one bucket (`R2BucketScope{Jurisdiction: jurisdiction, Bucket: bucket}`) — a bucket consumer's own object-level credential (`cloudflare/r2` mints none; see [cloudflare/r2](#cloudflarer2)) |

Each is a plain `[]ChildTokenPolicy` value: use it as `Policies` in a
`ChildTokenConfig` directly, or edit the slice it returns before passing
it on. No preset exists for a "status" or backup-tunnel-only child, since
that grant is closer to an estate-specific trade-off (how much of `edge`'s
own grant a backup token should be handed) than a shape this library can
name once and reuse everywhere; write that one policy by hand.

## cloudflare/presettoken

The client of a sluis Cloudflare preset. A stack asks for one preset's credential at preview and apply time and holds it in memory only; sluis clones the preset's disabled prototype (see [cloudflare/prototypes](#cloudflareprototypes)) into a token that expires.

```go
cred, err := presettoken.Token(ctx, presettoken.Config{
    Issuer:         "https://access.example.test",
    SluisctlMin:    "1.76.8",
    BinaryPrefixes: []string{"/nix/store"},
    BreakGlass: presettoken.BreakGlass{
        Env: "EXAMPLE_CLOUDFLARE_BREAKGLASS", Account: "example.1password.com",
        Vault: "vault", Item: "cloudflare-root", Field: "token", ChildPrefix: "example-breakglass",
    },
}, presettoken.Request{Preset: "example-edge", AccountID: accountID, Policies: policies})
```

`Credential` holds `Token` (an API-token preset) or `AccessKeyID` and `SecretAccessKey` (`Request.R2`), and `ExpiresOn`. Build the Pulumi provider from it; do not store it.

### Config

| Field | Meaning |
| --- | --- |
| `Issuer` | The sluis issuer, passed as `--issuer`. Set it: when empty, sluisctl's own configuration decides which issuer is asked. A plain https URL when set. The environment's `SLUISCTL_*` is stripped, so this is the only way an issuer reaches `sluisctl` |
| `SluisctlMin` | `MAJOR.MINOR.PATCH` floor, checked once per binary with `sluisctl version --json`. Required |
| `BinaryPrefixes` | Directories `sluisctl` must resolve under after symlinks (`/nix/store` for a nix or devbox profile). Absolute, clean, at least two components, not a general directory (see [safety](safety.md#cloudflarepresettoken)). Required |
| `Lifetime`, `MinRemaining` | What is asked for (default and maximum 1h, `MaxLifetime`) and the least remaining life accepted (default 15m, at least 1m when set) |
| `BreakGlass` | `Env`, `Account`, `Vault`, `Item`, `Field`, `ChildPrefix`; all or none. `Env` must match `^[A-Z][A-Z0-9_]*BREAKGLASS[A-Z0-9_]*$`. The zero value disables break-glass |
| `GrantHint`, `Runbook` | Text appended to the "not granted" and break-glass refusals |

### Break-glass

With `BreakGlass.Env` set to `1` the credential is not asked of sluis. The root is read with `op read --account <Account> --no-newline op://<Vault>/<Item>/<Field>`, refused if empty or holding whitespace, and used to mint an account token `<ChildPrefix>-<preset>-<UTC time>` with `Request.Policies` that expires within `Lifetime`. A banner on stderr names the child's id; the child is left to expire. For an R2 preset the secret is the SHA-256 of the token value, as Cloudflare derives it. `Source.RootToken` is the same read, for a caller that needs the root itself (the prototypes stack).

### What is refused

See [docs/cloudflare/safety.md](safety.md#cloudflarepresettoken).

`presettoken.Estate` holds what an estate decides once for all of its accounts (`SluisctlMin`, `BinaryPrefixes`, `Lifetime`, `GrantHint`, `Runbook` and `BreakGlass EstateBreakGlass{Env, ItemEnv, Account, Vault, Field, ChildPrefix, ItemHint}`). `Estate.Config(account, item, issuer, getenv)` is the `Config` of one account: the account's 1Password item (or the `ItemEnv` variable over it) completes the break-glass block; with no item, break-glass off is the zero block and break-glass on is an error naming what to set. `Estate.Credential(ctx, src, account, req, item, issuer, getenv)` is `Source.Token` for it, and refuses an API-token preset that comes back without a token. `presets.Preset.Request()` is the `Request` of a preset. See [presets.md](presets.md).

## cloudflare/prototypes

`prototypes.Deploy(ctx, Args)` declares the disabled prototype tokens the presets are cloned from.

| Field | Meaning |
| --- | --- |
| `AccountID` | The account, 32 lowercase hex characters |
| `Presets` | `[]Preset{Name, Policies}`; each policy list is validated as a child token's, so Account API Tokens are refused |
| `NamePrefix` | Starts every token's name in Cloudflare |
| `BreakGlass` | The estate's break-glass configuration; `Env` is required |
| `Moved`, `MovedKey` | Prototypes arriving from another stack's state. The gate is conditional: only while `Moved` is non-empty, nothing is declared until the stack config `<project>:stateMoved` is `true` |

Each token is the Pulumi resource `prototype-<name>`, `status: disabled`, protected. The stack outputs `sluisPrototypeIds` (name to id) and `sluisPrototypeStatus_<name>`. The estate keeps its own wrapper that reads the root from its vault into `CLOUDFLARE_API_TOKEN` for one `pulumi` command, and its own check that asks Cloudflare for each prototype's status.

`prototypes.DeployTable(ctx, Table)` is `Deploy` over the table of [cloudflare/presets](presets.md): `Table{Account, Presets, Estate, Item, Getenv, NamePrefix, Moved, MovedKey, Options}` declares the prototype of each preset of `Account`, after refusing a stack whose account root item the estate does not name. `prototypes.FromPresets(table, account)` is the conversion alone. `Args.Options` (and `Table.Options`) returns extra resource options per preset, appended after the protection: an alias, for a prototype declared under another URN.

## cloudflare/zone

`github.com/truvity/edge/v4/cloudflare/zone`

```go
func New(ctx *pulumi.Context, name string, args Args, opts ...pulumi.ResourceOption) (*Zone, error)
```

A component of type `truvity:cloudflare:Zone`. Every setting is optional,
and an unset one is a setting this estate does not manage: the zone keeps
whatever it has.

```go
z, err := zone.New(ctx, "example-com", zone.Args{
	ZoneID:        "example-com-zone-id",
	SSL:           "strict",
	MinTLSVersion: "1.2",
	TotalTLS:      &zone.TotalTLS{Enabled: true, CertificateAuthority: "google"},
	Cache: &zone.Cache{
		Hosts:                   []string{"app.example", "*.tenants.example"},
		RespectOriginBrowserTTL: true,
	},
	TrustedClients: &zone.TrustedClients{
		Zone:   "example.com",
		Hosts:  []string{"api.example.com"},
		Ranges: []string{"192.0.2.0/24"},
	},
	RateLimits: &zone.RateLimits{
		Plan: zone.PlanPro,
		Rules: []zone.RateLimitRule{{
			Name:              "Public ingest",
			Endpoints:         []zone.Endpoint{{Host: "app.example.com", Path: "/collect"}},
			Period:            10,
			Requests:          20,
			MitigationTimeout: 10,
			Action:            "block",
		}},
	},
}, acct.Use())
```

### Args

| Field | YAML key | Values | Manages |
| --- | --- | --- | --- |
| `ZoneID` | `zoneId` | required | the zone the settings apply to |
| `SSL` | `ssl` | `off`, `flexible`, `full`, `strict` | the zone setting `ssl`: how Cloudflare connects to the origin. `full` encrypts and accepts any certificate; `strict` also verifies it |
| `MinTLSVersion` | `minTlsVersion` | `1.0`, `1.1`, `1.2`, `1.3` | the zone setting `min_tls_version`: the lowest version a browser may negotiate |
| `TotalTLS` | `totalTls` | see below | Total TLS: a certificate per proxied hostname |
| `Cache` | `cache` | see below | the zone's cache rules: which hostnames may be cached at all |
| `TrustedClients` | `trustedClients` | see below | one rule that skips the security features for known client ranges on named hosts |
| `RateLimits` | `rateLimits` | see below | the zone's whole `http_ratelimit` ruleset: rate limiting rules, validated against the zone plan's limits |
| `Records` | `records` | see below | the listed DNS records, and only those: records made by hand, by an import or by a tunnel are never touched |
| `AlwaysUseHTTPS` | `alwaysUseHttps` | `true`, `false` (a `*bool`; nil leaves it alone) | the zone setting `always_use_https`: `on` redirects every plain-HTTP request to HTTPS |
| `HSTS` | `hsts` | see below | the zone setting `security_header`: the `Strict-Transport-Security` header |
| `BotManagement` | `botManagement` | see below | the zone's AI bots rule and managed `robots.txt` (`bot_management`), nothing else of it |
| `ManagedWAF` | `managedWaf` | see below | the zone's whole `http_request_firewall_managed` ruleset: the Cloudflare Managed Ruleset on the listed hosts |

`TotalTLS`:

| Field | YAML key | Notes |
| --- | --- | --- |
| `Enabled` | `enabled` | turns per-hostname issuance on or off |
| `CertificateAuthority` | `certificateAuthority` | `google`, `lets_encrypt` or `ssl_com`; empty leaves Cloudflare's default |

Total TLS issues a certificate for every proxied A, AAAA or CNAME record,
so a new hostname needs no certificate resource of its own. It needs
Advanced Certificate Manager on the zone's plan.

`Cache`:

| Field | YAML key | Notes |
| --- | --- | --- |
| `Hosts` | `hosts` | the hostnames that may be cached: an exact name (`app.example`) or one leading wildcard label (`*.app.example`). An empty list is a policy — the zone caches nothing |
| `RespectOriginBrowserTTL` | `respectOriginBrowserTtl` | sets the zone setting `browser_cache_ttl` to `0`, Cloudflare's "Respect Existing Headers". False leaves the setting alone |

Without this block a zone caches by **file extension**: a response under a
name ending in `.js` or `.css` is stored at the edge whether or not its
origin asked for that, and where the origin sent no `Cache-Control` the
zone gives the browser four hours. An application that answers unknown
paths with its own shell then has one page cached under another page's
name.

With it, the phase holds two rules whose expressions partition the zone —
the listed hosts, and everything else — so exactly one matches any request
and the outcome does not depend on how two overlapping cache rules merge.
A listed host is cached only as far as its own `Cache-Control` goes
(`edge_ttl` mode `bypass_by_default`: the origin's header decides, and a
response without one is not cached), and the browser is handed that header
unchanged (`browser_ttl` mode `respect_origin`). So how long anything
lives is decided in the application that serves the bytes and knows
whether they are content-addressed.

**A zone has one ruleset per phase.** A zone that declares `cache` owns
the whole `http_request_cache_settings` phase: cache rules added beside
these in the dashboard are replaced, not merged. If the zone already has
cache rules when this is first applied, adopt them with `pulumi import`
onto the child name below, or delete them first — Cloudflare will
otherwise refuse a second entry point ruleset for the phase.

`Records`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `Zone` | `zone` | the zone's lower-case domain (`example.com`); required when entries are set |
| `Entries` | `entries` | the records, any order |
| `Entries[].Name` | `name` | lower-case hostname, the zone itself or a name inside it; a leading `*.` makes a wildcard at any depth (`*.api.example.com`) |
| `Entries[].Type` | `type` | `A`, `AAAA` or `CNAME`: the types Cloudflare can proxy |
| `Entries[].Content` | `content` | IPv4 (`A`), IPv6 (`AAAA`), or a lower-case target hostname without a trailing dot (`CNAME`) |
| `Entries[].Proxied` | `proxied` | serve the name through Cloudflare's edge (orange cloud); false answers DNS only |
| `Entries[].TTL` | `ttl` | DNS-only TTL, 60 to 86400 seconds; 0 is automatic. A proxied record must leave it at 0 |
| `Entries[].Comment` | `comment` | shown beside the record in the dashboard |

One `cloudflare.DnsRecord` per entry, named `record-<name>-<host-slug>-cname`
for a CNAME and `record-<name>-<host-slug>-<type>-<content-slug>` for an A or
AAAA (`zone.RecordName`). A record that already exists with the same name and
type (an import, a hand edit) is not adopted: delete it, or import it onto
that name, before the first apply. A proxied name more than one label below
the zone needs `totalTls` (or an advanced certificate) for the edge to serve
it.

`TrustedClients`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `Zone` | `zone` | the zone's lower-case domain (`example.com`); required when hosts are set |
| `Hosts` | `hosts` | exact lower-case hostnames, the zone itself or a name inside it. No wildcards |
| `Ranges` | `ranges` | CIDR blocks, IPv4 or IPv6, no host bits set; shorter than /8 (IPv4) or /16 (IPv6) is refused as too broad |
| `Description` | `description` | the rule's description; empty uses `Trusted clients: skip bot and rate-limit checks` (`Trusted clients: skip selected security features` when `Skip` is set) |
| `Skip` | `skip` | what the rule skips, by name; empty is the default below, a list replaces it. Unknown or repeated names are refused |

One zone custom-firewall rule with action `skip`, logging on, and the
expression `(http.host in {...}) and (ip.src in {...})` (both sets sorted,
so authoring order never changes the plan). A matching request skips the
bot and rate-limit checks and nothing else: the phases `http_ratelimit` and
`http_request_sbfm` (rate limiting, Super Bot Fight Mode) and the legacy
products `bic`, `hot`, `rateLimit`, `securityLevel` and `uaBlock`. The
managed rules (`http_request_firewall_managed`), the legacy `waf` product,
`zoneLockdown` and the rest of the custom rules (`ruleset: current`) still
apply to a trusted client. A skip that includes them is what Cloudflare
Security Center reports as `skip_above_threshold`.

`Skip` takes any of `current`, `http_ratelimit`, `http_request_sbfm`,
`http_request_firewall_managed`, `bic`, `hot`, `rateLimit`, `securityLevel`,
`uaBlock`, `waf`, `zoneLockdown`. `zonespec.DefaultTrustedSkip()` is the
default; `zonespec.LegacyTrustedSkip()` is all of them, the behaviour before
`Skip` existed (v3.4.0 and earlier). To add back only the managed rules, list
the default plus `http_request_firewall_managed`. Which products and phases a
plan honours is Cloudflare's: on Pro the legacy `waf` and `rateLimit` products
may be inert, and the API, not this library, decides.

It exists for a machine-to-machine caller whose egress is known and
published (for example the address range a connector platform calls from):
the caller then never meets a challenge a later rule might introduce. It is
insurance for a known caller, not a way around a rule that blocks it. Both
lists empty manages no ruleset at all; one empty and the other not is
refused, because the rule would match nothing.

**Token permission.** Writing the ruleset needs `Zone WAF Write` on the
zone. `EdgePolicies` does not include it; use
`account.EdgePoliciesWithWAF([]string{zoneID}, zoneID)` (the preset's WAF
option, which appends one `Zone WAF Write` policy per listed zone) for the
token that applies the zone, for example the zone ID of `example.com`.

**A zone has one entry point ruleset per phase.** A zone that declares
`trustedClients` owns the whole `http_request_firewall_custom` phase: custom
rules added in the dashboard are replaced, not merged. This library manages
no other rule in that phase, so it composes with `cache` (a different
phase). A zone that already has custom firewall rules must adopt them with
`pulumi import` onto the child name below, or delete them first, because
Cloudflare refuses a second entry point ruleset for the phase.

`HSTS`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `Enabled` | `enabled` | send the header; false stores the other fields and sends nothing |
| `MaxAge` | `maxAge` | `max-age`, 0 to 31536000 seconds (12 months). `0` with `enabled` is the header that tells browsers to forget the policy |
| `IncludeSubdomains` | `includeSubdomains` | extend the policy to every name below each name that sends it |
| `Nosniff` | `nosniff` | also send `X-Content-Type-Options: nosniff` |
| `Preload` | `preload` | the `preload` directive. Refused unless `enabled`, `includeSubdomains`, `maxAge` of at least 31536000 and `alwaysUseHttps: true` (the preload list's requirements) |

A browser that has seen the header refuses plain HTTP to the name for
`maxAge` seconds and offers no way past a certificate error, so roll it out
with a short `maxAge` (minutes) and raise it once nothing broke. To turn it
off, keep it enabled with `maxAge: 0` for at least the old `maxAge`, then
disable it. The value is written with the Cloudflare API's own keys
(`strict_transport_security.max_age`, ...): the camelCase keys in the
provider's generated example never converge
([pulumi/pulumi-cloudflare#1579](https://github.com/pulumi/pulumi-cloudflare/issues/1579)).

`BotManagement`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `AIBots` | `aiBots` | the AI scrapers and crawlers rule: `block`, `disabled` or `only_on_ad_pages`. Empty leaves it alone |
| `ManagedRobotsTxt` | `managedRobotsTxt` | serve Cloudflare's managed `robots.txt` (prepended to the zone's own). Nil leaves it alone; `false` turns it off |

At least one field is required; the block alone makes a zone worth declaring.
One `cloudflare.BotManagement` child, `bot-management-<name>`, is registered
with `zoneId`, `aiBotsProtection` and `isRobotsTxtManaged` only. Bot Fight
Mode, Super Bot Fight Mode (`fight_mode`, `sbfm_*`), JavaScript detections
and crawler protection are never sent, so this block neither turns them on
nor resets them. Nil registers nothing.

The setting is a singleton of the zone, so deleting the resource changes
nothing in Cloudflare. To undo, set `aiBots: disabled` and
`managedRobotsTxt: false`, apply, then remove the block. The token needs
`Bot Management Write` on the zone; `EdgePolicies` does not grant it, so add
it with `account.BotManagementPolicies` (the preset's `Row.BotManagementZones`).

`ManagedWAF`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `Zone` | `zone` | the zone's lower-case domain (`example.com`); required with hosts |
| `Hosts` | `hosts` | the hosts the ruleset runs on: an exact name, or one leading wildcard label (`*.dev.example.com`: every name below `dev.example.com`, at any depth, not the name itself) |
| `WholeZone` | `wholeZone` | run it on every request to the zone. Exclusive with `hosts` |
| `Description` | `description` | the rule's description; empty uses `Cloudflare Managed Ruleset` |

One execute rule in the zone's `http_request_firewall_managed` entry point
ruleset: it runs the Cloudflare Managed Ruleset
(`efb7b8c949ac4650a09736fc376e9aee`, `zone.CloudflareManagedRulesetID`)
with its default actions on requests matching the hosts (sorted, so
authoring order never changes the plan), or on every request with
`wholeZone`. The OWASP Core Ruleset is not deployed. The host list is the
rollout: a few hosts first, then more, then `wholeZone`. An empty list is
refused rather than read as the whole zone. A `trustedClients` rule skips
this phase for its own hosts and ranges. The managed ruleset needs a paid
plan (Pro or above); nothing checks the zone's real plan.

**A zone has one entry point ruleset per phase.** A zone that declares
`managedWaf` owns the whole phase: managed rules deployed in the dashboard
are replaced. Writing the ruleset needs `Zone WAF Write` on the zone
(`account.EdgePoliciesWithWAF`). `alwaysUseHttps` and `hsts` need only
`Zone Settings Write`, which `EdgePolicies` grants.

`RateLimits`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `Plan` | `plan` | required: `free`, `pro` or `business`. Every limit below depends on it; nothing checks the zone's real plan |
| `Rules` | `rules` | the rules, in evaluation order; at most 1 (free), 2 (pro) or 5 (business) |

`RateLimitRule`:

| Field | YAML key | Meaning |
| --- | --- | --- |
| `Name` | `name` | required, unique; the rule's description |
| `Endpoints` | `endpoints` | host and path pairs the rule protects (below); a request matching ANY of them is counted |
| `Expression` | `expression` | a hand-written Cloudflare expression, instead of `endpoints`; passed through (length checked, and on plans without the method field a mention of it is refused) |
| `CountingExpression` | `countingExpression` | business and above: count something other than what the rule matches. Refused on free and pro |
| `Characteristics` | `characteristics` | the counter's key. Empty is `ip.src`. `cf.colo.id` is added for you, as the API requires it. Free and Pro: `ip.src` only. Business: `ip.src` or `cf.unique_visitor_id` |
| `Period` | `period` | counting window, seconds. Free 10; Pro 10 or 60; Business 10, 60, 120, 300, 600 |
| `Requests` | `requests` | allowed per period before the action fires, at least 1 |
| `MitigationTimeout` | `mitigationTimeout` | seconds the action keeps applying. For `block`: Free 10; Pro 10, 60, 120, 300, 600, 3600; Business up to 86400. For the challenge actions: 0 or omitted (they throttle) |
| `Action` | `action` | `block`, `managed_challenge`, `js_challenge` or `challenge`. `log` is Enterprise-only and refused |

`Endpoint`: `host` (exact lower-case hostname), exactly one of `path`
(exact, starts with `/`, no query string) and `pathPrefix`, and optionally
`methods` (upper-case; business and above, because the Pro rule expression
has no request method field, so on Pro every method, a CORS preflight
included, is counted).

Endpoints render to one expression: each is
`(http.host eq "h" and http.request.uri.path eq "/p")`, joined with `or`, in
the order written. Values are held to a character set that needs no
escaping (hostnames to letters, digits, dots and hyphens; paths to
printable ASCII without a quote, a backslash, `?` or `#`), so a pasted
quote is refused instead of escaped into a rule that matches something
nobody meant.

The limits are the plan table in
[Cloudflare's rate limiting availability](https://developers.cloudflare.com/waf/rate-limiting-rules/#availability)
and [parameters](https://developers.cloudflare.com/waf/rate-limiting-rules/parameters/).
Where the two pages list different period values for a lower plan the
intersection is accepted. The counter always includes the data center
(`cf.colo.id`): Cloudflare keeps counters per data center, so the limit is
per address per data center, not a global one.

**Sharing the rule quota.** A Pro zone holds two rules, so list every
endpoint that deserves the same limit in ONE rule's `endpoints`. They share
a single counter per address (per data center): 20 requests per 10 seconds
is 20 across all of that rule's endpoints together, not 20 each, and an
address that trips it is blocked from all of them for the mitigation
timeout. Group by caller profile, not by app: browsers posting telemetry
(many addresses, each slow) in one rule, machine-to-machine ingest (few
addresses, fast) in another.

**A zone has one entry point ruleset per phase.** A zone that declares
`rateLimits` owns the whole `http_ratelimit` phase: rules added in the
dashboard are replaced, not merged. This library manages no other rule in
that phase (`trustedClients` only SKIPS it for its own hosts and ranges).
If the zone already has rate limiting rules, adopt them with
`pulumi import` onto the child name below, or delete them first. A
declared block with no rules applies an empty ruleset, which is how the
last rule is removed. Writing the ruleset needs `Zone WAF Write` on the
zone (`account.EdgePoliciesWithWAF`).

### Outputs and names

| | |
| --- | --- |
| `Zone.ZoneID` | the zone id, as an output |
| children | `setting-<name>-ssl`, `setting-<name>-min-tls-version`, `setting-<name>-browser-cache-ttl`, `setting-<name>-always-use-https`, `setting-<name>-security-header` (all `cloudflare.ZoneSetting`), `total-tls-<name>` (`cloudflare.TotalTls`), `cache-rules-<name>`, `firewall-custom-<name>`, `firewall-managed-<name>` and `ratelimit-<name>` (`cloudflare.Ruleset`), and `record-<name>-...` (`cloudflare.DnsRecord`, `zone.RecordName`), each only when its field is set |

## cloudflare/tunnel

`github.com/truvity/edge/v4/cloudflare/tunnel`

```go
func New(ctx *pulumi.Context, name string, args Args, secret pulumi.StringInput, opts ...pulumi.ResourceOption) (*Tunnel, error)
func Slug(host string) string
```

A component of type `truvity:cloudflare:Tunnel`: a remotely managed tunnel
(`config_src: cloudflare`), its ingress configuration, and optionally its
DNS records and Advanced Certificate packs. `secret` is the tunnel secret,
32 random bytes in base64, supplied by the caller (pulumi-random's
`RandomBytes.Base64` is the usual source); it is required, never derived,
and marked secret. Pass the account's provider (`acct.Use()`) and any
transformations in `opts`; children inherit them, and the tunnel-token
lookup (a plain invoke, not a child resource) carries the same explicit
provider explicitly rather than relying on that inheritance — see
[safety.md](safety.md#an-invoke-does-not-inherit-a-provider-the-way-a-resource-does).

### Args

| Field | YAML key | Required | Notes |
| --- | --- | --- | --- |
| `AccountID` | `accountId` | yes | the account that owns the tunnel; take it from `Account.AccountID` so the two cannot disagree |
| `Name` | `name` | yes | the tunnel's name in Cloudflare |
| `Ingress` | `ingress` | at least one | ordered rules; the library appends the catch-all `http_status:404` itself |
| `DNS` | `dns` | no | nil creates no records |
| `Certificates` | `certificates` | no | nil orders no certificate packs |

`Ingress` (one rule):

| Field | YAML key | Notes |
| --- | --- | --- |
| `Hostname` | `hostname` | required; exact or wildcard. Cloudflare's `*` spans dots |
| `Service` | `service` | required; the origin, for example `https://gateway.example-system.svc:443` |
| `OriginServerName` | `originServerName` | the SNI presented to the origin; set it when the origin's certificate names something other than the rule's hostname |
| `CAPool` | `caPool` | a file path inside the cloudflared container holding the origin's CA; with the chart, `/etc/cloudflared/certs/ca.pem`. Verification on |
| `NoTLSVerify` | `noTLSVerify` | skips origin verification for this rule; prefer `CAPool` |

Order matters and is checked: exact hostnames before wildcards, deeper
wildcards before broader ones. See
[safety.md](safety.md#ingress-order-is-checked).

`DNS`:

| Field | YAML key | Notes |
| --- | --- | --- |
| `ZoneID` | `zoneId` | the zone every record is created in, taken as given; set this or `Zones` |
| `Names` | `names` | required, non-empty, each name once (case-insensitive); each becomes a proxied CNAME to `<tunnel-id>.cfargotunnel.com` with automatic TTL, verbatim (exact or wildcard) |
| `Zones` | `zones` | for a tunnel whose names lie in several zones of its account: a list of `{ZoneID, Name}` (`zoneId`, `name`, both required, each zone once); set this or `ZoneID` |

With `Zones`, each name's record is created in the zone whose `Name` is the
longest suffix of it (a wildcard's `*.` is not a label of the zone, and a
zone delegated under another wins over its parent); a name in none of them
is refused. `DNS.ZoneIDOf(host)` returns the zone id a name's record goes
in. The records keep their names (`cname-<name>-<slug>`, no zone in it),
so a single-zone tunnel moved from `ZoneID` to a one-entry `Zones`
registers exactly the same resources. Every zone must belong to the
tunnel's account: the tunnel's provider writes the records. With `Zones`,
`Certificates.Hosts` is required (packs are ordered in one zone).

```go
DNS: &tunnel.DNS{
	Names: []string{"app.example.com", "api.example.net", "*.example.com"},
	Zones: []tunnel.DNSZone{
		{ZoneID: "<example.com zone id>", Name: "example.com"},
		{ZoneID: "<example.net zone id>", Name: "example.net"},
	},
},
```

`Certificates` (opt-in Advanced Certificate packs; needs Advanced
Certificate Manager):

| Field | YAML key | Default | Notes |
| --- | --- | --- | --- |
| `ZoneID` | `zoneId` | required | the zone the packs are ordered in |
| `Zone` | `zone` | required | the zone apex; added to every pack's host list, and `*.<zone>` is skipped because Universal SSL covers it |
| `Hosts` | `hosts` | `DNS.Names` | one pack per host; required when `DNS.Zones` is set |
| `ValidityDays` | `validityDays` | `90` | |
| `CertificateAuthority` | `certificateAuthority` | `lets_encrypt` | |

Packs are validated by TXT record. A zone with Total TLS (`cloudflare/zone`)
does not need them.

### Outputs and names

| | |
| --- | --- |
| `Tunnel.ID` | the tunnel id |
| `Tunnel.CNAMETarget` | `<id>.cfargotunnel.com`, what DNS points at |
| `Tunnel.Token` | the token cloudflared runs with (`--token-file`); a secret output, stored by the caller |
| children | `tunnel-<name>`, `ingress-<name>`, `cname-<name>-<slug>` per DNS name, `acm-<name>-<slug>` per certificate host |

`Slug(host)` renders a hostname as a name fragment: `*` becomes `star`
and `.` becomes `-`, so `*.team.example.com` is `star-team-example-com`.

## cloudflare/r2

`github.com/truvity/edge/v4/cloudflare/r2`

```go
func New(ctx *pulumi.Context, name string, cfg Config, opts ...pulumi.ResourceOption) (*R2, error)
```

A component of type `truvity:cloudflare:R2`: one R2 bucket and an opt-in
expiry lifecycle. It mints no credentials. A bucket's parent token (the one
R2's temporary credentials are minted from, and whose id and SHA-256 hashed
value are the S3 credential pair) is minted separately, with
`account.NewChildToken` or `account.NewChildTokenSet` and
`account.R2BucketParentPolicies(jurisdiction, bucket)`; see
[layout.md](layout.md#recommended-layout). Until v4.0.0 `Config.Token`
minted it inline; that path is removed.

**Provisioning permissions.** The Cloudflare API token the Pulumi program
itself runs as needs `Workers R2 Storage Write` on the account being
managed, to create the bucket and its lifecycle (the account's own
`getAccountApiTokenPermissionGroupsList` name, never the dashboard's label).

```go
r, err := r2.New(ctx, "cache", r2.Config{
	AccountID: acct.AccountID,
	Bucket:    "example-cache-bucket",
	Lifecycle: &r2.Lifecycle{ExpireAfterDays: 14},
}, acct.Use())
```

### Config

| Field | YAML key | Required | Notes |
| --- | --- | --- | --- |
| `AccountID` | `accountId` | yes | the account owning the bucket; take it from `Account.AccountID` |
| `Bucket` | `bucket` | yes | the R2 bucket name; refused if it does not follow Cloudflare's own naming rules (3-63 characters, lowercase letters, digits and hyphens, no leading or trailing hyphen) |
| `Jurisdiction` | `jurisdiction` | no | `""`/`"default"`, `"eu"`, `"fedramp"` or `"us"`: the bucket's data-residency jurisdiction |
| `Lifecycle` | `lifecycle` | no | see below; nil expires nothing |

`Lifecycle`:

| Field | YAML key | Notes |
| --- | --- | --- |
| `ExpireAfterDays` | `expireAfterDays` | required when `lifecycle` is set; must be greater than zero |
| `Prefix` | `prefix` | restricts the rule to objects whose key starts with it; empty is every object |

### Outputs and names

| | |
| --- | --- |
| `R2.BucketName` | the bucket name, as created |
| `R2.S3Endpoint` | `https://<accountId>.r2.cloudflarestorage.com`, this account's R2 S3-compatible endpoint |
| children | `bucket-<name>` (`cloudflare.R2Bucket`), `lifecycle-<name>` (`cloudflare.R2BucketLifecycle`, only when `Lifecycle` is set) |

## Child names are a contract

Every child name above is documented because an estate that already has
these resources, created at the top level of its own program, adopts them
by aliasing them onto exactly these names. A release that changed one
would turn an upgrade into a delete and a create, so a change to a child
name is a major version. [adoption.md](adoption.md#adopting-what-already-exists)
has the procedure.
