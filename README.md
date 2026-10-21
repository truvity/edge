# edge

The edge of a Kubernetes estate, as reusable mechanism: one catalog of what
is exposed, and the two halves that realise it. The **Cloudflare half**
(Pulumi Go components: accounts, zones, tunnels, R2) declares what happens
at Cloudflare; the **Envoy half** (Argo-friendly Helm charts for Envoy
Gateway) declares what happens inside the cluster. They are layers of one
exposure path, kept in one repository, one module and one version.

```
catalog (Go, data) ─┬─► Cloudflare half   cloudflare/*  (Pulumi): zones, tunnels, R2, tokens
                    └─► Envoy half        charts/gateway-*  (Helm): Gateways, ListenerSets, policies
                                          charts/cloudflared      : the in-cluster end of a tunnel
```

One tag versions everything: the Go module `github.com/truvity/edge/v4` and
all the charts are released together, at the same version (the module path
follows the major: `/v4` from v4.0.0).
The repository was formed from `truvity/cloudflare` and `truvity/gateway`
(full history of both kept; see [docs/README.md](docs/README.md)).

| Artifact | What | Status |
| --- | --- | --- |
| `cloudflare/account` | One API token bound to one account, as the resource option everything in that account is created with; `NewChildToken` mints a least-privilege, account-owned token scoped to one zone, one R2 bucket or the whole account, and `NewChildTokenSet` mints a whole map of them at once, in sorted order, with zero-diff naming | shipped |
| `cloudflare/zone` | The zone settings an estate decides: origin SSL mode, minimum TLS version, Always Use HTTPS, HSTS, Total TLS, cache rules, trusted client ranges, the Cloudflare Managed Ruleset on named hosts, rate limiting rules checked against the zone plan, DNS records | shipped |
| `cloudflare/tunnel` | A remotely managed tunnel, its ordered ingress rules, proxied DNS records and opt-in Advanced Certificate packs, from one config struct | shipped |
| `cloudflare/edge` | The edge stack factory: a provider per account, zone settings per zone and one tunnel per cluster and account from one `Args`; the caller's `TokenSink` decides where each tunnel token lives, and option hooks adopt existing resources by alias | shipped |
| `cloudflare/tokensplit` | The root of a token split: mints the least-privilege children (`edge`, `status`, `r2-admin`, one `r2-parent-<bucket>` per bucket) from the one token that may mint tokens, hands them to a caller's `Writer`, and parses the root and child secrets with the refusals every reader needs | shipped |
| `cloudflare/r2` | One R2 bucket, an opt-in expiry lifecycle, and an account-owned API token scoped to exactly that bucket — plus the S3-compatible credential pair Cloudflare derives from it | shipped |
| `cloudflare/cfnames` | Pure Cloudflare naming and validity rules — the R2 jurisdiction list and the R2 bucket-name syntax check — with no SDK import of any kind, not even this module's own `cloudflare/account` or `cloudflare/r2` | shipped |
| `cloudflare/presettoken` | The client of a sluis Cloudflare preset: `Token` execs `sluisctl` (pinned binary and version, cleaned environment, no credential cache, explicit issuer), checks the answer's lifetime and verifies a new API token against Cloudflare before use; behind an estate's break-glass switch it mints the same preset's rights from the account root instead. Every estate fact is a parameter | shipped |
| `cloudflare/prototypes` | The other half of a sluis preset: one DISABLED, protected account token per preset, declared on the default provider so the root that creates them reaches only the environment, never the state | shipped |
| `cloudflare/config` | The Cloudflare registry as a typed model with no SDK import: accounts, zones and their settings, fleet, R2 buckets and rotation knobs, loaded strictly from `cloudflare.yaml`; tunnels are derived from the catalog (a `dns_elsewhere` name keeps its ingress rule and loses its CNAME); `Zone.Contributions()` turns the zone blocks into `cloudflare/rules` contributions | shipped |
| `cloudflare/cloudflared` | The one cloudflared version pin: release, image digest and linux checksums, kept equal to the chart by `hack/cloudflared-pin.sh check` | shipped |
| `cloudflare/instanceconnector` | cloudflared as a tunnel connector on a Linux instance: pinned install, systemd unit, SSM-sourced token, start | shipped |
| `cloudflare/awsapigw` | The AWS API Gateway front door: a regional TLS 1.2 custom domain with mutual TLS that accepts only Cloudflare's client certificate (Authenticated Origin Pulls), the versioned truststore, the ACM certificate (DNS-validated through the caller's callback) and, optionally, the proxied CNAME; moved from sluis, with the same child names | shipped |
| `charts/cloudflared` | `cloudflared` as a plain Deployment: token from a Secret, optional origin CA from a Secret, one install per account | shipped |
| `charts/gateway-fleet` | GatewayClass and EnvoyProxy per class; one Gateway per exposure, with its health listener, its `allowedListeners` rule, its baseline ClientTrafficPolicy and its NetworkPolicy; further Services and egress policies in front of and around an exposure's proxies, TLS passthrough entries, and the controller's own NetworkPolicy (from gateway v1.17.0) | shipped |
| `charts/gateway-groups` | One ListenerSet per project group, a listener and a Certificate per domain, an optional BackendTLSPolicy, the ingress rules that let the gateway reach the group's workloads, the platform's own routes, and ReferenceGrants (from gateway v1.17.0) | shipped |
| `charts/gateway-policies` | The policies that protect what the other two expose: a TLS floor on every Gateway of a namespace unless it opts out, stricter TLS per listener, an OIDC or JWT SecurityPolicy per protected route with an `authenticated` or `groups` posture and CSRF, BackendTLSPolicy for private-chain backends, and BackendTrafficPolicy rows (timeouts, body limits, local rate limits; from v1.17.0) | shipped |
| `charts/gateway-projects` | `gateway-groups` and `gateway-policies` in one release, for every project on a cluster: `groups` and `policies` take those charts' values, so one Application is one chart | shipped |
| `charts/static-site-infra` | The cloud identity of a static-site install on EKS: the ACK IAM Policy and Role, the Pod Identity association, then the ServiceAccount, installed (and healthy) before `static-site`, which checks in a hook that a pod admitted now has the identity ([docs/static-site.md](docs/static-site.md#ecr-on-eks-with-ack-two-charts)) | shipped |
| `cmd/static-site`, `charts/static-site` | A generic static-site server and its chart: pulls one OCI artifact (a built site) by digest, serves it with CDN-friendly caching and allowlisted serve-time placeholders; one image and one chart for every site ([docs/static-site.md](docs/static-site.md)) | shipped |
| `charts/gateway-routes` | A library chart for the application's side: one HTTPRoute whose rules are named, `app` for the gated surface and `static` for the public assets, so a policy can gate one of them | shipped |
| `catalog` (Go, `github.com/truvity/edge/v4/catalog`) | The edge catalog as data: loader, validation, queries, the grants a ListenerSet admits by, and the per-project groups generated from a list of projects | shipped |

Charts publish to `oci://ghcr.io/truvity/charts/<chart>` on every tag, all
at the tag's version. The Go module is `github.com/truvity/edge/v4`; the
import paths changed from the two former repositories
([docs/cloudflare/adoption.md](docs/cloudflare/adoption.md#the-go-module-moves-to-edgev3)).

**Patterns to come** (not shipped yet; each lands as a package or chart of
this repository): a zone base, cluster and instance tunnels, AWS API
Gateway as an exposure, a static-site profile, a Snippet path split at the
Cloudflare edge, and an instance connector.

## Who it is for

A platform team that runs Kubernetes behind Cloudflare Tunnel, manages its
Cloudflare side with Pulumi in Go (the `pulumi-cloudflare` v6 provider),
and runs Envoy Gateway (1.9 or later, from upstream's `gateway-helm`) with
cert-manager and an issuer of its own inside the cluster. It wants its
entry points (public, private, client-certificate) declared as data, each
project's hostnames granted rather than self-served, and sign-in, token
checks and TLS floors stated as policy rather than left to each
application. The two halves are usable alone: a team on Cloudflare without
Envoy Gateway takes the Go packages and `cloudflared`; a team on Envoy
Gateway without Cloudflare takes the `gateway-*` charts and `catalog`.

The estate's domains may live in more than one Cloudflare account. The API
tokens, the tunnel secrets, where the tunnel token is stored, the origin
CA, the OIDC issuer and its clients, and the network policy around the
daemon are the estate's. Nothing here installs a secret manager, a
certificate issuer or the Envoy Gateway **controller**: the charts own what
it reconciles.

The same team, if it also uses R2, is who `cloudflare/r2` is for: the
bucket, its lifecycle and a bucket-scoped token. Short-lived R2
credentials for a build tool or a workload come from the estate's token
service, not from this repository: v2.5.0 to v2.18.0 of the former
`truvity/cloudflare` shipped an R2 credentials broker, removed in v2.19.0.
The Cloudflare module of [sluis](https://github.com/truvity/sluis) mints
the same kind of credential by cloning a disabled prototype token.

## The model


A Cloudflare API token is scoped to one account, and a tunnel cannot cross
accounts. So "which account" is a property of every resource, not a
global. **The zone is the unit and the account is its attribute**: a
domain belongs to a zone, a zone belongs to exactly one account, and an
account owns its zones and its tunnel.

```
account "example-platform"  (cloudflare/account: one token, one provider)
├── zone example.com        (cloudflare/zone, with acct.Use())
├── zone example.net        (cloudflare/zone, with acct.Use())
└── tunnel                  (cloudflare/tunnel, with acct.Use())
      ingress: app.example.com, app.example.net → the estate's origin
      token ──► Secret ──► cloudflared release "cloudflared-platform"

account "example-partner"
├── zone example.org
└── tunnel ──► Secret ──► cloudflared release "cloudflared-partner"
```

In Go, an estate writes the account as data: one `account.New` per
account, and `acct.Use()` passed to every zone, tunnel and record created
in it, so nothing can pick up another account's provider by accident.
Several zones in one account share that account's `Use()`, and one tunnel
serves hostnames from all of them. **The tunnel is per cluster and
account**: a cluster that serves hostnames from two accounts runs two
tunnels and two cloudflared installs, one per account, and a second
cluster serving the same account gets a tunnel of its own rather than
extra connectors on the first one's, so each cluster's routes, token and
failure stay its own. The chart carries the release name in every object
name and in the pod selector, so one cluster's installs share a
namespace.

### The Envoy half


An **exposure** is one way in: a public entry point behind a tunnel or a load
balancer, a private one on an internal address, later one that asks for a
client certificate. It is a Gateway, and the platform owns it.

A **group** is one project's claim on an exposure: the hostnames it serves and
the namespaces whose routes may attach to them. It is a ListenerSet, and the
platform owns it too — which hostnames exist and who may attach is exactly the
grant. The project owns only its HTTPRoutes, which name a ListenerSet as their
parent.

```
gateway-fleet                       gateway-groups              the project
─────────────                       ──────────────              ───────────
GatewayClass  ── parametersRef ──►  EnvoyProxy
  │
  └─ Gateway "public"  ◄── parentRef ── ListenerSet "argocd"  ◄── HTTPRoute
       health listener                    listener + Certificate    (argocd ns)
       allowedListeners                   per domain
       ClientTrafficPolicy                optional BackendTLSPolicy
       NetworkPolicy
```

**Policies** protect both halves (`gateway-policies`): a TLS floor selects
every Gateway of a namespace unless the Gateway opts out by label, and a
SecurityPolicy attaches to a project's route — or a listener, or one *rule*
of a route — by name.

**Routes** are the project's, and `gateway-routes` is the one piece of this
repository that lives in the project's chart: a library template that gives
a route's rules the names the policies target, so an application can serve
its assets to anyone while its shell and its API stay gated.

Two consequences worth stating, because both are load-bearing:

- **The exposure's listener is a health endpoint, not a route.** A Gateway
  with no listener has no proxies, so without it an exposure would be dark
  until the first group attached — and would go dark again when the last one
  left.
- **`allowedListeners` is the wall.** Its default admits only the exposure's
  own namespace, so a project cannot open a listener by creating an object in
  a namespace it controls.

## Install and a worked example

### The Cloudflare half

go get github.com/truvity/edge/v4@v4.0.0
```

A Pulumi program for the two accounts above. Every value is a
placeholder; an estate usually unmarshals the rows from its own YAML,
because every `Args` type is plain yaml-taggable data.

```go
package main

import (
	"github.com/pulumi/pulumi-cloudflare/sdk/v6/go/cloudflare"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"

	"github.com/truvity/edge/v4/cloudflare/account"
	"github.com/truvity/edge/v4/cloudflare/tunnel"
	"github.com/truvity/edge/v4/cloudflare/zone"
)

// One row per account: the account owns its zones and its tunnel.
type accountRow struct {
	Name    string
	Account account.Args
	Zones   []zoneRow
	Tunnel  tunnel.Args
}

type zoneRow struct {
	Name     string
	Settings zone.Args
	// Hostnames in this zone that point at the account's tunnel.
	Hostnames []string
}

var accounts = []accountRow{
	{
		Name:    "example-platform",
		Account: account.Args{AccountID: "example-platform-account-id"},
		Zones: []zoneRow{
			{
				Name: "example-com",
				Settings: zone.Args{
					ZoneID:        "example-com-zone-id",
					SSL:           "strict",
					MinTLSVersion: "1.2",
					TotalTLS:      &zone.TotalTLS{Enabled: true},
				},
				Hostnames: []string{"app.example.com"},
			},
			{
				Name:      "example-net",
				Settings:  zone.Args{ZoneID: "example-net-zone-id", SSL: "strict"},
				Hostnames: []string{"app.example.net"},
			},
		},
		Tunnel: tunnel.Args{
			Name: "example-platform",
			Ingress: []tunnel.Ingress{
				{Hostname: "app.example.com", Service: "https://gateway.example-system.svc:443",
					CAPool: "/etc/cloudflared/certs/ca.pem"},
				{Hostname: "app.example.net", Service: "https://gateway.example-system.svc:443",
					CAPool: "/etc/cloudflared/certs/ca.pem"},
			},
		},
	},
	{
		Name:    "example-partner",
		Account: account.Args{AccountID: "example-partner-account-id"},
		Zones: []zoneRow{
			{
				Name:      "example-org",
				Settings:  zone.Args{ZoneID: "example-org-zone-id", MinTLSVersion: "1.2"},
				Hostnames: []string{"portal.example.org"},
			},
		},
		Tunnel: tunnel.Args{
			Name: "example-partner",
			Ingress: []tunnel.Ingress{
				{Hostname: "portal.example.org", Service: "https://gateway.example-system.svc:443"},
			},
		},
	},
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")

		for _, row := range accounts {
			// The token is scoped to this account. It comes from wherever
			// the estate keeps secrets; here, a Pulumi config secret.
			acct, err := account.New(ctx, row.Name, row.Account, cfg.RequireSecret(row.Name+"-api-token"))
			if err != nil {
				return err
			}

			// A tunnel cannot cross accounts: take the id from the account.
			args := row.Tunnel
			args.AccountID = acct.AccountID

			tun, err := tunnel.New(ctx, row.Name, args, cfg.RequireSecret(row.Name+"-tunnel-secret"), acct.Use())
			if err != nil {
				return err
			}

			for _, z := range row.Zones {
				if _, err := zone.New(ctx, z.Name, z.Settings, acct.Use()); err != nil {
					return err
				}

				// One proxied CNAME per hostname, in its own zone, at the
				// account's tunnel.
				for _, host := range z.Hostnames {
					if _, err := cloudflare.NewDnsRecord(ctx, "cname-"+tunnel.Slug(host), &cloudflare.DnsRecordArgs{
						ZoneId:  pulumi.String(z.Settings.ZoneID),
						Name:    pulumi.String(host),
						Type:    pulumi.String("CNAME"),
						Content: tun.CNAMETarget,
						Proxied: pulumi.Bool(true),
						Ttl:     pulumi.Float64(1),
					}, acct.Use()); err != nil {
						return err
					}
				}
			}

			// The token is a secret output. Store it where the estate
			// keeps secrets; charts/cloudflared reads it from a Secret.
			ctx.Export(row.Name+"-tunnel-token", tun.Token)
		}

		return nil
	})
}
```

`tunnel.Args.DNS` creates the same records: `DNS.ZoneID` for a tunnel whose
hostnames all sit in one zone, or `DNS.Zones` (one `{ZoneID, Name}` per
zone, each name's record in its longest-suffix zone) for one that serves
several zones of its account. The example creates them by hand to show
the mechanism.

Then one cloudflared per account, each reading its own tunnel's token
from a Secret the estate created (External Secrets, SOPS, or
`kubectl create secret generic … --from-literal tunnel-token=…`):

```sh
helm install cloudflared-platform oci://ghcr.io/truvity/charts/cloudflared \
  --version <version> --namespace cloudflare-system --create-namespace \
  --values platform-values.yaml
```

```yaml
# platform-values.yaml
secretName: cloudflared-platform-token   # key tunnel-token (secretKey)
# The CA that signed the origin's certificate, as a Secret key ca.pem;
# the ingress rules above point caPool at the mounted file.
caSecretName: cloudflared-platform-origin-ca
# Three replicas (the default), one per zone; the PodDisruptionBudget
# (maxUnavailable: 1) is on by default.
zoneSpread: DoNotSchedule
```

The second account is a second release, `cloudflared-partner`, with its
own `secretName`. [docs/cloudflare/reference.md](docs/cloudflare/reference.md) has every value
and every Go field.

### The Envoy half


All the charts come from one tag; pin them at the same version.

```sh
helm install fleet oci://ghcr.io/truvity/charts/gateway-fleet \
  --namespace envoy-gateway-system \
  --values fleet-values.yaml
```

```yaml
classes:
  internal:
    # false: every Gateway of this class gets its own Deployment and
    # Service, so one exposure can carry a pinned address or a load
    # balancer without the others. true: they collapse onto one of each.
    mergeGateways: false

exposures:
  public:
    class: internal
    health:
      hostname: gateway-health.example.com
      certificate:
        issuerRef: { name: example-ca, kind: ClusterIssuer, group: cert-manager.io }
    proxy:
      service: { name: gateway-public }

  private:
    class: internal
    health:
      hostname: gateway-health.internal.example
      certificate:
        issuerRef: { name: example-private-ca, kind: ClusterIssuer, group: cert-manager.io }
    proxy:
      service:
        name: gateway-private
        type: LoadBalancer
        clusterIP: 10.0.0.60                 # a stable address inside the cluster
        loadBalancerClass: example.com/network-load-balancer
        loadBalancerSourceRanges: [10.0.0.0/14]
      shutdown:
        healthCheckFailureDelay: 5s          # drain before the balancer notices
```

```sh
helm install groups oci://ghcr.io/truvity/charts/gateway-groups \
  --namespace envoy-gateway-system \
  --values groups-values.yaml
```

```yaml
defaults:
  parent: { name: public }
  certificate:
    issuerRef: { name: example-ca, kind: ClusterIssuer, group: cert-manager.io }

groups:
  argocd:
    domains:
      - argocd.example.com
      # A rename in flight: the old host is a second domain of the same
      # group until the soak is over. Delete the entry and its listener,
      # Certificate and Secret go with it.
      - host: argocd.old.example.com
        listenerName: legacy
        secretName: argocd-legacy-tls
    allowedRoutes:
      namespaces:
        from: Selector
        selector:
          matchLabels:
            gateway.example.com/route-grant-argocd: "true"
```

A project then attaches an HTTPRoute to the ListenerSet `argocd` in the
exposure's namespace, from a namespace carrying the grant label.

```sh
helm install policies oci://ghcr.io/truvity/charts/gateway-policies \
  --namespace envoy-gateway-system \
  --values policies-values.yaml
```

```yaml
# On by default, with no values at all: TLS 1.2 or later and the
# forward-secret AEAD ciphers on every Gateway of the namespace, unless the
# Gateway carries the label `tls-baseline-exempt`.
tlsBaseline:
  namespaces: [envoy-gateway-system]

defaults:
  oidc:
    issuer: https://id.example.com     # nothing signs in until this is set

securityPolicies:
  argocd:
    namespace: argocd
    targetRefs: [{name: argocd}]       # the project's HTTPRoute
    oidc:
      clientID: example-argocd
      clientSecret: {name: example-argocd-client}   # key client-secret; yours to deliver
      hostname: argocd.example.com
    # CSRF is in shadow on a sign-in route by default: counted, not refused.
```

[docs/gateway/reference.md](docs/gateway/reference.md#gateway-policies) has a worked
example with every kind of policy.

The application's own chart depends on `gateway-routes` and calls one
template, which is what makes `sectionName` above meaningful:

```yaml
# the application's Chart.yaml
dependencies:
  - name: gateway-routes
    version: 3.0.0
    repository: oci://ghcr.io/truvity/charts
```

```yaml
# the application's templates/httproute.yaml
{{ include "gateway-routes.productRoute" (dict "root" $ "route" .Values.route) }}
```

```yaml
# the application's values.yaml
route:
  name: example-app
  hostnames: [app.example]
  parentRefs: [{kind: ListenerSet, name: argocd, namespace: envoy-gateway-system}]
  backend: {name: example-app, port: 8080}
  static:
    paths: [/app/assets]        # public: no policy names this rule
```

A `securityPolicies` entry with `targetRefs: [{name: example-app, sectionName: app}]`
then gates the shell and the API and leaves `/app/assets` anonymous —
which is what lets a browser that has not signed in, and an edge cache
that holds no cookie, fetch them. [docs/gateway/reference.md](docs/gateway/reference.md#gateway-routes)
has every input.

## Consumers

Who uses this repository, and through which surface:

| Consumer | Surface |
| --- | --- |
| truvity/gitops | Go `cloudflare/account`, `cloudflare/tunnel`, `cloudflare/zone`; charts `cloudflared`, `gateway-fleet`, `gateway-groups`, `gateway-policies` |
| A second, non-AWS estate | Go `cloudflare/tunnel`; charts `cloudflared`, `gateway-fleet`, `gateway-policies` |

## Neighbours

Repositories a reader must know about, and the boundary with each:

- **cloudflare ↔ gateway ↔ tailscale** are three LAYERS of one exposure
  path, not alternatives. The first two are this repository: the tunnel
  (`cloudflare/tunnel`, `charts/cloudflared`) carries public traffic to the
  estate's origin; that origin is a gateway exposure (a ClusterIP behind
  the tunnel). Tailscale routes whole CIDRs for private access; the
  private gateway exposure and tailscale's Service-CIDR route are the two
  ways to reach a private service.

## Documentation

The full index is [docs/README.md](docs/README.md).

- Cloudflare half, in [docs/cloudflare/](docs/cloudflare/): `layout.md` (the
  recommended root/edge/r2(/status) split), `adoption.md`, `safety.md`,
  `reference.md`, `doctrine.md`
- Envoy half, in [docs/gateway/](docs/gateway/): `adoption.md`, `safety.md`,
  `reference.md`, `doctrine.md`
- [CHANGELOG.md](CHANGELOG.md): what changed for a consumer, per version;
  the gateway's changelog before the fold is
  [docs/gateway/CHANGELOG-history.md](docs/gateway/CHANGELOG-history.md)

## The rule that makes this repository public


**Mechanism only.** Nothing here names an account, a zone, a hostname, a
cluster, an issuer or a secret path. Every such thing is an input with a neutral
default, and the consuming estate supplies it from its own (private)
repository. `hack/leak-canary.sh` enforces this in CI, and public history
cannot be unpublished — so the rule is mechanical, not remembered.

This repository follows the shared
[component contract](https://github.com/truvity/policy/blob/master/docs/contracts/component.md).

## Status

Used in production by its maintainers. Releases are listed on the
[releases page](https://github.com/truvity/edge/releases), and
[CHANGELOG.md](CHANGELOG.md) says what changed for a consumer in each.

## Development

```sh
devbox shell        # or direnv
just check          # build + lint + test + leak canary
just golden         # regenerate tests/golden after a template change — review the diff
```

CI runs `build`, `lint`, `test` and `leak-canary`, each as its own job;
`vuln` runs daily, never as a required check on a pull request (a new
CVE must not turn CI red). Every
`tests/cases/<chart>/<case>/values.yaml` is rendered and compared
byte-for-byte with `tests/golden/<chart>/<case>.yaml` (a case may pin its
release name in a `release` file next to its values); a template change
is reviewed as a diff, with no cluster involved.

`tests/invalid/<chart>/` holds one fixture per refusal. Each must fail to
render; `just lint` proves it. A rule without a fixture is a rule that
will quietly stop working.

A library chart renders nothing by itself, so its cases and fixtures go
through `tests/harness/<chart>/`, a consumer chart that stands in for the
application including it. `charts/gateway-projects` is generated from
`gateway-groups` and `gateway-policies` by `hack/sync-gateway-projects.sh`
(`just lint` checks it is in step).

## Releasing


Push a tag `vX.Y.Z`. The shared release workflow creates the GitHub
Release and pushes all the charts at that version — a chart's own `version`
field is a placeholder that never moves — and the same tag is the Go
module's version.

Auto-release is present but not armed (`vars.AUTO_RELEASE` is unset), so
every release today is a manual tag. When armed it cuts **patches only**:
at once for a merged `security`-labelled pull request, weekly for
dependency bumps. Minors and majors are always manual, tagged when the
change merges and after its CHANGELOG heading.

## Licence

MIT — see [LICENSE](LICENSE).
