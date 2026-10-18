# static-site

One generic Go server (`cmd/static-site`) and one chart (`charts/static-site`)
serve every static site. The site is data, an OCI artifact; the server and the
chart are the same for all of them.

```
site CI ──oras push──► registry: static/<site>@sha256:…
                                   │ pull by digest (startup, SIGHUP)
                         static-site pods (several replicas)
                                   │
                         CDN in front (caches by headers, no purge)
```

A promotion tool (Kargo, or anything that can write a Helm value) promotes the
artifact by setting `site.artifact.digest`; the chart takes the digest as a
value of its own, independent of the tag in `site.artifact.ref`.

## The artifact

Push it with `oras`, under `static/<site>` in your registry:

```sh
# a directory: one tar+gzip layer, unpacked at the root of the site
oras push registry.example.com/static/www:main ./dist
# or single files, each named by its title annotation
oras push registry.example.com/static/www:main index.html css/site.css
```

Accepted layers: a tar (gzip or not; a layer with the media type
`application/vnd.oci.image.layer.v1.tar[+gzip]` or the `oras` unpack
annotation), and any other layer with an
`org.opencontainers.image.title` annotation, written as that file. The
manifest must be an image manifest, not an index. Layers are verified against
their digests while they are read. Only regular files and directories are
accepted: a link, device, absolute path or `..` in an archive fails the load.
Size and file count are capped (`site.maxBytes`, `site.maxFiles`).

