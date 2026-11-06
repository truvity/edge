# cloudflare/presets

The sluis Cloudflare presets, built from plain data rows. An estate states a
preset as a row (name, kind, account, zones, grants, lifetime); the kind
decides the rest: which permission groups, over which resources, in which
order. The estate keeps its rows and its registry of accounts and zones; this
package keeps the mechanism, so a second estate states rows, not code.

```go
reg := presets.Registry{
	Accounts: []presets.Account{{Name: "example", ID: accountID}},
	Zones:    []presets.Zone{{Name: "example.com", ID: zoneID, Account: "example"}},
}

rows := presets.StandardRows("example", []string{"example:deploy"}, []string{"example.com"}, nil)

table, err := presets.Build(reg, rows) // status, r2-admin, zone, services
```

The package reads no file and no environment and registers no resource. Rows
and registry are values; load them from your own YAML.

## Why a table

A preset's rights are used in three places that must agree: the disabled
prototype token sluis clones (`cloudflare/prototypes`), the sluis installation
(the preset's lifetime, store and grants), and break-glass, which mints a child
with the same policies when sluis cannot (`cloudflare/presettoken`). All three
read one `presets.Preset`.

## Kinds

| Kind | Rights, in this order |
|---|---|
| `zone` | the per-company zone stack (`cloudflare-<company>-zone`): no account-wide grant. DNS, SSL and certificates, zone settings, cache settings and config settings write on each zone (`account.ZonePolicies`; the dashboard's "Config Rules: Edit"); Dynamic URL Redirects Write only on `RedirectZones` (`account.RedirectPolicies`; the dashboard's "Single Redirect > Edit"); Zone WAF Write on `WAFZones`; Bot Management Write on `BotManagementZones`; Cloud Connector Write on `CloudConnectorZones`; DNS Write on `DNSOnlyZones`; Zone Settings Write alone on each `SecurityTxtOnly` zone (one extra policy per such zone, last: what writing its security.txt takes, `account.SecurityTxtPolicies`). DNS Write is the zone's own `records:`. No R2. The `edge` kind (this plus the account's tunnel write in one token) was removed in v4.0.0: use `zone` and `services` |
| `services` | the per-company services stack (`cloudflare-<company>-services`): tunnel write on the account; DNS write on each managed zone. The same policies as `status` (`tokensplit.StatusPolicies`), differing in name and description only; a `SecurityTxtOnly` zone is refused as for `status` |
| `status` | tunnel write on the account; DNS write on each zone |
| `dns` | DNS write on each zone |
| `insights` | read only: Account Security Center Insights Read on the account; Zone Security Center Insights Read, Bot Management Read, Analytics Read and Zone WAF Read on each zone. Nothing writable |
| `insights-admin` | Account Security Center Insights Write and Read on the account; Zone Security Center Insights Write and Read on each zone. The one kind that can dismiss insights (`edge-insights dismiss`); Read sits beside Write because dismissing starts from a listing and Write is not assumed to imply Read. Nothing else writable |
| `r2-admin` | **DEPRECATED, kept until a narrower replacement exists.** Workers R2 Storage Write on the account (every bucket) |
| `bucket-rw` | R2 object read and write on one bucket (`Bucket`, `Jurisdiction`); an R2 credential |
| `bucket-write` | R2 object write on one bucket; an R2 credential |

### Moving from `edge` to `zone` and `services`

