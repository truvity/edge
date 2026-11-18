# docs-mcp

`docs-mcp` serves a documentation site to agents over the Model Context
Protocol, read-only, from a static artifact. It has two halves:

- `docs-mcp pack` runs in CI. It turns a directory of Markdown into an **MCP
  pack**: a versioned directory with a section index and an inverted index for
  ranked search. Push it as an OCI artifact, next to the site's own bundle,
  with `ocictl static push` or `oras push`.
- `docs-mcp serve` runs in the cluster (`charts/docs-mcp`). It pulls the pack
  by digest, refuses a format version it does not read, and serves MCP over
  Streamable HTTP at `/mcp`. It never builds anything at start and holds no
  authentication code.

```
CI:       Markdown ──docs-mcp pack──► pack/ ──push──► registry: docs/pack@sha256:…
                                                         │ pull by digest (start)
cluster:  client ─► gateway ─► :8080 resource-proxy ─► 127.0.0.1:8081 docs-mcp
                               checks the token,        search_docs, list_docs,
                               audits, strips it        read_doc, get_llms_context
```

The site generator (docmd, Starlight, anything) is not part of this: `pack`
reads Markdown and, optionally, the `llms.txt` and `llms-full.txt` the site
publishes. A generator that is heavy to run runs in CI only, and its output is
what gets packed.

## `docs-mcp pack`

```
docs-mcp pack --in <dir> --out <dir> [--title <text>]
```

| Flag | Meaning |
|---|---|
| `--in` | Directory of Markdown: `.md`, `.markdown` and `.mdx`, at any depth. `llms.txt` and `llms-full.txt` at its top are carried along. Names starting with a dot, and `node_modules`, are skipped; a symbolic link is an error, so a pack cannot take in a file from outside the input. |
| `--out` | The pack directory. It must not exist or be empty, and must not lie inside `--in`. |
| `--title` | Optional name of the documentation set. It goes in the manifest and in the instructions the server gives its clients. |

The environment variable `SOURCE_DATE_EPOCH` (seconds, the reproducible-builds
convention) sets `createdAt` in the manifest. Without it the manifest records no
time.

**The output is byte-reproducible.** The same input gives the same pack, byte
for byte, wherever it is built: documents and sections are in sorted order,
JSON objects are written with sorted keys, no file time, mode, user or host
path is recorded, and the only time is the one `SOURCE_DATE_EPOCH` gives. So a
pack pushed twice has one digest, and a re-run after a half-failed release
finds the artifact already present.

`pack` ends by loading what it wrote with the reader's own checks, so a writer
that drifts from the reader fails in CI and not as a pod that cannot start; a
pack that fails is not left behind.

Exit status: 0 on success, 1 when the input cannot be packed (nothing to pack,
a symlink, a limit exceeded), 2 for a usage error.

## The pack format

The format is owned and versioned by this repository (`internal/docspack`).
This is **format version 1**. A reader refuses any other version before it
reads anything else of the pack: a change that an older reader could misread
increases the version, and one that only adds information does not.

```
manifest.json        the entry point (below)
index.json           the inverted index over the sections
docs/<route>         each Markdown file, as it was, at its route
llms.txt             optional, as the site publishes it
llms-full.txt        optional
```

### `manifest.json`

| Field | Meaning |
|---|---|
| `format` | `docs-mcp-pack` |
| `formatVersion` | `1` |
| `createdAt` | RFC 3339, only when `SOURCE_DATE_EPOCH` was set |
| `title` | the `--title`, when given |
| `tokenizer` | `unicode-alnum-lower-v1`: a term is a maximal run of letters and digits, lower-cased; a lone ASCII character is no term |
| `docs[]` | sorted by `route` |
| `index` | `{path, bytes, sha256}` of `index.json` |
| `llms`, `llmsFull` | `{path, bytes, sha256}` of the two optional files |

Each document of `docs[]`:

| Field | Meaning |
|---|---|
| `route` | path of the file relative to `--in`, forward slashes (`guides/install.md`): the name `read_doc` takes |
| `url` | path of the page the file builds to (`/guides/install/`) |
| `title` | the front matter `title`, else the first level-1 heading, else the file name |
| `path`, `bytes`, `sha256` | the file inside the pack (`docs/<route>`), its size and SHA-256; the reader checks both |
| `sections[]` | the section index, in document order |

