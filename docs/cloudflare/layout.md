# Recommended layout

How to split an estate's Cloudflare Pulumi program into a root stack that
mints least-privilege child tokens and one stack per child, each running
on its own — the pattern `cloudflare/account`'s `NewChildToken` and
`NewChildTokenSet` make reusable. This is a layout this library
*documents*, not one it enforces: nothing here reads more than one
stack's worth of Pulumi state, and a single-stack estate can ignore this
page entirely and call `account.New` plus `cloudflare/zone`/`cloudflare/tunnel`/`cloudflare/r2`
directly, exactly as the [README](../../README.md)'s worked example does.

## Why more than one token

A Cloudflare API token that can create and manage other tokens
(`Account API Tokens Read`/`Write`) is, in effect, a master key: whoever
holds it can mint a token with any grant the account allows, including a
fresh copy of itself. Running every stack — zones, tunnels, R2 buckets —
on that one token means every stack's blast radius is the whole account,
and rotating any one stack's credential means rotating all of them at
once (they are the same credential).

The alternative this library makes practical: **one root token that only
mints**, held by exactly one stack, and every other stack runs as a child
token scoped to precisely what it does. `ChildTokenConfig.Validate`
refuses `Account API Tokens Read`/`Write` in any child's `PermissionGroups`
outright (see [safety.md](safety.md#child-tokens-never-hold-account-api-tokens)),
so this separation cannot be undone by a child token minting its own
replacement or its own siblings.

## The four stacks

```
root token (Account API Tokens Read/Write; never runs application code)
  │
  │  account.New + account.NewChildTokenSet
  ▼
┌─────────┬───────────┬────────────┬────────────────────────┐
│  edge   │  status¹  │  r2-admin  │  r2-parent-<bucket>×N  │
└─────────┴───────────┴────────────┴────────────────────────┘
     │           │            │                │
     ▼           ▼            ▼                ▼
  edge stack  status stack  r2 stack     a bucket's own
  (zones +    (a backup     (buckets     consumer(s) — an
   tunnel)     tunnel)       only)        S3-compatible
                                          credential, or a
                                          broker (see below)
```

¹ Optional: only estates that run a second, backup path for their tunnel
need a `status` child at all.

| Stack | Runs on | Mints/manages | Calls |
| --- | --- | --- | --- |
| **root** | the root token (never delivered to any other stack) | every child below, via `account.NewChildTokenSet` | `account.New`, `account.NewChildTokenSet`, the presets below |
| **edge** | the `edge` child | zone settings and the tunnel | `cloudflare/zone`, `cloudflare/tunnel`, both with `acct.Use()` bound to the `edge` child's own provider |
| **r2** | the `r2-admin` child | R2 buckets and their lifecycles (no tokens) | `cloudflare/r2` |
| **status** (optional) | the `status` child | a backup tunnel, DNS only | `cloudflare/tunnel` (and `cloudflare/zone` only as far as DNS) |

A bucket's own consumer — the thing that actually reads or writes
objects — is not a fifth Pulumi stack in this picture: it is whatever
holds the `r2-parent-<bucket>` child's secret value (a Kubernetes Secret,
an external secret store, or a broker that mints short-lived credentials
from it on request). The root stack mints that child; delivering it
onward is the estate's own installation, described below.

## What the root stack does

```go
package main

import (
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"

	"github.com/truvity/edge/v4/cloudflare/account"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		// The root token: Account API Tokens Read/Write, and nothing
		// this stack itself uses to touch a zone, a tunnel or a bucket.
		// Read it from wherever this estate keeps its most sensitive
		// secrets — never from the same store a child token's own
		// consumer reads from.
		rootToken := readRootTokenFromYourSecretStore(ctx)

		acct, err := account.New(ctx, "root", account.Args{AccountID: "example-account-id"}, rootToken)
		if err != nil {
			return err
		}

		zoneIDs := []string{"0123456789abcdef0123456789abcdef"}
		buckets := []string{"example-bucket"}

		cfgs := map[string]account.ChildTokenConfig{
			"edge": {
				Name: "cloudflare-edge",
				// Add EdgePoliciesWithRedirects(redirectZoneIDs, zoneIDs...)
				// when a zone manages Single Redirects; the baseline never
				// grants that permission.
				Policies: account.EdgePolicies(zoneIDs...),
			},
			"r2-admin": {
				Name:     "cloudflare-r2-admin",
				Policies: account.R2AdminPolicies(),
			},
		}
		for _, bucket := range buckets {
			cfgs["r2-parent-"+bucket] = account.ChildTokenConfig{
				Name:     "cloudflare-r2-parent-" + bucket,
				Policies: account.R2BucketParentPolicies("", bucket),
			}
		}

		tokens, err := account.NewChildTokenSet(ctx, "root", acct, cfgs)
		if err != nil {
			return err
		}

		// Deliver each child to wherever the consuming stack reads its
		// own token from. This package knows nothing about that store —
		// see "Delivery is the estate's job" below.
		for name, token := range tokens {
			if err := writeChildToYourSecretStore(ctx, name, token); err != nil {
				return err
			}
		}

		return nil
	})
}
```

An optional `status` child (a backup tunnel path) is one more entry in
`cfgs`, built from the same zone list plus a narrower policy — see
`EdgePolicies`'s own doc in [docs/cloudflare/reference.md](reference.md#role-presets)
for why this library ships no preset for it: how much of `edge`'s own
grant a backup token should be handed is an estate's own trade-off, not a
shape this library can name once and reuse everywhere.

## What the edge and r2 stacks do

Each downstream stack builds its OWN `*account.Account` from the child
token it was handed — never from the root token, which it never sees —
and calls this library exactly as the [README](../../README.md)'s worked
example does:

```go
// The edge stack: reads ONLY the "edge" child's secret value, never the
// root token.
edgeToken := readChildFromYourSecretStore(ctx, "edge")

acct, err := account.New(ctx, "edge", account.Args{AccountID: "example-account-id"}, edgeToken)
if err != nil {
	return err
}

if _, err := zone.New(ctx, "example-com", zone.Args{
	ZoneID: "example-com-zone-id",
	SSL:    "strict",
}, acct.Use()); err != nil {
	return err
}

// cloudflare/tunnel.New(...) with the same acct.Use(), as in the README.
```

```go
// The r2 stack: reads ONLY the "r2-admin" child's secret value.
r2AdminToken := readChildFromYourSecretStore(ctx, "r2-admin")

acct, err := account.New(ctx, "r2-admin", account.Args{AccountID: "example-account-id"}, r2AdminToken)
if err != nil {
	return err
}

if _, err := r2.New(ctx, "example-bucket", r2.Config{
	AccountID: acct.AccountID,
	Bucket:    "example-bucket", // the parent token is r2-parent-example-bucket, minted by root
}, acct.Use()); err != nil {
	return err
}
```

## Delivery is the estate's job

Neither `NewChildToken` nor `NewChildTokenSet` knows or cares where a
minted token's secret value ends up. This is deliberate — see the
[README](../../README.md)'s "Credentials come in, secrets go out" contract,
which every package in this library follows the same way. A root stack
typically writes each child into whatever the estate already uses to move
a secret between two Pulumi programs (a secret manager's own KV store, a
Pulumi StackReference to an encrypted config, or an external secrets
operator that mounts it into the cluster a downstream stack itself runs
in). Storage and delivery mechanics — OpenBAO, SOPS, cloud KMS, anything
estate-specific — are explicitly OUT of scope for this library; only the
mechanism for minting the tokens themselves is.
