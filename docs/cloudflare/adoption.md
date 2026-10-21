# Adoption

## Prerequisites

- **Cloudflare:** a zone for each domain, and per account an API token
  scoped to that account, with permission for what the program creates
  there: tunnels, DNS records, and zone settings, Total TLS or
  certificate packs if it manages them. Total TLS and certificate packs
  need Advanced Certificate Manager on the zone's plan.
- **Go and Pulumi:** a Pulumi program in Go on the Go version this module's
  `go.mod` declares, with the `pulumi-cloudflare` v6 provider. The module
  depends on its SDK (`github.com/pulumi/pulumi-cloudflare/sdk/v6`).
- **Kubernetes:** `policy/v1` for the PodDisruptionBudget, and Helm 3 with
  OCI registry support to pull the chart.
- **A place for secrets** the estate already runs (External Secrets,
  SOPS, or a hand-made Secret): the tunnel token has to arrive in the
  cluster as a Secret, and nothing in this repository puts it there.

## Install order

1. **Accounts.** One `account.New` per account, with its token.
2. **Zone settings,** if the estate manages them: `zone.New` per zone,
   with that account's `Use()`. Turn Total TLS on before relying on it
   for new hostnames.
3. **The tunnel,** one per account: `tunnel.New` with its ingress rules,
   its DNS records and, if needed, its certificate packs. Store
   `Tunnel.Token` where the estate keeps secrets.
4. **The token Secret** in the cluster, under the name the chart will read
   (`secretName`, key `secretKey`).
5. **The origin CA Secret,** if origins are verified: a Secret holding
   only the CA certificate, key `ca.pem`.
6. **`charts/cloudflared`,** one release per account, each with its own
   `secretName` (and `caSecretName`). The connectors come up and the
   tunnel reports healthy.