Registry credentials: see [Registry authentication](#registry-authentication).

## Registry authentication

The server picks one method per pull and logs only its name
(`registry auth: dockerconfig`, `registry auth: ecr-sdk` or
`registry auth: anonymous`), never a credential. With
`STATIC_SITE_REGISTRY_AUTH` unset (`auto`) the order is:

1. a **Docker config file**, when one is configured (`DOCKER_CONFIG` is set, or
   `~/.docker/config.json` exists). The chart mounts the
   `kubernetes.io/dockerconfigjson` Secret named by `site.pullSecretName`
   there. Nothing changes for existing users.
2. **Amazon ECR through the AWS SDK default credential chain**, when the
   registry host is `<account>.dkr.ecr.<region>.amazonaws.com` (the
   `ecr-fips` and `.amazonaws.com.cn` variants too). The server calls
   `ecr:GetAuthorizationToken` itself, decodes the `AWS:<password>` token and
   uses it as basic auth for the registry. It keeps the token, replaces it
   once half of its 12 hours have passed (so a pod that runs for weeks and
   re-pulls on `SIGHUP` keeps working), and replaces it and retries once when
   the registry answers 401. The token goes only to the artifact's registry.
3. **anonymous**.

`STATIC_SITE_REGISTRY_AUTH` can force one: `dockerconfig`, `ecr` (an error for
a host that is not ECR) or `none`. In the chart:

```yaml
registry:
  auth: dockerconfig   # default; site.pullSecretName, or none when it is empty
  # auth: ecr          # the section below
  # auth: none
```

`ecr` renders no pull-secret volume and no `DOCKER_CONFIG`, leaves the
ServiceAccount token mounted (IRSA and Pod Identity need it; the chart
otherwise sets `automountServiceAccountToken: false`), and refuses
`site.pullSecretName`. The chart's `dockerconfig` default sets no
`STATIC_SITE_REGISTRY_AUTH`, so the render is the same as before the key
existed.

### ECR with Pod Identity or IRSA

The pod's own ServiceAccount gets the AWS credentials; nothing borrows another
workload's identity. The SDK chain finds them either way, so the server does
not know which one is in use.

**EKS Pod Identity.** Create a pod identity association for the namespace and
a stable ServiceAccount name; the chart needs no annotation:

```yaml
registry:
  auth: ecr
serviceAccount:
  name: static-site
site:
  artifact:
    ref: 123456789012.dkr.ecr.eu-central-1.amazonaws.com/example:main
```

**IRSA** (a self-managed cluster with an OIDC provider; also EKS). Annotate the
ServiceAccount with the role. The webhook injects `AWS_ROLE_ARN` and
`AWS_WEB_IDENTITY_TOKEN_FILE`:

```yaml
serviceAccount:
  name: static-site
  annotations:
    eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/example-static-site
```

The role needs this and nothing else (the repository ARN is the one the
artifact lives in):

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": "ecr:GetAuthorizationToken",
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": [
        "ecr:BatchGetImage",
        "ecr:GetDownloadUrlForLayer",
        "ecr:BatchCheckLayerAvailability"
      ],
      "Resource": "arn:aws:ecr:eu-central-1:123456789012:repository/example"
    }
  ]
}
```

`ecr:GetAuthorizationToken` does not support resource-level permissions, hence
`*`. Egress to the ECR API (`api.ecr.<region>.amazonaws.com`) and to STS (IRSA)
or the Pod Identity agent must be allowed next to the registry: the chart's
`networkPolicy` allows HTTPS egress, and the Pod Identity agent address
(`169.254.170.23`) needs an `extraEgress` rule where egress is restricted.

### ECR on EKS with ACK: two charts

Where the cluster runs ACK (`roles.iam.services.k8s.aws`,
`policies.iam.services.k8s.aws`, `podidentityassociations.eks.services.k8s.aws`),
the pod's AWS identity is created by a chart of its own,
`static-site-infra`, and `static-site` only uses it. The split is rule 11 of
the [platform contract](https://github.com/truvity/policy/blob/v1.51.0/docs/contracts/platform.md)
and [decision 0013](https://github.com/truvity/policy/blob/v1.51.0/docs/decisions/0013-pods-are-admitted-with-their-identity.md)
explains why:

- EKS Pod Identity associations are eventually consistent. The ACK controller
  reports an association `Synced` when the AWS API returns, seconds before the
  cluster's admission webhook can see it, and credentials are injected only
  when a pod is *created*; a container restart never repeats it. A pod created
  in the gap runs for its whole life without credentials and cannot pull its
  site. Sync waves and health checks wait on the status, which is already
  `Synced`, so they cannot close the gap.
- So `static-site` starts with a check: a `pre-install,pre-upgrade` hook Job
  (`restartPolicy: Never`, so every retry is a new pod admitted afresh) that
  runs as the site's ServiceAccount and asks STS who it is. It passes only
  once a pod admitted now has the identity; then the Deployment is created.
  With `registry.ack.accountId` set (the key `static-site-infra` already takes,
  so one set of values feeds both) it also proves the pod runs in that AWS
  account, not only under the role.
- A hook runs before anything else in its release, so the ServiceAccount
  cannot be the application release's. It is `static-site-infra`'s, rendered
  after the association.

| chart | renders |
|---|---|
| `static-site-infra` | the ACK `Policy` (wave -30), `Role` (-25) and `PodIdentityAssociation` (-20), then the `ServiceAccount` (-15) with `argocd.argoproj.io/sync-options: Prune=false,Delete=false`; with `networkPolicy.enabled`, the egress of the check |
| `static-site` | the Deployment and the rest, the identity check hook; no ACK objects and no ServiceAccount (it names `serviceAccount.name`, default the fullname) |

`static-site` derives which case it is in: `registry.auth=ecr` with
`registry.ack.enabled`. There is no other switch. In every other case (`none`,
`dockerconfig`, IRSA, ack off) it behaves as before and creates its own
ServiceAccount when `serviceAccount.create`.

The keys have the same names in both charts, so the estate keeps one set of
values and passes each chart the keys it declares (each schema rejects the
other's keys):

```yaml
# static-site-infra
registry:
  auth: ecr
  ack:
    enabled: true
    clusterName: example                       # required
    repositoryArns:                            # required
      - arn:aws:ecr:eu-central-1:123456789012:repository/example
    # rolePrefix: example-default-static-site  # name of the Role and Policy;
    #                                          # default <clusterName>-<namespace>-<installName>
    # tags: {project: example}                 # on the Role; `cluster` is always set
    # permissionsBoundary: arn:aws:iam::123456789012:policy/example-boundary
    # accountId: "123456789012"                # with region: pins the trust to
    # region: eu-central-1                     # the cluster's account and ARN