Each section: `heading` (the path of heading texts from the top of the
document, `[]` for the text before the first heading), `anchor` (the slug of the
heading, an explicit `{#id}` when the heading has one, repeats numbered
`-1`, `-2`), `level` (1 to 6; 0 before the first heading), `start`, `end` and
`subtreeEnd` (byte offsets into the file as stored, front matter included:
`start` is the heading line, `end` the next heading of any level, `subtreeEnd`
the next heading of the same or a higher level), and `tokens` (the number of
terms, the length ranking normalizes by). Headings are ATX (`#`); headings in
code fences are not headings.

### `index.json`

`{formatVersion, tokenizer, sections, postings}`. `postings` maps each term to
`[section, frequency]` pairs, ascending by section number; sections are
numbered across all documents, in manifest order. Ranked search reads the
posting lists of the query's terms; it never scans documents. (The one text it
reads at query time is the section of each result, for the snippet.)

### Limits

A pack holds at most 20,000 documents (8 MiB each), 1,000,000 sections and, by
default, 64 MiB; the reader's byte limit is `DOCS_MCP_MAX_BYTES`
(`pack.maxBytes`). The reader checks every size and digest against the
manifest, refuses a route that is not a clean relative path, and reads files
through a rooted directory, so a manifest cannot point outside the pack.

## `docs-mcp serve`

Configured by environment, like `static-site`:

