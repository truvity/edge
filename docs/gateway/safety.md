# Safety — what can break, and what the charts do about it

The first three sections are `gateway-fleet` and `gateway-groups`;
[gateway-policies](#gateway-policies) and [gateway-routes](#gateway-routes)
have their own below.

The mistakes that take an edge down are relational: two things claiming
one name, one hostname or one Secret, a grant that grants everything, a
proxy that is silently dropped. None of them is visible in a single
object, and most are not visible until they are live. So the charts
check them at render time, and every check has a fixture under
`tests/invalid/<chart>/` that must fail — `just lint` renders each one and
refuses a fixture that renders.

## Refused at render time

| Refusal | What it prevents |
|---|---|
| two exposures, or two groups, claiming one Gateway, Service, ListenerSet, Certificate or Secret name | the second object silently overwriting the first; the diff looks like an edit |
| two groups claiming one hostname on one exposure and port | a listener conflict that takes **both** groups down, invisible until it is live |
| a per-exposure `proxy` on a merged class | the settings being silently ignored: a merged class has one proxy for every exposure |
| a merged class with its proxy disabled | a class with no proxy at all |
| an exposure naming a class that does not exist or is disabled | a Gateway no controller will ever program |
| a wildcard `health.hostname` | the health listener shadowing a group's listener |
| a missing `health.hostname` | a Gateway with no listener, which has no proxies |
| a `Selector` grant with an empty selector (`allowedListeners`, `allowedRoutes`) | an empty selector matches every namespace — the opposite of a grant |
| a `directResponse` health route the exposure's own namespace may not attach | a health endpoint that never answers |
| a `clusterIP` that is not an IPv4 address or `None` | a Service patch the API server rejects after the rest has applied |
| a certificate with no `issuerRef.name` | a chart that picks an issuer; the issuer is always the estate's |
| a group with no parent, or no domains | a ListenerSet that attaches to nothing, or serves nothing |
| a domain that is not a hostname | a listener the controller rejects |
| `backendTLS` enabled with no `targetRefs` or no `validation` | a policy that re-encrypts nothing, or TLS that is never verified |
| a `networkPolicy` with no namespaces, no ports, no source namespace or no source pod labels | an allow-everything rule written by omission |
| on a shared host (`domains[].paths`): a malformed prefix, prefixes that nest, a namespace with two prefixes or none, a grant `allowedRoutes` does not admit by name, `paths` on some domains of a group and not others, or on a wildcard | a tenant on a shared host answering for another's prefix, or for the whole host |
| a group route on a shared host outside its namespace's prefix, with a match that names no path, or with a `RegularExpression` path | the same, from a route the platform renders itself |
| any unknown key (`values.schema.json`) | a typo read as "use the default" |

## Defaults chosen because the other one failed

**The exposure's own listener is a health endpoint.** A Gateway with no
listener has no proxies. Without the health listener an exposure is dark
until the first group attaches, and goes dark again when the last one
leaves — which is exactly when nobody is looking.

**`allowedListeners` admits only the exposure's own namespace.** A
ListenerSet is a claim on hostnames. If a project could create one in a
namespace it controls and have it attach, the grant would be whatever the
project chose.

**One Certificate per domain, never one with every domain as a SAN.** A
group's domains are added, renamed and retired one at a time. A SAN list
has to be reissued as a whole for each change, and a failed reissue takes
every domain on it down together.

**A split class by default.** With `mergeGateways: false` every exposure
has its own Deployment, Service and address, so a private exposure does not
share a failure domain, a load balancer or a pinned address with a public
one.

## Traps worth knowing

### Adding one access-log field does not drop the others

A JSON access-log format in `EnvoyProxy.spec.telemetry` **replaces** the
controller's default line rather than extending it, so adding one field by
hand silently drops the other two dozen. `proxy.accessLog.extraFields`
therefore renders the controller's default fields too and merges the
extra ones over them. The defaults are copied from the controller: re-check
them when upgrading the controller. With no extra fields nothing is
rendered and the controller's own default stays in charge.

Log URL-valued headers with `%REQ_WITHOUT_QUERY(...)%`, or a Referer puts
its query string — tokens included — in the log.

### A pinned address goes through the controller's patch hook

The controller owns the proxy's Service, so a `clusterIP` or load-balancer
field set on it directly is reverted. The chart sets them through the
EnvoyProxy's service patch, which the controller applies on every
reconcile.

### Drain before the balancer notices

A load balancer keeps sending to an endpoint until its own health check
fails. `shutdown.healthCheckFailureDelay` fails the proxy's readiness that
long **before** it starts draining, so the endpoint is removed first and
in-flight connections are not cut.

### A P-384 certificate cannot finish a TLS 1.2 handshake by default

Envoy's key-exchange group list is `X25519:P-256`. Under TLS 1.2 the
server's signature is made on a group from that list, so a **P-384
certificate cannot complete a 1.2 handshake at all** — while every TLS 1.3
client works, because 1.3 separates the signature from the key exchange.

The failure is at the handshake, before any policy or route is consulted,
and nothing in the objects says so: the certificate is valid, the listener
is Ready, the floor is applied, and one class of client simply cannot
connect. Raising `minVersion` does not help, and neither does the cipher
list — the ciphers name the bulk encryption, not the curve.

Set `tls.ecdhCurves` where the certificates are P-384:

```yaml
tlsBaseline:
  tls:
    ecdhCurves: [X25519, P-384]   # keep the fast path, admit P-384
```

`[P-384]` requires it instead. The chart refuses a name Envoy does not
know, because a typo there is not a weaker TLS floor — it is a policy the
proxy rejects whole, so the floor stops applying and nothing says why.

## gateway-policies

A security policy fails in two directions, and both are quiet: a route
that should be protected and is not, and a route that refuses what it
should serve. Neither shows in the object; both show only in traffic.

### Refused at render time

| Refusal | What it prevents |
|---|---|
| an `oidc` entry without an issuer, client id, client Secret name, or a `hostname`/`redirectURL` | a policy rendered half-configured; sign-in is **off until given an issuer**, never guessed |
| an `http://` issuer, endpoint or redirect (schema) | tokens and codes readable by anyone on the path |
| a `jwt` entry without an issuer, an audience or a JWKS URI | a machine route that accepts every token the issuer ever signed, for anything |
| CSRF `shadow` or `enforce` on a `jwt` entry | refusing every mutating request of a machine client, which sends no `Origin` |
| a `deny` or `allow` entry with CSRF on, or the `groups` posture | a policy with no cookie for CSRF to protect, or a group rule it never renders (their `targetRefs` must also be non-empty, and a target is claimed once like any other) |
| `additionalOrigins` with CSRF `off`, a shadow fraction above one, an origin that is not an origin (schema) | a setting nobody reads, or one the API server rejects after the rest has applied |
| the `groups` posture with no groups, or on an `oidc` entry without `idToken.enabled` | a route nobody can use, or a rule with no verified claim to read (the API server refuses a JWT principal with no JWT provider) |
| `idToken.enabled` without a JWKS URI | an ID token that cannot be verified |
| two entries targeting one route or listener | the controller applies the **older** policy and marks the newer Conflicted: the one just written silently does nothing |
| two objects of one kind with one namespace and name — across `tlsBaseline` namespaces, `tlsPolicies`, `securityPolicies`, `backendTLSPolicies` | the second overwriting the first |
| a `tlsPolicies` entry on a route, or with no target | TLS policy where TLS is not terminated |
| a TLS floor above its ceiling; an unquoted version (`1.2`, a number); an unknown cipher (schema) | a listener no client can reach, or a version the API server reads as something else |
| an empty `tlsBaseline.exemptLabel` | a baseline no Gateway can opt out of |
| a `backendTLSPolicies` entry with no target, no hostname, neither or both of `caCertificateRefs` and `wellKnownCACertificates`, or a CA ref that is not a ConfigMap or Secret | TLS to a backend that is never verified, or verified against nothing |
| an `oidc.backendSettings` block Envoy Gateway does not define (schema) | a name the API server prunes away while applying the rest: an Accepted policy missing exactly the setting that was added to stop a silent failure |
| any unknown key (`values.schema.json`) | a misspelt security setting read as "use the default" |

### Defaults chosen because the other one failed

**The TLS floor is on, and selected by the absence of a label.** Without
a ClientTrafficPolicy Envoy negotiates whatever the client offers,
including versions RFC 8996 retired. A floor attached by name covers only
the Gateways someone remembered; one that selects every Gateway without
the opt-out label covers the next Gateway from its first moment, and the
exception is written on the exception.

**The cipher list is stated.** The default is Envoy's own ECDHE AEAD
list, written out so it is visible on the cluster and does not move when a
proxy upgrade changes Envoy's default.

**CSRF is `shadow` on a sign-in route and refused on a machine route.**
`SameSite` stops most cross-site posts, but not a same-site attacker, an
older browser or a cookie already sent; the Origin check does. Turned on
blind, it refuses every client that sends no `Origin` — so it starts in
shadow, where it counts and refuses nothing.

**`SameSite` is `Lax`, and always written.** An unset attribute means
different things in different browsers and versions: a policy nobody wrote
and nobody can read off the cluster. `Lax` still sends the cookie on a
top-level link into the application, which is how people arrive.

**Sign-out ends the session at the issuer when `endSessionEndpoint` is
set.** Clearing only the gateway's cookie leaves the issuer session alive,
and the next page load signs the person straight back in — which everyone
reads as "sign-out is broken".

**Every field the API server would default is written out** (a target's
`group`, a Secret reference's `group` and `kind`, a JWKS backend's `kind`):
a GitOps controller otherwise diffs its render against the server's
defaults forever.

### CSRF: shadow, then enforce

Shadow evaluates every mutating request, counts the verdict and lets it
through. Enforce answers a failing one with `403 Invalid origin`. Move a route to `enforce` once the counters and the logs
below show that everything it would refuse is something it should.

The counters are Envoy's CSRF filter statistics, one set per listener
(`http.<connection manager prefix>.csrf.*`; in Prometheus form
`envoy_http_csrf_*` with the prefix as the `envoy_http_conn_manager_prefix`
label). They count identically in shadow and in enforce:

| Counter | Counts |
|---|---|
| `request_valid` | mutating requests whose source origin matched |
| `request_invalid` | mutating requests that would be (shadow) or were (enforce) refused |
| `missing_source_origin` | the subset of those with neither `Origin` nor `Referer` — typically a script, a curl or a webhook, not a browser |

A counter says *how many*, per listener, and on a shared listener many
routes feed one counter. To say *which* request, add these fields to the
proxy's access log with `gateway-fleet` (v1.1.0 and newer,
`proxy.accessLog.extraFields`; see
[below](#adding-one-access-log-field-does-not-drop-the-others)):

```yaml
accessLog:
  extraFields:
    origin: "%REQ(ORIGIN)%"
    referer: "%REQ_WITHOUT_QUERY(REFERER)%"   # never log the query: it can carry a code
    sec-fetch-site: "%REQ(SEC-FETCH-SITE)%"
    sec-fetch-mode: "%REQ(SEC-FETCH-MODE)%"
```

With the default fields (`method`, `:authority`, `route_name`,
`user-agent`, `response_code`, `response_code_details`) every shadow
decision can be recomputed from one line: a mutating `method` on a route
with the policy, whose `origin` — or, when empty, `referer` — does not
match `:authority` or an additional origin, is one the filter would refuse.
Empty on both is `missing_source_origin`. `sec-fetch-*` are set by browsers
and nothing else, so they separate a real cross-site form post from a
script. Once enforced, a refusal is logged with `response_code` 403 and the
filter's detail in `response_code_details`.

### One Gateway, one Gateway-level ClientTrafficPolicy

A Gateway that already has its own Gateway-level ClientTrafficPolicy (for example `gateway-fleet`'s `exposures.<n>.clientTrafficPolicy`) must
carry the baseline's opt-out label (`gateway-fleet`: `exposures.<n>.labels`). Two policies at the same level
are a conflict the controller resolves by age, not by intent. A
listener-level policy (`tlsPolicies` with a `sectionName`) is more specific
and wins for its listener without an opt-out.

### A claim header the client can also send

`claimToHeaders` ADDS the header; it does not overwrite one the request
already carries. A client that sends the same header leaves its own value
first, and a backend that reads the first value is told whatever the client
chose. Name every such header in `gateway-fleet`'s
`clientTraffic.earlyRequestHeaders.remove`: it is removed from the request
before any filter, so the only copy left is the one the gateway sets from
the verified token. A listener-level policy replaces the Gateway-level one
for its listener, so keep claim headers off listeners that carry one.

### `mode: off` is not the string "off"

In YAML 1.1, which Helm reads, an unquoted `off` (like `no`, `yes`, `on`)
is a boolean. `csrf.mode: off` would reach the chart as `false`; the
schema refuses it rather than read it as unset and fall back to shadow.
Quote it: `mode: "off"`.

### An idle connection to the issuer can be dead without saying so

The gateway calls the issuer itself — once per sign-in and once per token
refresh — over pooled upstream connections it keeps between calls. If that
path crosses a NAT gateway, a stateful firewall or a load balancer, the
device holds the flow's state only while the flow is used: most drop an
idle one after a few minutes, and many drop it **without sending a reset**
to either end. Nothing tells the proxy. The connection stays in the pool,
the pool believes it is healthy, and the next request written to it is
answered by nobody.

What that looks like is worth recognising, because none of it points at the
network:

- sign-in completes and then the session is gone — the code was exchanged
  on a good connection, the refresh was not;
- the failure is **intermittent and unrelated to load**, since it depends
  only on how long the pool sat idle;
- the proxy's own counters for the issuer's cluster show
  `upstream_rq_timeout` rising with `upstream_cx_connect_fail` at **zero**,
  and `upstream_cx_destroy_local_with_active_rq` tracking the timeouts:
  new connections are fine, old ones are graves;
- the successful requests are fast, so a latency percentile reads healthy.

`oidc.backendSettings` is where to answer it. The estate's numbers are its
own — read the idle timeout of whatever sits on the path — but the shape is
always the same three:

```yaml
defaults:
  oidc:
    backendSettings:
      tcpKeepalive:
        idleTime: 60s        # below the device's idle timeout
      retry:
        numRetries: 2
        retryOn:
          triggers: [reset, connect-failure]
      timeout:
        http:
          requestTimeout: 10s
```

The keepalive stops the flow going idle long enough to be forgotten; the
retry spends an attempt, rather than someone's session, on a connection
that was dropped anyway; the request timeout decides how long a request on
a dead one waits before the retry can happen, so a long one hides the fix.

None of this is a default. A keepalive interval is a property of the path,
not of the chart, and a retry on a token exchange is a decision the estate
makes — so an install that sets nothing renders exactly as before.

### ...and stating it in one place may not be enough

`oidc.backendSettings` can be set, Accepted, visible in the rendered
policy, and still absent from the proxy. The reason is naming.

Envoy Gateway derives an upstream cluster from each URL the gateway has to
call — the token endpoint, the JWKS URI — and names that cluster after the
**host and port** alone. When the issuer serves its keys from the host it
serves tokens from, which is the ordinary arrangement, both sides resolve
to the same cluster name. Only one cluster is built: the translator keeps
the first one and returns early for the second, on the assumption that two
requests for the same name ask for the same thing. Settings on the side
that lost are dropped, and nothing reports it — the policy's status says
`Accepted`, and the rendered YAML still shows the block that was ignored.

What that looks like: the filter-scoped parts of `backendSettings` —
`retry` and `timeout` — take effect, because they live in the OIDC filter's
own configuration rather than the cluster, while `tcpKeepalive` and the
other cluster-scoped blocks silently do not. In a config dump the issuer's
cluster carries an empty `upstream_connection_options`, and its
`last_updated` never moves when the policy changes.

The fix is not to pick a winner but to make the race not matter: state the
same settings on `oidc.idToken.remoteJWKS.backendSettings` (and on
`jwt.remoteJWKS.backendSettings`, for a machine route against the same
host) as on `oidc.backendSettings`. A YAML anchor keeps them honest:

```yaml
defaults:
  oidc:
    backendSettings: &issuerPath
      tcpKeepalive:
        idleTime: 60s
    idToken:
      remoteJWKS:
        uri: https://id.example.com/keys
        backendSettings: *issuerPath
```

Giving the JWKS side `backendRefs` — fetching the keys from an in-cluster
Service — also separates the two, because a cluster built from a backend
reference is named for that backend and never collides. Either answer
works; what does not work is assuming the token endpoint speaks for the
whole host.

## gateway-routes

A route whose rules are named fails in one direction that matters: the
gate lands on a rule nobody serves, or on no rule at all, and the
application answers — signed in or not — exactly as it would have. Nothing
in the route says so.

### Refused at render time

| Refusal | What it prevents |
|---|---|
| `static` or an `extraRules` path of `/` | a rule that carries no policy serving the whole application, shell and API included, to anyone: the one mistake this template exists to make impossible |
| a path that is not absolute; a rule with no paths | a match the API server rejects, or a rule that matches nothing |
| `static` with a backend or a filter but no paths | the rule is not rendered at all: a public surface someone believes exists |
| an `extraRules` entry named `app` or `static`, or a name used twice | a second rule under the name a policy targets — the gate then applies to one of them |
| an `extraRules` entry with no name, or a name that is not a section name | a rule no policy can target, and one that cannot be told apart in a status condition |
| no `name`, a `name` that is not a DNS subdomain | an object the API server rejects |
| no `hostnames`, or one that is not a hostname | a route that takes everything its parent serves |
| no `parentRefs`, or one with no name | a route attached to nothing |
| a rule with no backend name, or a port outside 1–65535 | a rule that routes nowhere |
| any unknown key, at the route, a rule, a backend or a parent | a library template has no `values.schema.json`: a misspelt `hostnames` or `static` would otherwise render a route that quietly serves something else |

### Traps worth knowing

#### A `sectionName` that matches no rule takes the gate off

The policy is not applied and **the route keeps serving**. The controller
says so — `Accepted: False`, `reason: TargetNotFound`, "No section name
*x* found for HTTPRoute *ns/name*" — and traffic says nothing: the request
that should have been redirected to the issuer reaches the application
instead. So the rule names here are a contract rather than a label, and
renaming one is a breaking change for whoever writes the policies.

Two consequences for anyone changing this chart or a caller's values:

- `app` and `static` are fixed. A caller cannot rename them, and an
  `extraRules` entry cannot take either name.
- After a change to a route's rules, read the policy's status, not the
  route's: an accepted route with an unaccepted policy is an open door.

#### The most specific prefix wins, not the first rule

Envoy ranks path prefixes by length, so `static`'s `/app/assets` wins over
`app`'s `/` however the rules are ordered, and a rule added later cannot
steal traffic from a more specific one. Ordering the rules, or giving them
priorities, is therefore not a thing to get right — but two rules with the
*same* prefix are, and that is what the duplicate-name and path refusals
above are for.

#### One route, not two

The same split can be written as two HTTPRoutes, one targeted by the
policy and one not. It works, and it puts the security-relevant half of
the arrangement in two objects that nothing ties together: a rename of the
untargeted route is invisible to the policy, and a reader of either one
cannot tell which surface is gated. Named rules keep the pair in one
object, where the diff shows both halves at once.