serviceAccount:
  name: ""                                     # default the fullname of the site
  annotations: {}
networkPolicy:
  enabled: false                               # egress of the check, see below
installName: ""                                # default this release's name
nameOverride: ""
fullnameOverride: ""
```

```yaml
# static-site (the same registry.ack keys are accepted and ignored but for enabled and accountId)
registry:
  auth: ecr
  ack:
    enabled: true
    # accountId: "<account-id>"                # optional: the hook also checks the pod runs in this
    #                                          # AWS account (-expect-account; needs identity-check >= 0.34.0)
serviceAccount:
  name: ""
images:
  identity-check:                              # the hook's image, pinned by digest
    registry: ghcr.io
    repository: truvity/k8s/identity-check
    tag: 0.34.0
    digest: sha256:9fc40a066f5455eb807f0826f7083b0ead46924956af3c8ed5e2c381233d1a38
```

`static-site-infra` must be installed with the **same release name** as
`static-site` (or give `installName` the application's release name): the
default Role and Policy name `<clusterName>-<namespace>-<installName>`, the
object labels and the default ServiceAccount name derive from it, and they are
the names the objects have today. It renders nothing unless `registry.auth` is
`ecr` and `registry.ack.enabled` is set.

The Policy holds the actions of the server (`ecr:GetAuthorizationToken` on `*`;
`ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`,
`ecr:BatchCheckLayerAvailability` on `repositoryArns`), the Role trusts
`pods.eks.amazonaws.com` (`sts:AssumeRole`, `sts:TagSession`) for this
namespace and ServiceAccount, with the Policy in `policyRefs`, and the
`PodIdentityAssociation` binds the cluster, namespace and ServiceAccount to the
Role (`roleRef`). The actions are written in the template and a Go test
compares them with `staticsite.ECRAccountActions` and `ECRRepositoryActions`,
the lists the server's own guard tests check against what it calls; another
checks the waves, the guard and the split.

**Install order.** `static-site-infra` first, and healthy, then `static-site`:

- *Argo CD, two Applications* (recommended): one for each chart, the infra one
  synced and healthy before the application one is created or synced (an
  app-of-apps wave, or `argocd.argoproj.io/sync-wave` on the two Application
  objects with a health check that waits for the ACK objects and the
  ServiceAccount). Waves order the objects of one Application, not two
  Applications, and the hook of the second needs the first one's ServiceAccount.
  The infra Application's ServiceAccount carries `Prune=false,Delete=false`
  whatever either chart does later.
- *Plain Helm:* `helm upgrade --install www oci://ghcr.io/truvity/charts/static-site-infra -f infra-values.yaml`,
  wait for the ACK objects (`kubectl wait --for=condition=ACK.ResourceSynced`),
  then `helm upgrade --install www oci://ghcr.io/truvity/charts/static-site -f values.yaml`.
  Helm runs the hook as part of the second release.

**The check's network.** The check pod has labels no selector of `static-site`
matches (`app.kubernetes.io/name` and `component` are `static-site-identity-check`
and the pod carries `service-lib.io/identity-check`), so the site's Service,
PodDisruptionBudget and NetworkPolicy never select it: it does not receive
site traffic and the site's NetworkPolicy does not restrict it. In a namespace
that denies egress by default it needs its own: name resolution, the Pod
Identity agent (`169.254.170.23:80`) and HTTPS to STS.
`static-site-infra` renders that policy with `networkPolicy.enabled: true`.

**The guard.** The Policy, Role and PodIdentityAssociation carry
`argocd.argoproj.io/sync-options: Prune=false,Delete=false`, like the
ServiceAccount. Until the handover is done the live objects are tracked by the
site's old Application, which refreshes independently and could re-take
tracking; its sync would then prune them and ACK would delete the IAM Role.
Argo CD reads the sync option from the live object, and the server-side apply
by the infra Application puts the annotation there, where the previous owner
cannot remove it. `Delete=false` also keeps the Role if the infra Application is
deleted.