| Variable | Default | |
|---|---|---|
| `DOCS_MCP_ARTIFACT_REF` | | `registry/repository[:tag]` of the pack |
| `DOCS_MCP_ARTIFACT_DIGEST` | | `sha256:<64 hex>`. The pull is by digest: set this, or give the ref as `repository@sha256:…` |
| `DOCS_MCP_DIR` | | serve a pack directory and skip the registry (local use) |
| `DOCS_MCP_CONTENT_DIR` | `/var/run/pack` | where the artifact is unpacked; removed once the pack is in memory |
| `DOCS_MCP_ADDR` | `127.0.0.1:8081` | the MCP listener. A non-loopback address is refused unless `DOCS_MCP_ALLOW_NON_LOOPBACK=true`: the server does not authenticate |
| `DOCS_MCP_OPS_ADDR` | `:9090` | `/healthz`, `/readyz`, `/metrics`; nothing of MCP |
| `DOCS_MCP_ALLOWED_HOSTS` | any | comma list of `Host` headers the MCP listener answers |
| `DOCS_MCP_REGISTRY_AUTH`, `DOCKER_CONFIG`, `DOCS_MCP_PLAIN_HTTP`, `DOCS_MCP_PULL_TIMEOUT` | | as for [static-site](static-site.md#registry-authentication): the same puller |
| `DOCS_MCP_MAX_BYTES` | 64 MiB | cap on the pack |
| `DOCS_MCP_MAX_RESPONSE_BYTES` | 128 KiB | cap on the text of one tool result (at least 1024) |
| `DOCS_MCP_MAX_CONCURRENT_CALLS` | 16 | tool calls running at once; the others wait their turn. With the response cap it bounds the memory a burst of calls can hold |
| `DOCS_MCP_LOG_LEVEL`, `DOCS_MCP_SHUTDOWN_DELAY` | `info`, `5s` | |

The pull is the one of `cmd/static-site` (`internal/staticsite.Pull`): an
image manifest whose layers are a tar (gzip or not), or single files named by
their title annotation, verified against their digests, with Docker-config,
ECR-SDK or anonymous registry authentication. `docs-mcp pack` writes the
directory; push it as `oras push` does a directory.

The ops port comes up first, so liveness works during the pull. `/readyz` turns
true only once the pack is loaded and checked, and the MCP port opens then. A
pack with an unknown format version, a size or digest that does not match its
manifest, or a bad route stops the start with an error that names it.

### The tools

The tool names and arguments are those of `docmd mcp`, so an agent that knows
one knows the other. The text of a pack is data written by the site's authors,
and the server's instructions tell the client so.

| Tool | Arguments | Returns |
|---|---|---|
| `search_docs` | `query`; optional `limit` (10, at most 50), `subdir` | the best sections, ranked: route, heading path, anchor, score and a snippet |
| `list_docs` | optional `subdir`, `offset` | every route with its title |
| `read_doc` | `route`; optional `section` (an anchor), `offset` | the raw Markdown of the document, or of the section with its subsections |
| `get_llms_context` | optional `offset` | `llms-full.txt`, else `llms.txt`, else an outline of the documents |

Ranking is BM25 over the sections, with a boost for query terms in the
section's heading and a smaller one for those in the headings above it, a
bonus for sections that hold every term, and prefix matching ("install" finds
"installer"). Ties break by document and section order, so the same query
gives the same answer. A result longer than `DOCS_MCP_MAX_RESPONSE_BYTES` is
cut at a character boundary and says where to continue with `offset`. An
unknown route or anchor is a tool error that lists what exists. A route
resolves only through the pack's own table: `../` and absolute paths find
nothing.

The server is stateless: no session is kept, and every request stands alone.

Metrics on the ops port: `docs_mcp_requests_total`,
`docs_mcp_request_duration_seconds`, `docs_mcp_tool_calls_total{tool,result}`,
`docs_mcp_pack_loads_total{result}`, `docs_mcp_pack_info{digest}`,
`docs_mcp_pack_loaded_timestamp_seconds`, `docs_mcp_pack_documents`,
`docs_mcp_pack_sections`, and the Go and process collectors.

## Running `pack` in CI

Release assets, for a CI that cannot run a container: every release carries
`docs-mcp_<version>_linux_amd64.tar.gz` and `docs-mcp_<version>_linux_arm64.tar.gz`
(the binary and the licence) and one checksums file,
`edge_<version>_checksums.txt` (SHA-256, one line per archive of the release).
Download by version and check the checksum. The checksum file comes from the
same release as the archive, so it protects against a damaged download, not
against a tampered release: to guard against that, pin the SHA-256 of the
archive in your own repository, where a change is a reviewed commit.

```sh
v=4.10.0   # the release you pin
curl -fsSLO "https://github.com/truvity/edge/releases/download/v$v/docs-mcp_${v}_linux_amd64.tar.gz"
curl -fsSLO "https://github.com/truvity/edge/releases/download/v$v/edge_${v}_checksums.txt"
grep " docs-mcp_${v}_linux_amd64.tar.gz\$" "edge_${v}_checksums.txt" | sha256sum -c -
tar -xzf "docs-mcp_${v}_linux_amd64.tar.gz" docs-mcp
SOURCE_DATE_EPOCH="$(git log -1 --format=%ct)" ./docs-mcp pack --in site-md --out pack --title "Example Docs"
```

The same binary is the image `ghcr.io/truvity/edge/docs-mcp:<version>`
(distroless, non-root, linux/amd64 and linux/arm64; entrypoint `docs-mcp`):
`docker run --rm -v "$PWD":/w ghcr.io/truvity/edge/docs-mcp:<version> pack --in /w/site-md --out /w/pack`.

## The chart

`charts/docs-mcp` renders one Deployment with two containers:

| Container | Listens | |
|---|---|---|
| `docs-mcp` | `127.0.0.1:8081` (MCP, no container port) and `:9090` (container port `ops`: probes, metrics) | the server above |
| `proxy` | `:8080` (container port `http`), the pod's only port that accepts MCP | the [sluis `resource-proxy`](https://github.com/truvity/sluis/blob/master/docs/guides/sluis/connect/mcp-resource-proxy.md): checks the bearer token (signature against the issuer's keys, `iss`, and `aud` equal to `resourceURL`), serves the RFC 9728 protected-resource metadata, writes one audit line per request, strips the caller's `Authorization` header and forwards to `docs-mcp` on loopback |

