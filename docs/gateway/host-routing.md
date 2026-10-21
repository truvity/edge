# Host routing

The convention this repository's catalog and charts are built for: **one
origin per hostname**, and a path is decided in Envoy, never at the CDN.
The examples use `example.com`.

## The rule

A hostname resolves to one origin. Services that would otherwise share a
name and split it by path each get their own host instead, which costs
one catalog row and no extra mechanism. A CDN on a self-serve plan cannot
send one path of a host to a different origin: the DNS-record override
that does it (an origin rule) is an Enterprise feature, and the
alternatives (edge scripts, an API gateway proxying one path) are code or
buffering that no catalog row declares.

## Where each thing lives

- **An in-cluster service** gets its own host, a domain in a catalog
  group: `<thing>.<cluster>.example.com` where the certificate scheme
  covers a second label (the CDN's own per-name certificates do), and
  `<thing>.example.com`, one label deep, where it does not (a wildcard
  covers one label only).
- **A browser app** composes its paths in Envoy on its own host: `/`,
  `/api`, `/telemetry/collect` are rules of one HTTPRoute, each with its
  own backend, policy and body limit. The CDN in front caches static
  assets, applies its WAF, and rate-limits sign-in paths only.
- **An off-cluster component** (a function behind an API gateway, say)
  keeps its own host and calls the in-cluster services server-side, by
  their catalog hosts. It forwards a token and never the user's cookies
  or `Host`, so the browser sees one origin.
- **Ingestion** (telemetry, webhooks) is a platform host per cluster, such
  as `otlp.<cluster>.example.com`. Body caps and request rates are policy
  rows on the route in Envoy, not CDN rules, which are few on a
  self-serve plan and are better kept for sign-in paths.

## One host shared by several installations

When installations of one service must share a name
(`audit.example.com/<installation>`), the catalog's path grants plus
three more guards keep one installation from answering for another:

1. **Path grants.** A catalog domain takes `paths: [{prefix, namespace}]`
   (see [path grants](reference.md#path-grants-on-a-shared-host)). Each
   namespace holds one prefix, and `gateway-groups` refuses a route
   outside it.
2. **Admission.** Gateway API's listener `allowedRoutes` admits a
   namespace for the whole host, so add an admission policy that lets an
   HTTPRoute naming the host in only from a namespace holding a grant,
   with every match a `PathPrefix` or `Exact` at or under it. A rule with
   no match means `/`, and a regular expression or a lookalike prefix such as `/auditx`
   can escape the grant, so the policy refuses all three. Put the grants
   inline in the policy: with a parameter resource, a missing object is
   refused before the match condition is evaluated, which would block
   every HTTPRoute write in the cluster.
3. **One audience per installation.** The token's audience names the
   installation (`audit:<installation>`), and each backend verifies only
   its own.
4. **The prefix is stripped.** The route rewrites the prefix to `/`, and
   no backend is shared between installations.

## Two traps

- **A `BackendTLSPolicy` binds to a Service, not to a route.** If a
  `TLSRoute` in passthrough mode uses the same Service, Envoy originates
  TLS on the passthrough path as well, and the client's TLS arrives
  wrapped in Envoy's. Give a TLS-originating route its own Service, and
  refuse the combination in a render test.
- **A new host's certificate waits for its issuing role.** If the role
  that issues certificates for catalog hosts has an allow-list derived
  from the catalog, a certificate requested before that list is applied is
  refused and backs off. After the apply, force the renewal of that one
  certificate rather than waiting out the backoff, and never renew every
  certificate at once.
