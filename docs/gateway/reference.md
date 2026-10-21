# Reference

Every value of every chart. `charts/<chart>/values.yaml` carries the same
keys as commented defaults, and `values.schema.json` is the authority on
types: an unknown key fails the render.

## gateway-fleet

### Top level

| Value | Default | Notes |
|---|---|---|
| `controllerName` | `gateway.envoyproxy.io/gatewayclass-controller` | as installed by upstream's `gateway-helm` |
| `commonAnnotations` | `{}` | on every rendered object; an object's own annotations override matching keys |
| `classes` | `{}` | one GatewayClass and one EnvoyProxy per entry |
| `exposures` | `{}` | one Gateway per entry |

### `clientTraffic`

| Value | Default | Notes |
|---|---|---|
| `clientTraffic.earlyRequestHeaders.remove` | `[]` | lowercase header names removed from every request before any HTTP filter runs, on every ClientTrafficPolicy the chart renders (`spec.headers.earlyRequestHeaders.remove`). Empty renders no `headers` block, so an existing render is unchanged. Needs `clientTrafficPolicy.enabled: true` on every enabled exposure (the render fails otherwise). Refused: pseudo-headers (`:authority`), uppercase or non-token names, duplicates, and `host`, `content-length`, `transfer-encoding`, `connection`, `keep-alive`, `upgrade`, `te`, `trailer`, `cookie`, `authorization`, `proxy-authorization` |
| `clientTraffic.connection.maxConnectionDuration` | unset | cap on how long a downstream connection lives, as a Gateway API duration (`5m`, `1h30m`); renders `spec.connection.connectionLimit.maxConnectionDuration` on every ClientTrafficPolicy the chart renders. Unset renders nothing |
| `clientTraffic.idleTimeout` | unset | `spec.timeout.http.idleTimeout`: how long a connection with no active request is kept (Envoy default: 1h). Unset renders nothing |
| `clientTraffic.http1.disableSafeMaxConnectionDuration` | unset | `spec.http1.disableSafeMaxConnectionDuration`. By default an HTTP/1 connection past the cap is closed at the next request (with `Connection: close`); `true` closes it shortly after the cap instead. Refused without `connection.maxConnectionDuration`. Unset renders nothing |