`docs-mcp` has no backend, so the proxy has no outbound side and nothing is
exchanged. The Service has one port, `http`, which targets the proxy. Objects:
Deployment (rolling update with `maxUnavailable: 0`, soft spread over zones and
nodes), Service, ServiceAccount (unless the identity is `static-site-infra`'s),
NetworkPolicy, PodDisruptionBudget, and optionally a PodMonitor, an HTTPRoute
and the identity check hook. Labels: `app.kubernetes.io/name: docs-mcp` (or
`nameOverride`) and `app.kubernetes.io/instance: <release>`.

The pod meets the Pod Security Standard *restricted*: non-root with a numeric
uid, `RuntimeDefault` seccomp, no privilege escalation, every capability
dropped, a read-only root filesystem.

### Values a consumer sets

| Value | |
|---|---|
| `issuerURL` | **required.** The access-token issuer: its `iss`, and where its keys are |
| `resourceURL` | **required.** The server's resource identifier and the token audience; https, with a path (for example `https://docs.example.com/mcp`) |
| `proxy.image.tag`, `proxy.image.digest` | **required.** The sidecar release, pinned by tag and digest; `proxy.image.repository` defaults to `ghcr.io/truvity/sluis/resource-proxy` |
| `pack.artifact.ref`, `pack.artifact.digest` | **required.** The pack's registry reference without a digest, and its `sha256:` digest |
| `networkPolicy.ingressFrom` | **required** while the policy is on (the default): the gateway's peers |
| `networkPolicy.extraEgress` | extra egress rules, verbatim; with Pod Identity, `169.254.170.23/32` tcp 80 |
| `registry.auth`, `registry.ack.enabled` | `ecr` and `true` for an ECR registry with the identity of `static-site-infra` |
| `serviceAccount.name` | with the ACK identity, the account `static-site-infra` renders |
| `fullnameOverride`, `replicaCount`, `podDisruptionBudget` | names and availability (the budget needs two replicas) |
| `httpRoute.*` | optional route, below |

The chart refuses, at render time, what would otherwise render cleanly and fail
later: an empty issuer, resource URL, sidecar tag or digest, or pack digest; a
resource URL with no path; a NetworkPolicy that admits nobody; a budget with one
replica; a route with no parent. `values.schema.json` rejects unknown keys,
and every refusal has a fixture under `tests/invalid/docs-mcp/`.

### Cloud identity: reuse `static-site-infra`

The pack is pulled with the same ECR permissions a static site needs
(`ecr:GetAuthorizationToken`, and the three read actions on the repository), so
**the identity is `static-site-infra`'s, unchanged**; `docs-mcp` renders no ACK
object and no ServiceAccount of its own with `registry.ack.enabled`. Install
`static-site-infra` with `installName` and `serviceAccount.name` set to the
docs-mcp release's name and the `registry.ack` keys of the repository the pack
is in; give this chart the same `serviceAccount.name`, `registry.auth: ecr` and
`registry.ack.enabled: true` (and `registry.ack.accountId`, to have the hook
also check the account). A pre-install hook then proves that a pod admitted now
has the identity, before the Deployment exists. The labels `static-site-infra`
puts on the ServiceAccount say `static-site`; that is cosmetic. See
[static-site.md](static-site.md#ecr-on-eks-with-ack-two-charts).

### Network

Ingress: the proxy's port from `networkPolicy.ingressFrom`; the ops port only
from `networkPolicy.metricsFrom` (the kubelet's probes are not subject to a
NetworkPolicy). Egress: DNS (port 53 to any destination, or to
`networkPolicy.egress.dns`), HTTPS to `networkPolicy.egress.https` or any
destination (443, plus the port of `issuerURL` or of the registry host when
they name another), and `networkPolicy.extraEgress`. **HTTPS and DNS egress are open by default** (port 443 and port 53 to any
destination), because a registry or an issuer behind a CDN or a cloud endpoint
has no address a chart can know; name the peers in `egress.https` and
`egress.dns` to close them. The server's MCP listener is on loopback, so
nothing outside the pod can reach it whatever the policy.

### Identity and memory

The pod mounts no ServiceAccount token (`automountServiceAccountToken: false`,
always): Pod Identity and IRSA project the token they need themselves. Both
containers share the pod's network and identity, so with `registry.auth: ecr`
the role that may pull the pack is available to the sidecar too: keep that role
to the pull.

The pack is unpacked into a memory-backed volume (counted against the
container's memory), then loaded, then the unpacked copy is removed. At the
peak the volume, the files in memory and the index being built are held at
once: set the memory limit to at least three times `pack.maxBytes`. The
defaults (64 MiB and 256Mi) do.

### Routing

`httpRoute.enabled` renders one HTTPRoute in the release namespace, to the
Service, from the path of `resourceURL` (here `/mcp`): `PathPrefix /mcp`,
`PathPrefix /.well-known/oauth-protected-resource/mcp` and
`Exact /.well-known/oauth-protected-resource`, each with `timeouts.request: 0s`
(MCP may hold a response open) and no filters. `httpRoute.parentRefs` may name a
ListenerSet. Who may call is the issuer's policy for `resourceURL`.

## Release

A tag releases, at one version: the chart `oci://ghcr.io/truvity/charts/docs-mcp`,
the image `ghcr.io/truvity/edge/docs-mcp:<version>` (also `latest`), and the two
binary archives with the checksums file on the GitHub Release.