**Adoption.** The Policy and Role carry
`services.k8s.aws/adoption-policy: adopt-or-create` (ACK runs with
`ResourceAdoption=true`), so a create that finds the object already in AWS
adopts it instead of failing with `Resource already exists` forever, which
would block the Role and the association that reference it. This recovers an
install whose first create reached AWS while ACK's own role was missing. ACK
adopts by a resource's primary identifier: a Role's is its name, but a managed
policy's is its ARN, so the Policy also carries
`services.k8s.aws/adoption-fields: '{"arn":"arn:aws:iam::<accountId>:policy/<name>"}'`,
built from `registry.ack.accountId` (rendered only when that is set; without
it only the Role is adopted and the Policy create still fails with
`EntityAlreadyExists`). The
PodIdentityAssociation is not adopted: AWS identifies it by (cluster, namespace,
ServiceAccount), and adopting it could let a legacy owner's deletion remove the
association under the new object.

**Moving an existing install.** The Policy, Role and PodIdentityAssociation
render as `static-site` v3 rendered them (same names, labels and specs) plus the
wave and the guard, and the ServiceAccount has the same name, labels and
annotations plus the wave and the guard. **Use `static-site-infra` 3.12.1 or
later for the handover** (3.12.0 lacks the guard on the ACK objects). Install it
with the site's values *before* upgrading `static-site`: Argo CD adopts the live
objects, and the upgraded `static-site` no longer renders them. Upgrade
`static-site` first and it prunes the ACK objects, and ACK deletes the IAM Role.
See the Upgrade section of the changelog.

The namespace needs the estate's ACK project wiring: the ACK role that applies
these objects is confined on the `project` tag (`aws:ResourceTag/project`), so
put `project: <name>` in `ack.tags`. A refused apply reads as an ACK condition
on the object, not as a failed pod.

**IRSA** (hive, any cluster without EKS Pod Identity) stays values only: create
the role and its trust outside the chart and set
`serviceAccount.annotations["eks.amazonaws.com/role-arn"]`. `registry.ack`
stays off (and `static-site` creates its ServiceAccount, with no identity check).

**A repository in another account** needs both sides: the Policy here (it
grants on whatever ARNs it is given), and a repository policy in the registry's
account that admits the role (or its account) to `ecr:BatchGetImage`,
`ecr:GetDownloadUrlForLayer` and `ecr:BatchCheckLayerAvailability`. The chart
cannot create that half. `ecr:GetAuthorizationToken` is always checked in the
role's own account.

If the server cannot get a token (no identity bound, a missing permission) the
pull fails with the `GetAuthorizationToken` error and the pod stays not ready;
the log line names the call, not a credential.

## Serving

| | |
|---|---|
| Content-Type | from a built-in table, then the host's types, else `application/octet-stream`. Never sniffed; `X-Content-Type-Options: nosniff` is sent |
| ETag | strong, a hash of the bytes served (after substitution). `If-None-Match` answers 304. No `Last-Modified`, so replicas agree |
| hashed names | `Cache-Control: public, max-age=31536000, immutable` when the base name matches `cache.hashedPattern` (default: a `.`, `-` or `_` then 8 or more hex digits then the extension, such as `app.3f9a1c2b.js`). HTML is never immutable |
| everything else | `public, max-age=<cache.htmlMaxAge>` (default 60s) `, stale-while-revalidate=<cache.staleWhileRevalidate>` (default 300s) |
| directories | `/dir/` serves `/dir/index.html`; `/dir` redirects to `/dir/`; a directory with no index is a 404, never a listing |
| 404 | `site.notFoundPage` (default `404.html`) with status 404 and `no-store`, else a plain 404 |
| SPA | with `site.spaFallback`, an extensionless path that matches no file serves `/index.html` with 200; a missing asset is still a 404 |
| methods | GET and HEAD; anything else is 405 |
| paths | a `..` segment, NUL or backslash is a 400. Only files indexed from the unpacked tree can be served, and symlinks are not indexed |
| headers | `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: strict-origin-when-cross-origin`; `headers` adds or overrides, an empty value removes one (add a `Content-Security-Policy` here) |

