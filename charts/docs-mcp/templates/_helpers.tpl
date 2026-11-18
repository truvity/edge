{{- define "docs-mcp.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "docs-mcp.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "docs-mcp.serviceAccountName" -}}
{{- default (include "docs-mcp.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/* The selector is immutable on a live Deployment: identity only. */}}
{{- define "docs-mcp.selectorLabels" -}}
app.kubernetes.io/name: {{ include "docs-mcp.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "docs-mcp.labels" -}}
{{ include "docs-mcp.selectorLabels" . }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "docs-mcp.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "docs-mcp.proxyImage" -}}
{{- $i := .Values.proxy.image -}}
{{- printf "%s:%s@%s" $i.repository $i.tag $i.digest -}}
{{- end -}}

{{/*
The fixed addresses inside the pod.

  proxy inbound   :8080           the pod's only MCP port
  docs-mcp MCP    127.0.0.1:8081  loopback, no container port
  docs-mcp ops    :9090           /healthz, /readyz, /metrics; nothing of MCP
*/}}
{{- define "docs-mcp.proxyPort" -}}8080{{- end -}}
{{- define "docs-mcp.mcpListen" -}}127.0.0.1:8081{{- end -}}
{{- define "docs-mcp.opsPort" -}}9090{{- end -}}

{{/*
The port of a URL or of a registry reference's host, from the scheme when it
names none (a reference has no scheme: 443).
*/}}
{{- define "docs-mcp.urlPort" -}}
{{- $u := urlParse . -}}
{{- $p := regexFind ":[0-9]+$" $u.host | trimPrefix ":" -}}
{{- default (ternary "80" "443" (eq $u.scheme "http")) $p -}}
{{- end -}}

{{- define "docs-mcp.registryPort" -}}
{{- $host := regexReplaceAll "/.*$" .Values.pack.artifact.ref "" -}}
{{- default "443" (regexFind ":[0-9]+$" $host | trimPrefix ":") -}}
{{- end -}}

{{/* The host (no port) of resourceURL. */}}
{{- define "docs-mcp.resourceHost" -}}
{{- regexReplaceAll ":[0-9]+$" (urlParse .Values.resourceURL).host "" -}}
{{- end -}}

{{/* The path of resourceURL, no trailing slash. */}}
{{- define "docs-mcp.resourcePath" -}}
{{- (urlParse .Values.resourceURL).path | trimSuffix "/" -}}
{{- end -}}

{{/*
"true" when the pod's cloud identity is managed by the static-site-infra chart:
it renders the ACK objects and the ServiceAccount, this chart only names the
account and checks, in a hook, that a pod admitted now has the identity.
Derived, never a switch: registry.auth=ecr with registry.ack.enabled.
*/}}
{{- define "docs-mcp.identityManaged" -}}
{{- if and (eq .Values.registry.auth "ecr") .Values.registry.ack.enabled -}}true{{- end -}}
{{- end -}}

{{/*
Every refusal in this chart. They guard a value that is EMPTY (or wrong) by
default and whose absence renders cleanly and fails later, somewhere else.
*/}}
{{- define "docs-mcp.validate" -}}
{{- if not .Values.issuerURL -}}
{{- fail "docs-mcp: `issuerURL` is empty. The proxy validates every caller's access token against the issuer's signing keys and `iss`; without it there is nothing to validate against, and the pod would be an unauthenticated door to the documentation. Set it to the access-token issuer's URL." -}}
{{- end -}}
{{- if not .Values.resourceURL -}}
{{- fail "docs-mcp: `resourceURL` is empty. It is the server's RFC 8707 resource identifier: the proxy requires every token's `aud` to equal it and publishes it in its protected-resource metadata. Empty, no client could obtain a token the server accepts." -}}
{{- end -}}
{{- if not (include "docs-mcp.resourcePath" .) -}}
{{- fail (printf "docs-mcp: `resourceURL` %q has no path. The proxy forwards the path to /mcp and the HTTPRoute matches it as a prefix; a bare host would take the whole host. Use a path such as https://docs.example.com/mcp." .Values.resourceURL) -}}
{{- end -}}
{{- if not .Values.proxy.image.tag -}}
{{- fail "docs-mcp: `proxy.image.tag` is empty. The resource-proxy sidecar is built by another repository's release, so this chart carries no default version; pin the release you have verified." -}}
{{- end -}}
{{- if not .Values.proxy.image.digest -}}
{{- fail "docs-mcp: `proxy.image.digest` is empty. The sidecar checks every token and is pinned by tag AND digest, so a moved tag cannot change what guards the server." -}}
{{- end -}}
{{- if not .Values.pack.artifact.digest -}}
{{- fail "docs-mcp: `pack.artifact.digest` is empty. The pack is pulled by digest (sha256:<64 hex>); the server refuses to start on a tag alone." -}}
{{- end -}}
{{- if and (ne .Values.registry.auth "dockerconfig") .Values.pack.pullSecretName -}}
{{- fail (printf "docs-mcp: pack.pullSecretName is the Docker config of registry.auth=dockerconfig; registry.auth is %q" .Values.registry.auth) -}}
{{- end -}}
{{- if and (eq .Values.registry.auth "ecr") (not .Values.serviceAccount.create) (not .Values.serviceAccount.name) -}}
{{- fail "docs-mcp: registry.auth=ecr binds AWS credentials to the pod's ServiceAccount: set serviceAccount.create, or serviceAccount.name of an existing one that the cluster has bound" -}}
{{- end -}}
{{- if and .Values.registry.ack.enabled (ne .Values.registry.auth "ecr") -}}
{{- fail "docs-mcp: registry.ack.enabled gives the pod AWS credentials for ECR: it needs registry.auth=ecr" -}}
{{- end -}}
{{- if and .Values.podDisruptionBudget.enabled (lt (int .Values.replicaCount) 2) -}}
{{- fail (printf "docs-mcp: podDisruptionBudget.enabled is true but replicaCount is %d. A budget of one available on a single replica blocks every node drain. Run two replicas, or turn the budget off." (int .Values.replicaCount)) -}}
{{- end -}}
{{- if and .Values.networkPolicy.enabled (not .Values.networkPolicy.ingressFrom) -}}
{{- fail "docs-mcp: `networkPolicy.enabled` is true but `networkPolicy.ingressFrom` is empty, so the rendered policy would admit NOBODY to the proxy's port, the gateway included. Name the gateway's peers, or set `networkPolicy.enabled: false` if the cluster's CNI does not enforce NetworkPolicy." -}}
{{- end -}}
{{- if .Values.httpRoute.enabled -}}
{{- if not .Values.httpRoute.parentRefs -}}
{{- fail "docs-mcp: `httpRoute.enabled` is true but `httpRoute.parentRefs` is empty, so the route would attach to no Gateway and answer nothing. Name the Gateway or ListenerSet it attaches to." -}}
{{- end -}}
{{- if and .Values.httpRoute.hostnames (not (has (include "docs-mcp.resourceHost" .) .Values.httpRoute.hostnames)) -}}
{{- fail (printf "docs-mcp: the host %q of `resourceURL` is not in `httpRoute.hostnames` (%s); the route would not match the URL clients are told to use." (include "docs-mcp.resourceHost" .) (join ", " .Values.httpRoute.hostnames)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