Why the connection settings exist: a client that keeps connections open
(cloudflared's tunnel) stays on the Envoy filter chain it first reached. After
a ListenerSet move that chain is drained and the client gets 404 until it
reconnects (38 minutes on 2026-09-17). With `connection.maxConnectionDuration`
set on the single Gateway-level policy, the client reconnects within the cap
whatever order the move was synced in, so recovery no longer depends on sync
order. The policy that carries the claim-header removal list carries the cap
too, so the list is not repeated on a second policy (a ListenerSet-level
policy would override this one). HTTP/2 streams are cut at the cap, so pick a
value longer than your slowest legitimate request. Any of the three settings
makes a policy with `tls.enabled: false` non-empty; each needs
`clientTrafficPolicy.enabled: true` on every enabled exposure.

Why the removal list exists: the JWT filter's `claimToHeaders` (see `gateway-policies`)
copies a verified claim into a header by ADDING it. A header the client
already sent under that name is not replaced; it stays first, and a backend
that reads the first value takes the client's. A signed-in user could then
present another identity to any backend that trusts the header. Listing the
header here removes the client's copy before the JWT filter runs. List every
header named in any `claimToHeaders`. To add the removal to an exposure
that has no TLS floor of its own, enable its `clientTrafficPolicy` with
`tls.enabled: false`: the policy then carries only `headers`.

Coverage (Envoy Gateway v1.9.1): a ClientTrafficPolicy that targets a
Gateway applies to every listener of that Gateway, including the listeners
that come from ListenerSets attached to it, except listeners claimed by a
more specific policy (a `sectionName` target, or a policy targeting the
ListenerSet). That more specific policy replaces this one for its listeners
without merging, so it must carry its own removal. On a merged Gateway
class, a policy per Gateway is accepted when the listeners are HTTPS (each
has its own filter chain, and early header mutation is per filter chain). On
a plain-HTTP port shared by listeners of two Gateways, Envoy Gateway refuses
the policy ("applied to multiple http (non https) listeners on the same
port"), so keep such listeners on one Gateway. A listener-level ClientTrafficPolicy
(`gateway-policies` `tlsPolicies` with a `sectionName`) replaces the
Gateway-level one for its listener and carries no such list, so do not
point one at a listener whose claim headers matter.

### `metrics`

Off by default: an existing render does not change.

| Value | Default | Notes |
|---|---|---|
| `metrics.podMonitor.enabled` | `false` | renders `monitoring.coreos.com/v1` PodMonitors; the CRD must exist |
| `metrics.podMonitor.interval` | `""` | empty: the scraper's own default |
| `metrics.podMonitor.labels` | `{}` | on every PodMonitor, for a scraper that selects by label |
| `metrics.podMonitor.annotations` | `{}` | merged over `commonAnnotations` |
| `metrics.podMonitor.proxy.enabled` | `true` | one PodMonitor per namespace that holds a proxy (each enabled class's namespace, and the namespace of an exposure with a proxy of its own) |
| `metrics.podMonitor.proxy.name` | `envoy-proxy` | |
| `metrics.podMonitor.proxy.port`, `.path` | `metrics`, `/stats/prometheus` | the proxy's stats port (19001) |
| `metrics.podMonitor.proxy.keep` | the families Envoy Gateway's own dashboards read | regexes over the whole metric name, joined into one `keep` rule. **`[]` keeps every series** and renders no rule. Setting the key replaces the list |
| `metrics.podMonitor.proxy.metricRelabelings` | `[]` | further rules, appended after the keep rule, verbatim. Each must write `action`; the render fails otherwise |
| `metrics.podMonitor.controller.enabled` | `true` | the controller is upstream's chart; this chart only scrapes it |
| `metrics.podMonitor.controller.name`, `.namespace` | `envoy-gateway`, `envoy-gateway-system` | |
| `metrics.podMonitor.controller.port`, `.path` | `metrics`, `/metrics` | |
| `metrics.podMonitor.controller.selector` | `control-plane: envoy-gateway`, `app.kubernetes.io/instance: envoy-gateway` | |
| `metrics.networkPolicy.from` | `[]` | NetworkPolicy peers; each exposure NetworkPolicy (`networkPolicy.enabled`) gains one ingress row for them, last. Empty: no row. The controller's own policy is not this chart's |
| `metrics.networkPolicy.port` | `19001` | |

The default `keep` list exists because Envoy exports a stat family per
upstream cluster and a merged fleet has one cluster per routed backend:
unfiltered, the proxies are the largest scrape on an estate. The list is
checked against the dashboards `envoy-proxy-global.json` and
`envoy-clusters.json` of Envoy Gateway v1.9.2. Every rule carries `action`
explicitly: the prometheus-operator CRD defaults it, but the
VictoriaMetrics operator's conversion should not have to.

### `classes.<name>`

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | |
| `namespace` | `envoy-gateway-system` | where the class's EnvoyProxy and, by default, its exposures live |
| `annotations` | `{}` | |
| `mergeGateways` | `false` | `true` shares one proxy across every Gateway of the class; no exposure may then bring its own. `false` gives each exposure its own Deployment and Service |
| `proxy.enabled` | follows `mergeGateways` | a merged class needs its class proxy; a split one does not, because each exposure brings its own |
| `proxy.name` | `<class>-config` | the EnvoyProxy's name |
| `proxy.annotations` | `{}` | |
| `proxy.filterOrder` | `[]` | verbatim `EnvoyProxy.spec.filterOrder` |
| `proxy.replicas` | `3` | one per zone in a three-zone cluster, with `pod.zoneSpread` |
| `proxy.podDisruptionBudget.maxUnavailable` | `1` | one proxy at a time, whatever the replica count; `0` or `null`: no PodDisruptionBudget |
| `proxy.podDisruptionBudget.minAvailable` | `null` | set: rendered instead of `maxUnavailable` (EnvoyProxy takes one of the two); `0`: no PodDisruptionBudget |
| `proxy.pod.tolerations`, `.nodeSelector`, `.affinity`, `.topologySpreadConstraints` | empty | scheduling is the estate's |
| `proxy.pod.zoneSpread` | `""` | `DoNotSchedule` or `ScheduleAnyway`: one proxy per zone (`maxSkew: 1` on `topology.kubernetes.io/zone`), selected by the proxy's own pod labels and `matchLabelKeys: [pod-template-hash]`, so a rolling update spreads the new pods by themselves; `presets/fleet.yaml` sets `DoNotSchedule` |
| `proxy.service.name` | `gateway-<class>` | |
| `proxy.service.type` | `ClusterIP` | |
| `proxy.service.clusterIP` | `""` | a pinned IPv4 address, or `None` for headless; reaches the Service through the controller's patch hook |
| `proxy.service.annotations`, `.labels` | `{}` | cloud annotations belong here |
| `proxy.service.loadBalancerClass` | `""` | |
| `proxy.service.loadBalancerSourceRanges` | `[]` | |
| `proxy.service.externalTrafficPolicy` | `""` | |
| `proxy.service.patch` | `{}` | `{type, value}`, merged over the fields above |
| `proxy.useListenerPortAsContainerPort` | `false` | |
| `proxy.shutdown.drainTimeout` | `""` | |
| `proxy.shutdown.minDrainDuration` | `""` | |
| `proxy.shutdown.healthCheckFailureDelay` | `""` | fail readiness this long before draining, so a load balancer removes the endpoint first |
| `proxy.backendTLS.clientCertificateRef` | unset | `{name, namespace}`: the certificate Envoy presents to a backend that asks for one |
| `proxy.accessLog.extraFields` | `{}` | fields added to the controller's default JSON access log (`name: command operator`); see [safety.md](safety.md#adding-one-access-log-field-does-not-drop-the-others) |
| `proxy.extraSpec` | `{}` | deep-merged over the generated EnvoyProxy spec, last |

### `exposures.<name>`

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | |
| `class` | *required* | must name an enabled class |
| `namespace` | the class namespace | |
| `gatewayName` | the map key | a DNS label |
| `annotations`, `labels` | `{}` | |
| `allowedListeners.namespaces.from` | `Same` | `None`, `Same`, `All` or `Selector`; which ListenerSets may attach |
| `allowedListeners.namespaces.selector` | unset | required, and non-empty, with `Selector` |
| `health.listenerName` | `https` or `http`, from `protocol` | |
| `health.hostname` | *required* | exact, never a wildcard |
| `health.port` | `443` | |
| `health.protocol` | `HTTPS` | |
| `health.tls.secretName` | `<gateway>-health-tls` | |
| `health.certificate.enabled` | `true` | |
| `health.certificate.issuerRef` | *required* when enabled | `{name, kind, group}`; the issuer is the estate's |
| `health.certificate.name`, `.annotations`, `.labels`, `.duration`, `.renewBefore`, `.privateKey`, `.usages` | cert-manager defaults | passed through to the Certificate |
| `health.allowedRoutes.namespaces.from` | `Same` | |
| `health.allowedRoutes.kinds` | `HTTPRoute` | must be a kind the listener's protocol accepts |
| `health.directResponse.enabled` | `false` | answers without a backend, so the exposure can be probed when everything behind it is down; needs `health.allowedRoutes.namespaces.from` `Same` or `All` |
| `health.directResponse.routeName`, `.filterName` | `<gateway>-health` | |
| `health.directResponse.path` | `/healthz` | |
| `health.directResponse.statusCode` | `200` | |
| `health.directResponse.contentType` | `text/plain` | |
| `health.directResponse.body` | `ok` | |
| `clientTrafficPolicy.enabled` | `false` | the TLS floor for everything the exposure terminates; attached ListenerSets inherit it |
| `clientTrafficPolicy.name` | `<gateway>-tls` | |
| `clientTrafficPolicy.tls.enabled` | `true` | `false` renders no `tls` block, so the policy carries only `clientTraffic.earlyRequestHeaders.remove` and the exposure's TLS settings stay Envoy Gateway's defaults. Refused with `clientValidation.enabled`, and refused when the remove list is empty (the policy would render nothing) |
| `clientTrafficPolicy.tls.minVersion` / `.maxVersion` | `"1.3"` / `"1.3"` | |
| `clientTrafficPolicy.tls.clientValidation.enabled` | `false` | turns the exposure into one only a certificate holder can reach |
| `clientTrafficPolicy.tls.clientValidation.optional`, `.allowExpiredCertificate` | `false` | |
| `clientTrafficPolicy.tls.clientValidation.caCertificateRefs` | `[]` | `[{kind: ConfigMap\|Secret, name}]` |
| `proxy` | `{}` | this exposure's own proxy, same shape as `classes.<name>.proxy`; only on a split class |
| `networkPolicy.enabled` | `false` | |
| `networkPolicy.name` | `envoy-<exposure>` | |
| `networkPolicy.ingress`, `.egress` | `[]` | the rules are the estate's |
| `networkPolicy.xds.enabled` | `true` | the egress to the controller's xDS port; structural, because without it the proxies never get a configuration |
| `networkPolicy.xds.controllerPodLabels` | `control-plane: envoy-gateway` | |
| `networkPolicy.xds.port` | `18000` | |

## gateway-groups

### Top level

| Value | Default | Notes |
|---|---|---|
| `commonAnnotations` | `{}` | |
| `defaults` | see below | merged under every group, so a shared issuer or exposure is stated once |
| `groups` | `{}` | one ListenerSet per entry |

### `defaults` and `groups.<name>`

`defaults` takes `namespace`, `parent`, `port`, `protocol`, `certificate`
and `allowedRoutes`; a group may set any of them, and the group's value
wins. The other keys are per group only.

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | per group only |
| `namespace` | `envoy-gateway-system` | where the ListenerSet and Certificates live: the exposure's namespace, not the project's |
| `listenerSetName` | the map key | per group only |
| `parent.name` | *required* | the exposure's Gateway |
| `parent.namespace` | the group's namespace | |
| `annotations`, `labels` | `{}` | per group only |
| `domains` | *required*, non-empty | a hostname string, or `{host, listenerName, secretName, certificateName, certificate}` to give one domain its own names or issuer; one leading `*.` is allowed |
| `port` | `443` | |
| `protocol` | `HTTPS` | |
| `certificate.enabled` | `true` | |
| `certificate.issuerRef` | *required* when enabled | once in `defaults`, per group, or per domain |
| `certificate.annotations`, `.labels`, `.duration`, `.renewBefore`, `.privateKey`, `.usages` | cert-manager defaults | |
| `allowedRoutes.namespaces.from` | `Same` | `All`, `Same` or `Selector`; a selector naming a label the platform writes is how a grant is expressed |
| `allowedRoutes.namespaces.selector` | unset | required, and non-empty, with `Selector` |
| `allowedRoutes.kinds` | `HTTPRoute` | |
| `backendTLS.enabled` | `false` | re-encrypt to the group's backend |
| `backendTLS.name` | the ListenerSet name | |
| `backendTLS.targetRefs` | *required* when enabled | `[{group, kind: Service, name, sectionName}]` |
| `backendTLS.validation` | *required* when enabled | `{caCertificateRefs, hostname}`; TLS that is never verified is not re-encryption |
| `networkPolicy.enabled` | `false` | the other half of the grant: the namespace label lets the route attach, this lets the packet arrive |
| `networkPolicy.name` | `allow-gateway-<group>` | |
| `networkPolicy.namespaces` | *required* when enabled | where to render, usually the namespaces whose routes attach |
| `networkPolicy.podSelector` | `{}` | every pod of the namespace |
| `networkPolicy.from.namespace` | *required* when enabled | the namespace the gateway's pods run in |
| `networkPolicy.from.podLabels` | *required* when enabled | the gateway's pod labels |
| `networkPolicy.ports` | *required* when enabled | `[{port, protocol}]` |

### `groups.<name>.routes`

The HTTPRoutes the **platform** owns on a group's ListenerSet: a console the
estate publishes, a read path, a door for a machine caller. A project's own
route stays in the project's chart (`gateway-routes`); a route in this list
is for the case where the workload's chart must not choose which listener
answers for it. Standard Gateway API only: a policy on a route (a body
limit, a rate limit) is the gateway implementation's. A disabled group
renders none of its routes.

| Value | Default | Notes |
|---|---|---|
| `routes[].name` | *required* | the HTTPRoute's object name; unique per namespace across all groups |
| `routes[].namespace` | *required* | the namespace of the route, which the group's `allowedRoutes` must admit |
| `routes[].annotations`, `.labels` | `{}` | an annotation overrides the same key of `commonAnnotations`, which is how a route takes a sync wave of its own |
| `routes[].hostnames` | every domain of the group | each must be a domain the group serves; a hostname the group does not serve attaches to no listener, so it fails the render |
| `routes[].rules[].name` | unset | the section name a policy targets; unique within the route |
| `routes[].rules[].matches` | *required*, non-empty | HTTPRouteMatch entries, verbatim (path, method, headers, query parameters); a rule with no match would answer every request |
| `routes[].rules[].backend` | unset | `{name, port, group, kind, weight}`; group `""`, kind `Service` and weight `1` are written out, because an undeclared default inside a list item is a permanent GitOps diff |
| `routes[].rules[].backends` | unset | the same, several, for a split; not with `backend` |
| `routes[].rules[].filters` | unset | HTTPRoute filters, verbatim; a rule needs a backend or filters (a redirect has no backend) |
| `routes[].rules[].extra` | `{}` | deep-merged over the rule, last: timeouts, retry, session persistence |

The parent is the group's own ListenerSet, so a route cannot name a listener
that does not exist.

## gateway-policies

The policies that protect what the other two charts expose. With an empty
values file it renders one object — the TLS floor for the release
namespace — and authenticates nothing. Written against the Envoy Gateway
1.9.1 CRDs (see [adoption.md](adoption.md#prerequisites)).

### Worked example

Neutral names throughout; an abridged copy of the `full` golden case
(`tests/cases/gateway-policies/full/values.yaml`), whose render is
`tests/golden/gateway-policies/full.yaml`.

```yaml
commonAnnotations:
  example.com/sync-wave: "70"

tlsBaseline:
  namespaces: [gateways, example-tools]
  annotations:
    example.com/sync-wave: "66"

tlsPolicies:
  strict-listener:                  # one listener held to TLS 1.3 only
    namespace: gateways
    targetRefs:
      - name: public
        sectionName: partners
    tls: {minVersion: "1.3", maxVersion: "1.3"}

defaults:
  oidc:
    issuer: https://id.example.com
    authorizationEndpoint: https://id.example.com/authorize
    tokenEndpoint: https://id.example.com/token
    endSessionEndpoint: https://id.example.com/end_session
    scopes: [openid, email, profile, offline_access]
    idToken:
      enabled: true
      header: x-example-id-token
      providerName: example-id
      remoteJWKS:
        uri: https://id.example.com/keys
        backendRefs:                # keys from the issuer's Service next door
          - {name: issuer, namespace: example-id, port: 8080}
      claimToHeaders:
        - {header: x-auth-request-user, claim: sub}
        - {header: x-auth-request-email, claim: email}
  jwt:
    issuer: https://id.example.com
    remoteJWKS:
      uri: https://id.example.com/keys

securityPolicies:
  console:                          # anyone signed in; CSRF in shadow (default)
    namespace: example-console
    targetRefs: [{name: console}]
    oidc:
      clientID: example-console
      clientSecret: {name: example-console-client}
      hostname: console.example.com
      forwardAccessToken: true

  admin:                            # one group; CSRF enforced
    namespace: example-admin
    targetRefs: [{name: admin}]
    oidc:
      clientID: example-admin
      clientSecret: {name: example-admin-client}
      hostname: admin.example.com
      pathPrefix: /_auth
      cookieSameSite: Strict
    authorization:
      posture: groups
      groups: [example-admins]
    csrf:
      mode: enforce
      additionalOrigins: [https://portal.example.com]

  api:                              # a machine route: bearer tokens, no CSRF
    type: jwt
    namespace: example-api
    targetRefs: [{kind: GRPCRoute, name: api}]
    jwt:
      audiences: [example-api]
    authorization:
      posture: groups
      groups: [example-api-callers]

backendTLSPolicies:
  store:
    namespace: example-store
    targetRefs: [{name: store, sectionName: https}]
    hostname: store.example-store.svc
    caCertificateRefs: [{kind: ConfigMap, name: example-trust-bundle}]
```

Relational mistakes fail the render; [safety.md](safety.md#gateway-policies)
lists every refusal.

### Top level

| Value | Default | Notes |
|---|---|---|
| `commonAnnotations` | `{}` | on every rendered object; an object's own `annotations` win per key |
| `tlsBaseline` | on | see below |
| `tlsPolicies` | `{}` | one ClientTrafficPolicy per entry |
| `defaults` | see below | merged under every `securityPolicies` entry |
| `securityPolicies` | `{}` | one SecurityPolicy per entry |
| `backendTLSPolicies` | `{}` | one BackendTLSPolicy per entry |

### `tlsBaseline` — the floor for every Gateway

A ClientTrafficPolicy only attaches to Gateways in its own namespace, so
one is rendered per namespace. It selects Gateways by the **absence** of a
label, so a Gateway created later is covered from its first moment, and one
that needs something else opts out visibly by carrying the label.

| Value | Default | Notes |
|---|---|---|
| `tlsBaseline.enabled` | `true` | |
| `tlsBaseline.name` | `tls-baseline` | the same name in every namespace |
| `tlsBaseline.namespaces` | `[]` | empty: the release namespace only |
| `tlsBaseline.annotations`, `.labels` | `{}` | |
| `tlsBaseline.exemptLabel` | `tls-baseline-exempt` | a Gateway carrying this label key (any value) is not selected |
| `tlsBaseline.tls.minVersion` | `"1.2"` | `"1.0"`–`"1.3"` or `Auto`, quoted. RFC 8996 retired 1.0 and 1.1 |
| `tlsBaseline.tls.maxVersion` | `""` | empty renders nothing: Envoy's maximum (1.3) applies |
| `tlsBaseline.tls.ciphers` | the six ECDHE AEAD suites | TLS 1.2 and older only; not rendered when `minVersion` is `"1.3"` (the API refuses both). `[]` leaves the choice to Envoy. The default is Envoy's own list, stated so it is visible and does not move with a proxy upgrade |
| `tlsBaseline.tls.ecdhCurves` | `[]` | the key-exchange groups, for 1.3 as well as 1.2. `[]` leaves Envoy's own list (`X25519:P-256`), which **cannot complete a TLS 1.2 handshake for a P-384 certificate** — see safety.md. `X25519`, `P-256`, `P-384`, `P-521`; another name is refused |

A Gateway with its own Gateway-level ClientTrafficPolicy must carry the
opt-out label: see [safety.md](safety.md#one-gateway-one-gateway-level-clienttrafficpolicy).

### `tlsPolicies.<name>` — stricter TLS where it is needed

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | |
| `name` | the map key | |
| `namespace` | *required* | the Gateway's namespace |
| `annotations`, `labels` | `{}` | |
| `targetRefs` | *required* | `{kind, name, sectionName}`; `kind` `Gateway` (default) or `ListenerSet`; `sectionName` narrows it to one listener |
| `tls.minVersion` | `"1.3"` | |
| `tls.maxVersion` | `""` | |
| `tls.ciphers` | `[]` | as for the baseline |
| `tls.ecdhCurves` | `[]` | as for the baseline, and the one TLS value a 1.3 floor does not make redundant |

### `defaults` and `securityPolicies.<name>` — protected routes

`defaults` is merged under every entry, key by key; an entry's own value
replaces it, including an empty one (`""`, `false`, `[]`). So an issuer, a
namespace or a CSRF posture shared by many routes is stated once.

Entry-only keys:

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | |
| `type` | `oidc` | `oidc`: the gateway signs the browser in. `jwt`: every request carries a bearer token. `deny`: default-deny (`authorization.defaultAction: Deny`), no sign-in or token. `allow`: `defaultAction: Allow`, for a route or rule that is public on purpose. `deny` and `allow` take no `oidc`/`jwt` and need CSRF `off` (the default for them). A route-level policy replaces the listener-level one, so a `deny` on a ListenerSet refuses every route that has no policy of its own, and an `allow` on a route or one of its rules (`sectionName`) opens just that |
| `name` | the map key | |
| `targetRefs` | *required* | `{kind, name, sectionName}`; `kind` `HTTPRoute` (default), `GRPCRoute`, `Gateway` or `ListenerSet`. Two entries on one target are refused: the controller would apply the older and silently ignore the newer |
| `extraSpec` | `{}` | deep-merged over the generated spec, last, for fields this chart does not model |

Keys valid in both `defaults` and an entry:

| Value | Default | Notes |
|---|---|---|
| `namespace` | `""` | *required* (here or in the entry): the target's namespace |
| `annotations`, `labels` | `{}` | |

#### `oidc` — browser sign-in

The gateway runs the authorization code flow itself, refreshes the tokens
and keeps them in cookies it encrypts: no proxy, no session store, no
cookie key to manage.

| Value | Default | Notes |
|---|---|---|
| `oidc.issuer` | `""` | **required** on every oidc entry. Until it is set nothing signs in: an entry without one is refused, never rendered half-configured. `https://` only |
| `oidc.authorizationEndpoint`, `.tokenEndpoint` | `""` | written out instead of discovered, so the controller never has to reach the issuer to translate the policy; empty uses discovery |
| `oidc.endSessionEndpoint` | `""` | sign-out also ends the session at the issuer. Without it the cookie is cleared, the issuer session lives on, and the next page load signs the person straight back in |
| `oidc.backendSettings` | `{}` | how the gateway REACHES the issuer, as opposed to what it asks for: Envoy Gateway `BackendSettings` for the provider's upstream cluster, passed through verbatim. `circuitBreaker`, `connection`, `dns`, `healthCheck`, `http2`, `loadBalancer`, `proxyProtocol`, `retry`, `tcpKeepalive`, `timeout`; another block name is refused. Set in `defaults` it applies to every sign-in route, and an entry's blocks merge over it key by key. See [safety.md](safety.md#an-idle-connection-to-the-issuer-can-be-dead-without-saying-so) |
| `oidc.clientID` | `""` | *required* |
| `oidc.clientSecret.name` | `""` | *required*: an Opaque Secret **in the policy's namespace** with the key `client-secret`. Delivering it is the caller's job; the chart only references it (with `group` and `kind` written out) |
| `oidc.hostname` | `""` | the redirect becomes `https://<hostname><pathPrefix>/callback`. One of `hostname` or `redirectURL` is *required*: it must equal the redirect registered at the issuer |
| `oidc.pathPrefix` | `/oauth2` | the callback is `<pathPrefix>/callback`, sign-out `<pathPrefix>/logout`. Pick one the application never serves |
| `oidc.redirectURL` | derived | set in full to override |
| `oidc.logoutPath` | `<pathPrefix>/logout` | |
| `oidc.scopes` | `[openid, email, profile]` | `openid` is always sent. Most issuers need `offline_access` for a refresh token |
| `oidc.refreshToken` | `true` | refresh underneath the session instead of signing the person out when the access token expires |
| `oidc.forwardAccessToken` | `false` | send the access token upstream as `Authorization: Bearer` |
| `oidc.cookieSameSite` | `Lax` | `Lax`, `Strict`, `None` or `""` (renders no `cookieConfig`). Stated, not left to the browser, whose default differs by browser and version. `Lax` still sends the cookie on a top-level link into the application; `Strict` suits one nothing links into |
| `oidc.idToken.enabled` | `false` | verify the ID token as a JWT on every request. *Required* for the `groups` posture and for `claimToHeaders` |
| `oidc.idToken.header` | `x-id-token` | the header the ID token is handed over in and read from |
| `oidc.idToken.providerName` | `oidc` | the JWT provider's name; the authorization rule refers to it |
| `oidc.idToken.audiences` | `[clientID]` | an ID token is minted for exactly one client |
| `oidc.idToken.remoteJWKS.uri` | `""` | *required* when enabled |
| `oidc.idToken.remoteJWKS.cacheDuration` | `300s` | |
| `oidc.idToken.remoteJWKS.backendRefs` | `[]` | `{name, namespace, port}`: fetch the keys from an in-cluster Service instead of the public URL (`uri` still names the path) |
| `oidc.idToken.remoteJWKS.backendSettings` | `{}` | as `oidc.backendSettings`, for the connection that fetches the keys. Worth stating whenever the key URI and the token endpoint share a host: Envoy Gateway names a derived cluster after host and port alone, so both collapse onto ONE cluster and the first built wins — a keepalive given only to `oidc.backendSettings` is then dropped in silence. See [safety.md](safety.md#and-stating-it-in-one-place-may-not-be-enough) |
| `oidc.idToken.claimToHeaders` | `[]` | `{header, claim}`: copy a claim of the verified token into a request header. The header is ADDED, not overwritten: a copy the client sent under the same name stays first, and a backend that reads the first value sees the client's. Every header named here must also be in `gateway-fleet`'s `clientTraffic.earlyRequestHeaders.remove`, which strips the client's copy before any filter runs; the two charts cannot check each other, so the rule is yours to keep |

#### `jwt` — machine routes

| Value | Default | Notes |
|---|---|---|
| `jwt.providerName` | `jwt` | |
| `jwt.issuer` | `""` | **required** on every jwt entry; until it is set nothing validates |
| `jwt.audiences` | `[]` | **required**: without one, every token the issuer ever signed, for anything, is accepted |
| `jwt.remoteJWKS.uri` | `""` | *required* |
| `jwt.remoteJWKS.cacheDuration` | `300s` | |
| `jwt.remoteJWKS.backendRefs` | `[]` | as for `oidc.idToken` — the in-cluster issuer's Service |
| `jwt.remoteJWKS.backendSettings` | `{}` | as for `oidc.idToken` — how the gateway reaches the keys, and the same cluster-name collision to watch for |
| `jwt.extractFrom` | `{}` | `{}` is Envoy Gateway's default, `Authorization: Bearer`. Otherwise `{headers: [{name, valuePrefix}], cookies, params}` |
| `jwt.claimToHeaders` | `[]` | as for `oidc.idToken.claimToHeaders`: the header is added, so list it in `clientTraffic.earlyRequestHeaders.remove` of `gateway-fleet` |

#### `authorization` — who passes

| Value | Default | Notes |
|---|---|---|
| `authorization.posture` | `authenticated` | `authenticated`: anyone the issuer signed in or signed a token for. `groups`: `defaultAction: Deny` and one rule allowing a token whose `claim` holds one of `groups` |
| `authorization.claim` | `groups` | read from the verified token (the ID token on an oidc entry) as a string array |
| `authorization.groups` | `[]` | *required* for `groups`; an empty allow-list is a route nobody can use |
| `authorization.ruleName` | `allow-groups` | |

#### `csrf` — cross-site request forgery

The gateway compares the `Origin` (or, when absent, the `Referer`) of every
mutating request — `POST`, `PUT`, `PATCH`, `DELETE` — with the request's
own host. It is the second line after `SameSite`, not a replacement: it
catches a same-site attacker, an older browser and a cookie already sent.

| Value | Default | Notes |
|---|---|---|
| `csrf.mode` | `""` | `off`, `shadow`, `enforce`, or `""` for the type's default: **`shadow` on `oidc`**, `off` on `jwt`. On a `jwt` entry anything but `off` is refused: a machine client sends no `Origin`, so CSRF on an API or a webhook refuses every mutating request |
| `csrf.additionalOrigins` | `[]` | other origins allowed to post, e.g. `https://app.example.com` (Envoy compares host and port only). Refused with `off` |
| `csrf.shadowFraction` | `100/100` | shadow only: the share evaluated in shadow; the rest are enforced. Lower the numerator to roll enforcement out gradually |

How to move a route from shadow to enforce, the counters, and the
access-log fields that make every shadow decision reconstructable:
[safety.md](safety.md#csrf-shadow-then-enforce).

### `backendTLSPolicies.<name>` — private-chain backends

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | |
| `name` | the map key | |
| `namespace` | *required* | the Service's namespace |
| `annotations`, `labels` | `{}` | |
| `targetRefs` | *required* | `{kind, name, sectionName}`; `kind` `Service` (default), `sectionName` the Service port's name |
| `hostname` | *required* | sent as SNI, and the name the certificate must carry |
| `caCertificateRefs` | `[]` | `{kind: ConfigMap\|Secret, name}` with the CA under `ca.crt`. Exactly one of this or `wellKnownCACertificates` |
| `wellKnownCACertificates` | `""` | `System`: the proxy's own trust store |
| `subjectAltNames` | `[]` | `{type: Hostname, hostname}` or `{type: URI, uri}`, verified instead of `hostname` |

The proxy presents a client certificate to such a backend only if its
EnvoyProxy has one (`gateway-fleet`: `proxy.backendTLS.clientCertificateRef`).
`gateway-groups` can also render a BackendTLSPolicy per group
(`groups.<n>.backendTLS`); use this chart for a backend that is not tied
to one group, and never both for one Service.

## gateway-routes

A **library** chart: it renders nothing of its own and has no values. Its
input is the argument of the template the application's chart calls, and an
unknown key there fails the render exactly as `values.schema.json` does for
the other charts.

```yaml
# the application's Chart.yaml
dependencies:
  - name: gateway-routes
    version: 1.4.0
    repository: oci://ghcr.io/truvity/charts
```

```yaml
# the application's templates/httproute.yaml
{{ include "gateway-routes.productRoute" (dict "root" $ "route" .Values.route) }}
```

`root` is the calling chart's context (`$`), used for the release namespace
alone. `route` is everything below.

### Worked example

```yaml
route:
  name: example-shop
  hostnames:
    - shop.example
  parentRefs:
    - kind: ListenerSet
      name: example
      namespace: gateways
      sectionName: shop
  backend:
    name: example-shop-web
    port: 8080
  app:
    # The default. Everything no more specific rule claims — including
    # every endpoint added next year — belongs to the gated surface.
    paths: ["/"]
    extra:
      timeouts:
        request: 30s
  static:
    # Content-hashed file names. Anonymous, because no policy names this
    # rule; cacheable to the extent the origin's own Cache-Control says.
    paths:
      - /app/assets
      - /hub/assets
    filters:
      - type: ResponseHeaderModifier
        responseHeaderModifier:
          set:
            - name: cache-control
              value: public, max-age=31536000, immutable
  extraRules:
    # Another anonymous surface of the same application, on a backend of
    # its own: a redirect, a well-known document.
    - name: redirect
      paths: ["/r"]
      backend:
        name: example-shop-redirect
        port: 8080
```

One HTTPRoute, three named rules. The estate gates the first of them by
naming it:

```yaml
securityPolicies:
  example-shop:
    namespace: example-shop
    targetRefs:
      - name: example-shop
        sectionName: app
```

### Top level

| Value | Default | Notes |
|---|---|---|
| `enabled` | `true` | `false` renders nothing |
| `name` | *required* | the HTTPRoute's object name, a DNS subdomain |
| `namespace` | the release namespace | |
| `annotations`, `labels` | `{}` | |
| `hostnames` | *required* | at least one; one leading `*.` is allowed |
| `parentRefs` | *required* | at least one `{group, kind, name, namespace, sectionName, port}`. `group` defaults to `gateway.networking.k8s.io` and `kind` to `Gateway`; a ListenerSet parent states `kind: ListenerSet` |
| `backend` | `{}` | the backend every rule sends to unless it names its own: `{group, kind, name, port, weight}`, defaulting to a `Service` of the core group with `weight: 1`. `name` and `port` are *required* somewhere — here or in each rule |
| `app` | see below | the **gated** rule. Always rendered |
| `static` | `{}` | the **public** rule. Rendered only when it has paths |
| `extraRules` | `[]` | further public rules, each `{name, …}` as below |

### A rule — `app`, `static`, and each of `extraRules`

| Value | Default | Notes |
|---|---|---|
| `name` | — | `extraRules` only, *required*: unique, and neither `app` nor `static` |
| `paths` | `["/"]` on `app`, none on `static` | `PathPrefix` matches. Absolute; `/` is refused on a public rule, which would publish the whole application |
| `backend` | `route.backend` | overrides it for this rule |
| `filters` | `[]` | HTTPRoute filters, verbatim |
| `extra` | `{}` | deep-merged over the generated rule, last: `timeouts`, `retry`, `sessionPersistence` — anything this template does not model |

The rule names `app` and `static` are a contract, not a label: whatever
generates the policies targets `app` by that name (`sectionName: app` in a
`gateway-policies` `targetRefs` entry). Envoy matches the **most specific**
path prefix, so `static` wins over `app`'s `/` however the rules are
ordered.

## catalog (Go library)

`github.com/truvity/edge/v4/catalog` reads the file an estate keeps to say
which names it serves, on which way in, and from whose namespaces. The
`gateway-groups` values are derived from it by the estate's own generator;
the library is what every reader of the file shares.

```yaml
version: 1
exposures:
  public:
    class: edge
    arrival: cloudflare-tunnel      # or private-clusterip, nlb
    project_groups:                 # optional: generate the per-project groups
      overrides:
        <cluster>:
          <project>:
            group: <name>           # keep a live group name
            description: <text>
            names:                  # keep live listener and Secret names, by host
              <host>: {listener_name: <name>, secret_name: <name>, waf: off, waf_reason: <why>}
    clusters:
      <cluster>:
        groups:                     # the groups nobody generates: platform consoles, wildcards
          <name>:
            # waf: off + waf_reason leaves a tunnel-served name out of the zone's derived Cloudflare managed WAF
            domains: [<host>, {host: <host>, listener_name: <n>, secret_name: <n>, dns_elsewhere: true, waf: off, waf_reason: <why>}]
            route_namespaces: [<namespace>]
            listener_set: true
```

```go
c, err := catalog.Load(fsys)          // refuses unknown keys
err = c.WithProjects([]catalog.Project{{
    Name: "shop",                     // also the namespace its routes live in
    Endpoints: []catalog.Endpoint{{Environment: "dev", Hostname: "shop.dev.example.com"}}, // WAFOff: "reason" opts a hostname out of the managed WAF
}})
```

With `project_groups`, every project that has an endpoint on a cluster the
exposure runs on gets one group there, and an endpoint's `Environment` is the
cluster it is served from. Call `WithProjects` once, before anything reads the
groups; a catalog with `project_groups` is not validated until then.
`NamespacesGrant` and `NamespaceGrant` write a ListenerSet's `allowedRoutes`
by `kubernetes.io/metadata.name`, which nobody can relabel.

`Config.DeriveGroups(catalog.DeriveInput{...})` derives one cluster's
`listener_set` groups: the groups, the Gateways of the exposures it derives,
and each group's route grant. A business group admits its own namespaces by
name; a business wildcard admits by the coarse label the estate names in
`BusinessGrantLabel` (per-install hosts land on a wildcard, in namespaces the
catalog does not list), or, with `WildcardByName`, by name over its own
namespaces and the estate's `WildcardNamespaces` (for an estate that writes no
coarse label); a platform group admits the one namespace the estate's
`PlatformConsumers` map gives it; a private group naming one namespace admits
it by name. The private exposure is not derived: its groups attach to the
Gateway of `PrivateEntry` and take its certificate policy. The result's
`ListenerSetsFor` points each project at the ListenerSet that serves its
primary hostname.

`catalog.NewPrivateEntry` derives a cluster's shared private entry from the
object names the estate owns, its private zone, the pinned address and the
private trust domain's leaf policy; `WriteValues` writes the `privateEntry`
block (the health listener, the TLS floor and, when `LoadBalancer` is set,
the internal load balancer's annotations).

`catalog.ClientProxy` is the gateway's half of a console's sign-in row
(`attach_route_name`, `attach_route_rule`, `posture`, `allow`, `forward_bearer`,
`csrf`, `csrf_additional_origins`, `same_site`): an estate embeds it inline in
its own row, and `Validate(where, checkAllow)` applies the rules. `ForwardsBearer`
and `PostureOrDefault` carry the defaults, so no estate restates them.