There is no cache purge: a new site version has new hashed assets, and its HTML
is stale for at most `htmlMaxAge` plus `staleWhileRevalidate`. The server does
not compress; let the CDN do it.

## Placeholders

A static site is built once and the same bytes run in every environment, so a
value that differs per environment (a Google Tag Manager id, an API base URL)
is filled in at serve time. `{{ name }}` (spaces optional) in `.html`, `.htm`,
`.js`, `.mjs`, `.css`, `.json` and `.webmanifest` files is replaced when the
content loads (startup, `SIGHUP` reload), not per request, by the value of the
named placeholder.

- Other file types (images, fonts, `.txt`, `.xml`) are never touched.
- A value may use only `A-Za-z0-9 ._:/@#+=,~-` (at most 512 characters); the
  server refuses to start on anything else, and the chart's schema refuses it
  at render. These values land in public pages: they are configuration, not
  credentials.
- Values are also escaped for the file type (HTML entities; JS `\xNN`; CSS
  `\hex `; JSON string escapes). In JS and JSON the token has to sit inside a
  string literal: `var id = '{{ gtmId }}'`.
- The ETag and Content-Length are computed after substitution.
- Values are never logged. The server logs the placeholder names at start; an
  error names the file and the placeholder, never a value.

### Sources

| Source | Variable | Notes |
| --- | --- | --- |
| Directory, one file per name | `STATIC_SITE_PLACEHOLDERS_DIR` | A mounted Secret. The safer default: a value in a file is not in the process environment, `kubectl describe` or a crash dump. Only the names declared in `_KEYS` and `_OPTIONAL_KEYS` are read, so an extra key is never served. One trailing newline is trimmed. |
| Environment | `STATIC_SITE_PLACEHOLDER_NAMES`, `STATIC_SITE_PLACEHOLDER_<NAME>` | Value in the environment of the container. |
| JSON file | `STATIC_SITE_PLACEHOLDERS_FILE` | `{"name": "value"}`; what the chart's inline values use. |

### Strict mode