The split replaces one `edge` token per account with two: `zone` for the zone
stack and `services` for the services stack. For rows that do not opt into
`RedirectZones`, the two together hold exactly the `(permission group,
resource)` pairs `edge` holds for the same row inputs: no group lost, none
gained. A redirect opt-in is zone-only and has no `services` counterpart; an
`edge`-shaped token gets it with `account.EdgePoliciesWithRedirects`. Only two
things differ, and both are the point of the split: `zone` has no tunnel write
and `services` has it, and `services` repeats DNS Write on each managed zone
(already inside `edge`'s zone policy). A `services` row cannot list a
`SecurityTxtOnly` zone, so for an account whose only zones are such, there is
no `services` row and the tunnel write has no counterpart. `edge` and
`r2-admin` stay for one release, then go.

A zone-scoped kind with no `Zones` covers every zone of its account, by zone
name, so a further zone picks the grant up with no row change. With `Zones`
the order is yours. `WAFZones`, `BotManagementZones` and `RedirectZones` must
be among the zones of the row; `DNSOnlyZones` must not be (that would grant the
same twice). `RedirectZones` is the explicit redirect capability: a zone not
listed there keeps the narrower baseline, even when another zone of the same
preset manages redirects.

### Redirect permission lifecycle

`RedirectZones` is a retained allowlist, not a value derived from the zone's
`redirects` block, so the token keeps the permission through every stage of
the rule's life:

1. **Enable:** add the zone to `RedirectZones` (and make sure it is in
   `Zones`) and apply the presets/prototypes. Apply the zone stack after the
   token exists, so the first ruleset create is authorized.
2. **Disable or delete:** keep the zone in `RedirectZones` through
   `disabled: true` staging and through deleting the last redirect ruleset.
   The same `Dynamic URL Redirects Write` grant authorizes the delete.
3. **Revoke:** only after the deletion apply has completed, remove the zone
   from `RedirectZones` and apply the presets again.

The same list is on `tokensplit.Inputs.RedirectZones` for an estate that mints
its own edge child. Removing the last rule and revoking the grant in one change
can leave the deletion without its permission.

No kind grants Account API Tokens, Billing, Account Settings, Memberships or
Access identity providers. sluis refuses a prototype that holds one, and a row
has no way to ask.

## Row

| Field | Meaning |
|---|---|
| `Name` | the sluis preset name; `<account>-<kind>` by convention (`PresetName`) |
| `Kind`, `Account` | a kind above; a registry account |
| `Description` | empty: the kind's generic sentence (`Description(account, kind)`). It is listed wherever sluis lists the preset; it is not part of the prototype token |
| `Zones`, `WAFZones`, `BotManagementZones`, `RedirectZones`, `CloudConnectorZones`, `DNSOnlyZones` | zone names of `Account` (a `SecurityTxtOnly` zone may be in `Zones` only; the `dns` and `status` kinds never cover one, the insights kinds cover it like any zone); all but `Zones` are `edge` only. `BotManagementZones` is for the zones that set `bot_management`; `RedirectZones` is for the zones that manage redirects; `StandardRows` leaves both empty, so set them on the returned row |
| `Owner` | empty: a row of the account, as before. Otherwise `platform` or a company label: the row is scoped to that owner's zones (below) |
| `Bucket`, `Jurisdiction` | bucket kinds only |
| `Grants` | the policy groups that may ask |
| `Lifetime` | zero: one hour |
| `Store`, `Rotation` | `none` (default, no rotation) or `external` (rotation positive and under the lifetime) |

`Validate(reg, rows)` reports every problem at once; `Build` calls it.

### Owner rows

A company that shares an account with the platform must not be handed a
credential that reaches the platform's zones. Set `Zone.Owner` on the registry's
zones (the zone `owner` of `cloudflare/config`) and `Row.Owner` on the company's
zone row:

```go
reg.Zones = []presets.Zone{
    {Name: "a.example", ID: idA, Account: "main", Owner: "acme"},
    {Name: "b.example", ID: idB, Account: "main", Owner: "platform"},
}
rows := []presets.Row{{
    Name: "main-acme-zone", Kind: presets.KindZone, Account: "main",
    Owner: "acme", Zones: []string{"a.example"},
}}
```

An owner row is refused unless it is the zone kind (`services`, `status`, `dns`,
the insights kinds and the bucket kinds are not scoped to a list of zones), it
lists `Zones` explicitly (empty would mean every zone of the account), every
listed zone has that owner in the registry (an unowned zone and another owner's
zone are both refused), and it sets neither `DNSOnlyZones` nor
`CloudConnectorZones`, which name zones outside `Zones`. It grants exactly what
the same row without an `Owner` grants, and `Preset.Owner` carries the owner so
the guard that decides who may ask for the preset can read it. A row without an
`Owner` is checked as before.

## Building

| Function | Result |
|---|---|
| `Build(reg, rows)` | every row's `Preset`, in row order; fails on any problem and on an account with a pending zone |
| `BuildReady(reg, rows)` | the same, leaving out the rows of an account with a pending zone (`zone_id_pending`): its prototypes would be narrower than its children, and the other accounts are unaffected |
| `ForAccount(reg, rows, account)` | one account's presets; fails only for that account's own problems and names the pending zone |
| `Named(table, name)`, `AccountOf(reg, preset)` | lookups |
| `StandardRows(account, grants, wafZones, botZones)` | the four rows `<account>-status`, `-r2-admin`, `-zone` (WAF on `wafZones`, Bot Management on `botZones`, no redirect grant) and `-services`. Set `RedirectZones` on the returned zone row. Until v4.0.0 it returned edge, status and r2-admin, and `SplitRows` added zone and services |

A `Preset` carries `Name`, `Kind`, `Account`, `AccountID`, `Description`,
`Policies` (the `account.ChildTokenPolicy` list, in its fixed order), `R2`,
`Lifetime`, `Rotation`, `Store` and `Grants`. `Preset.Documents()` renders the
policies as Cloudflare receives them (group names and resource keys), and
`Preset.Request()` is the `presettoken.Request` break-glass needs.

## The builders that use it

- **`presettoken.Estate`** is what an estate decides once for all accounts
  (sluisctl floor, binary prefixes, lifetime, grant hint, runbook, the
  break-glass 1Password account, vault and field, the environment variables).
  `Estate.Config(account, item, issuer, getenv)` is one account's `Config`;
  `Estate.Credential(ctx, src, account, preset.Request(), item, issuer, getenv)`
  is the credential of one preset. This replaces an estate's own
  `TokenConfig` and `ProviderToken`.
- **`prototypes.DeployTable(ctx, Table)`** is the prototypes stack of one
  account: it checks the estate names the account's root item, converts the
  account's presets (`prototypes.FromPresets`) and calls `prototypes.Deploy`.
  The Pulumi names (`prototype-<preset>`), the token names (`NamePrefix` plus
  the preset) and the policy order are `Deploy`'s, so an existing stack plans
  no change. `Table.Options` adds resource options per preset (an alias).

## Names, URNs and order stay

Nothing here renames a resource. The prototype of a preset keeps the Pulumi
name `prototype-<preset>`; its policies keep the order the kind gives, which is
the order of the policy list the token is registered with. A change of kind
order is a change of every prototype of that kind, so the equality test below
pins it.

## The golden test

`cloudflare/presets/testdata` holds a synthetic registry (two accounts, four
zones, one account whose zone preset has a WAF zone) and eight rows, and
`golden.json`, the table they build, dumped as the documents Cloudflare
receives. `TestGolden` builds the table and requires the bytes to match: names,
descriptions, grants, lifetimes, store, every group, scope and order. A change
to a kind is therefore a visible diff; regenerate with
`go test ./cloudflare/presets -update` and read it, because every consumer's
prototypes change with it. `prototypes` runs the same rows through
`DeployTable` under Pulumi mocks and compares the registered token names,
protection, account and policies.

A consumer that moves its own rows here proves parity in its own repository:
it asserts the table built from its rows equals the policy documents it
produced before the move. The opt-in redirect documents are pinned separately
by `TestRedirectPresetGolden`, so the default golden stays the pre-redirect
baseline.

## What stays with the estate

The registry (loaded from its YAML), the rows and which grants and WAF zones
each account has, the estate's `Estate` values, the sluis installation's
document, the break-glass wrapper that puts the root in the environment, and
the check that asks Cloudflare for each prototype's status.