7. **`caPool` on the ingress rules,** and `noTLSVerify` off, only once the
   pods from step 6 carry the CA mount (see
   [safety.md](safety.md#the-ca-mount-comes-before-the-capool)).

DNS records created in step 3 point at the tunnel before any connector
runs; until step 6, Cloudflare answers those hostnames with an error
page. Create the records in a later run if that window matters.

## The zero-diff gate

**A consumer adopts a release only when the render (or the preview) it
produces is byte-identical to what runs, or differs exactly by the change
the release announces** in [CHANGELOG.md](../../CHANGELOG.md).

- **The chart:** render your values at the pinned version and at the new
  one (`helm template <release> oci://ghcr.io/truvity/charts/cloudflared
  --version <v> --namespace <ns> -f values.yaml`) and compare.
- **The Go packages:** run `pulumi preview` on the new version. It shows
  no change, or exactly the change the release announces. A replacement
  you cannot explain from the CHANGELOG is a reason to stop.

Moving from hand-made objects to the chart or the packages is one change
whose render or preview is empty (next section). Tightening something
afterwards (`noTLSVerify` to `caPool`, a TLS floor, Total TLS) is a
separate change, adopted on its own evidence.

## Adopting what already exists

### A cloudflared already running

Name the release so the chart renders the names already live: a release
named `cloudflared` renders objects named `cloudflared`, or set
`fullnameOverride`. The pod selector must also match what runs,
`app.kubernetes.io/name`, `app.kubernetes.io/instance: <release>` and
`app.kubernetes.io/component: tunnel`. If it does not, the switch is the
delete-and-recreate described under
[the chart selector](#the-chart-selector-v1x-to-v2), not an in-place
update.

### Tunnels, records and settings Pulumi already manages

Every child name is a contract (see
[reference.md](reference.md#child-names-are-a-contract)), so resources
the estate created at the top level of its own program move under the
components by alias rather than by delete and create:

- `cloudflare/account`: `opts` go to the provider itself, so an existing provider
  is aliased onto `provider-<name>` with `pulumi.Aliases` in
  `account.New`'s options.
- `cloudflare/zone` and `cloudflare/tunnel`: children inherit the component's options,
  transformations included. Pass a transformation that adds an alias to
  each child, from its old top-level name to `tunnel-<name>`,
  `ingress-<name>`, `cname-<name>-<slug>`, `acm-<name>-<slug>`,
  `setting-<name>-ssl`, `setting-<name>-min-tls-version` or
  `total-tls-<name>`.

The gate is the preview: nothing is created, deleted or replaced.

## Upgrading

### v1.0.0 to v1.1.0: the values schema

The chart gained `values.schema.json`. An unknown key in your values now
fails the render. Render your values once before adopting and remove or
correct any key the chart does not have.

### The chart selector, v1.x to v2

**Breaking.** In v1.x every object was named `cloudflared` and the pod
selector was:

```yaml
app.kubernetes.io/name: cloudflared
app.kubernetes.io/component: tunnel
```

From v2.0.0 the Deployment, PodDisruptionBudget and ServiceAccount are
named after the release (see
[reference.md](reference.md#names-and-labels)), and the selector gains
the release:

```yaml
app.kubernetes.io/name: cloudflared
app.kubernetes.io/instance: <release>
app.kubernetes.io/component: tunnel
```

`serviceAccount.name` now defaults to empty, meaning "named after the
release"; v1.x defaulted it to `cloudflared`. Every object also gains the
`app.kubernetes.io/instance`, `app.kubernetes.io/version` and
`app.kubernetes.io/managed-by` labels.

A Deployment's selector is immutable, so `helm upgrade` cannot change it
in place: the API server rejects the patch, and a GitOps controller's
sync fails the same way. The Deployment has to be deleted and created
again. With the release named `cloudflared`, every object keeps its name
and only the selector and labels change; that is the diff the gate
expects.

The PodDisruptionBudget does not help here, in either direction. It
governs evictions, not deletes, so it neither blocks deleting the
Deployment nor keeps pods running when the Deployment is deleted with
them. Its own selector is mutable and is updated in place.

**Without an outage (recommended).** Orphan the old pods, let the new
Deployment start its own beside them, then remove the old ones:

```sh
ns=cloudflare-system

# 1. The gate: the only differences are the selector and the labels.
helm template cloudflared oci://ghcr.io/truvity/charts/cloudflared \
  --version <v1.x> --namespace $ns -f values.yaml > old.yaml
helm template cloudflared oci://ghcr.io/truvity/charts/cloudflared \
  --version <v2.x> --namespace $ns -f values.yaml > new.yaml
diff old.yaml new.yaml

# 2. Delete the Deployment but keep its ReplicaSet and pods: the tunnel
#    keeps its connectors, and the ReplicaSet still replaces a pod that dies.
kubectl -n $ns delete deployment cloudflared --cascade=orphan

# 3. Upgrade. The new Deployment's selector requires the instance label,
#    which the old ReplicaSet lacks, so it starts its own pods beside them.
helm upgrade cloudflared oci://ghcr.io/truvity/charts/cloudflared \
  --version <v2.x> --namespace $ns -f values.yaml --wait

# 4. Once the new pods are ready, remove the orphaned ReplicaSet.
kubectl -n $ns delete replicaset \
  -l 'app.kubernetes.io/name=cloudflared,app.kubernetes.io/component=tunnel,!app.kubernetes.io/instance'
```

Between steps 3 and 4 the tunnel has twice its usual connectors, which is
harmless: every replica is a connector of the same tunnel. With a GitOps
controller, step 3 is merging the version bump and letting it sync; do
step 2 when the sync reports the selector error, and the next sync
creates the Deployment.

**With a brief outage.** Delete the Deployment with its pods and upgrade:

```sh
kubectl -n $ns delete deployment cloudflared
helm upgrade cloudflared oci://ghcr.io/truvity/charts/cloudflared \
  --version <v2.x> --namespace $ns -f values.yaml --wait
```

Until the new pods connect, the tunnel has no connector and every
hostname on it fails.

A release named anything other than `cloudflared` renames every object
on this upgrade (a release `tunnel` renders `tunnel-cloudflared`). Helm
then creates the new Deployment and deletes the old one in the same
upgrade, before any `--wait`: no selector error, but no guarantee that
the new pods are ready before the old ones go. To keep the v1.x names,
set `fullnameOverride: cloudflared` (the ServiceAccount follows it) and
use the procedure above, with your release name in the `helm` commands.

### Three replicas and a budget by default (the release after v4.1.0)

`replicaCount` defaults to `3` (was `2`) and `podDisruptionBudget` is on
by default with `maxUnavailable: 1` (was off, `minAvailable: 1`). An
install that sets neither gains one replica and a PodDisruptionBudget on
the next sync; the tunnel gains one connector, which is harmless. To keep
the old shape, set `replicaCount: 2` and `podDisruptionBudget.enabled:
false`. A values file with `podDisruptionBudget.minAvailable` keeps that
budget: `minAvailable` is rendered instead of `maxUnavailable`.

`zoneSpread: DoNotSchedule` (new, off by default) spreads the replicas one
per zone. Leave it off on a cluster whose nodes carry no
`topology.kubernetes.io/zone` label: `DoNotSchedule` refuses such nodes.

### v4: the module moves to edge/v4, and three deprecated things go

v4.0.0 is a major release: the module path is `github.com/truvity/edge/v4`
(rewrite `github.com/truvity/edge/v3` to `.../v4` in your imports and run
`go mod tidy`), and every chart is published at 4.0.0 with no change in
what it renders. Three deprecated things are removed, so do these first, on
v3:

- **The `edge` preset kind** (`presets.KindEdge`). Use the `zone` and
  `services` kinds. `presets.StandardRows(account, grants, wafZones,
  botZones)` now returns status, r2-admin, zone and services; `SplitRows` is
  gone.
- **`r2.Config.Token`** (with `TokenConfig` and the `TokenID`, `TokenValue`,
  `S3AccessKeyID` and `S3SecretAccessKey` outputs, and the `token-<name>`
  child). Mint a bucket's parent token with `account.NewChildToken` and
  `account.R2BucketParentPolicies` (see [layout.md](layout.md#recommended-layout)).
- **`zonespec.Compat` (`TopLevelAliases`)**, and with it the alias `zonestack`
  set on `firewall-custom-`, `tls-client-auth-` and `ssl-mode-` children.
  Apply your zone stacks on v3 first, so their state holds the component
  URNs; a preview on v4 then shows no create or delete.

### The Go module moves to edge/v3

**The repository is now `truvity/edge`, and the Go module with it.** The
module path is `github.com/truvity/edge/v3`, first released as v3.0.0 (one
version for the whole repository: the module and all six charts). The
`pkg/` directory is gone: each Cloudflare package lives under
`cloudflare/`, and the gateway catalog under `catalog/`. No API changes:
only import paths.

| Before | After |
|---|---|
| `github.com/truvity/cloudflare/v2/pkg/<name>` | `github.com/truvity/edge/v3/cloudflare/<name>` |
| `github.com/truvity/gateway/catalog` | `github.com/truvity/edge/v3/catalog` |

```sh
go get github.com/truvity/edge/v3@v3.0.0
# rewrite the imports, for example
#   github.com/truvity/cloudflare/v2/pkg/tunnel -> github.com/truvity/edge/v3/cloudflare/tunnel
go mod tidy   # drops the v2 requirement
```

The chart OCI names are unchanged (`oci://ghcr.io/truvity/charts/<chart>`);
their versions jump to 3.0.0 with the content of the last release of their
former repository. The section below is the history of the earlier move,
to /v2.

### The Go module moves to /v2

**v2.0.0 cannot be fetched as a Go module.** Its `go.mod` still declared
the v1 path, and Go refuses a major-version tag whose module path does not
end in that major version. v2.0.1 is the first release whose module path
is `github.com/truvity/cloudflare/v2`. The chart published at 2.0.0 is
fine; for Go, use v2.0.1 or later.

```sh
go get github.com/truvity/cloudflare/v2@v2.0.1
# rewrite the imports
#   github.com/truvity/cloudflare/pkg/tunnel -> github.com/truvity/cloudflare/v2/pkg/tunnel
go mod tidy   # drops the v1 requirement
```

What else v2 changes for Go code:

- **`pkg/tunnel`'s API is unchanged.** `tunnel.Args` still carries
  `AccountID`, `New` still takes the provider in its options, and the
  child names are the same. A program that only rewrites its imports
  previews no change, unless its ingress list is refused (next point).
- **`Validate` refuses a shadowed ingress rule** (see
  [safety.md](safety.md#ingress-order-is-checked)). A list that passed
  v1.2.0 can fail v2. Reorder it: exact hostnames before wildcards,
  deeper wildcards before broader ones. The refused rule was being served
  by the wrong origin, so the reorder changes routing for exactly those
  hostnames, and that change is the one to review.
- **`pkg/account` and `pkg/zone` are new.** Adopting `pkg/account` moves
  the tunnel's resources to a provider named `provider-<name>`; alias the
  provider the program used before onto that name (see
  [adopting](#tunnels-records-and-settings-pulumi-already-manages)) so the
  preview stays empty. Set `tunnel.Args.AccountID` from
  `Account.AccountID` so the two cannot disagree. `pkg/zone` manages
  nothing until it is given a zone.