Without `STATIC_SITE_PLACEHOLDERS_STRICT=true` a token that is not a known
name is left exactly as written, so a site that happens to carry `{{ x }}` (a
template engine's own syntax) still serves. In strict mode the load fails
(the server does not start; a reload keeps the previous content) when:

- a token names something that is not a declared placeholder (unknown), or
- a token names a declared placeholder that has no value, unless it is
  listed in `STATIC_SITE_PLACEHOLDERS_OPTIONAL_KEYS`, in which case it is
  replaced by the empty string.

A declared name with no value that no file uses is not an error.

### In the chart

`placeholders` keeps its inline `name: value` form. Three reserved keys add a
Secret source, and nothing is rendered for it while `secretName` is empty:

```yaml
placeholders:
  secretName: static-site-values   # mounted read-only at /etc/static-site-secrets
  secretKeys: [gtmId]                    # expected names, one key each
  optionalKeys: [bannerText]             # may be absent; served as empty
```

The chart sets strict mode when `secretName` is set. It creates the Secret
only through `externalSecret` (below). The values are read when the pod starts, so a changed Secret takes a
restart: roll the Deployment (for example with a reloader annotation in
`podAnnotations`). The inline values roll the pods by themselves.

#### The chart renders the ExternalSecret

With the reserved key `externalSecret` the chart renders the
`external-secrets.io/v1` ExternalSecret that fills the Secret, so an estate
passes only names. It is unset by default: nothing is rendered and nothing
changes for an existing install.

```yaml
placeholders:
  secretName: static-site-values   # the target Secret (required)
  secretKeys: [gtmId]
  optionalKeys: [bannerText]
  externalSecret:
    storeRef:
      kind: ClusterSecretStore     # or SecretStore
      name: example-store
    remoteKey: static-sites/docs.example/placeholders   # one key in the store
    properties:                    # Secret key: property under remoteKey
      gtmId: gtm-id
      bannerText: banner-text
    refreshInterval: 1h            # default
    ignoreHealthcheck: true        # default; see below
    annotations: {}                # more annotations on the ExternalSecret
```

- The ExternalSecret is named `placeholders.secretName`, in the release
  namespace, and its target Secret has the same name (`creationPolicy: Owner`).
- The keys of `properties` must be exactly `secretKeys` plus `optionalKeys`;
  any difference fails the render, as does `externalSecret` without
  `secretName`. `storeRef` (`kind`, `name`), `remoteKey` and `properties` are
  required.
- Each entry reads `remoteRef` with `conversionStrategy: Default`,
  `decodingStrategy: None`, `metadataPolicy: None` and `nullBytePolicy: Ignore`.
- It carries `argocd.argoproj.io/ignore-healthcheck: "true"` by default: the
  values are written by hand after the install, and an unwritten value must
  not hold the Argo CD application Degraded (the ExternalSecret still reports
  its own state). Set `ignoreHealthcheck: false` to drop it.

For an estate without External Secrets, create the Secret yourself. An
ExternalSecret that fills it, as the chart would render it (the store, key
paths and namespace are yours):

```yaml
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata:
  name: static-site-values
spec:
  refreshInterval: 1h
  secretStoreRef:
    kind: ClusterSecretStore
    name: example-store
  target:
    name: static-site-values
    creationPolicy: Owner
  data:
    - secretKey: gtmId
      remoteRef:
        key: static-sites/docs.example/placeholders
        property: gtm-id   # the value is e.g. GTM-XXXXXXX
```

A plain `kubectl create secret generic static-site-values --from-literal=gtmId=...`
works the same: the chart only mounts the Secret named by `secretName`.

## Operations

- Site port 8080, ops port 8081: `/healthz` (process up), `/readyz` (content
  loaded and not shutting down), `/metrics` (Prometheus: requests, duration,
  loads, `static_site_content_info{digest}`, load time, file count).
- JSON logs on stdout; requests are logged at info only when they end in 4xx or
  5xx (`STATIC_SITE_LOG_LEVEL=debug` for all).
- `SIGHUP` reloads (a moved tag is re-resolved); a failed reload keeps serving
  the previous content. The previous copy is deleted after 30 seconds. The
  content volume holds both during a reload, which is why it is sized (and
  memory-limited) for two.
- On SIGTERM the server turns not-ready, waits `STATIC_SITE_SHUTDOWN_DELAY`
  (5s), then drains. The image is `gcr.io/distroless/static:nonroot`; the chart
  runs as 65532 with a read-only root file system and no capabilities.

All server settings are `STATIC_SITE_*` environment variables, listed in
`internal/staticsite/config.go`; the chart sets them from its values.
`STATIC_SITE_DIR` serves a local directory instead of pulling, for development.

## The chart

`site.artifact.ref` is `registry/repository[:tag]` (no digest), `site.artifact.digest`
is `sha256:…`; the digest wins over a tag. Replicas default to 2 with a
PodDisruptionBudget of `minAvailable: 1`. `serviceMonitor.enabled` renders a
ServiceMonitor for the ops port; `networkPolicy.enabled` allows ingress to the
site and ops ports (restricted by `ingressFrom` and `metricsFrom` when set) and
egress to DNS and HTTPS. `values.schema.json` is strict at the top level, and
every refusal has a fixture under `tests/invalid/static-site/`.

## Release

The image is built by this repository's goreleaser configuration (the `kos`
section, `gcr.io/distroless/static:nonroot`, linux/amd64 and arm64) and pushed
by the shared release workflow's `ko-docker-repo` input to
`ghcr.io/truvity/edge/static-site:<version>`, in the same job and at the same
tag as the charts and the Go module. The chart's `appVersion` is stamped with
that version and is the default `image.tag`, so a chart release always runs the
server release it was published with.
