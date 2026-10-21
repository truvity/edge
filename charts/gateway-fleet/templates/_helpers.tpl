{{/*
gateway-fleet helpers.

Two objects, two scopes:

  classes.<name>    a GatewayClass and, when the class is merged, the one
                    EnvoyProxy every Gateway of that class shares.
  exposures.<name>  a Gateway (one entry point: public, private, …) with its
                    health listener, its allowedListeners rule, its baseline
                    ClientTrafficPolicy and — when the class is NOT merged —
                    its own EnvoyProxy, which is what gives one exposure its
                    own Service, its own address and its own failure domain.

Every default lives here and is merged UNDER the caller's values, so no
template ever tests for a missing key.
*/}}

{{/* ------------------------------------------------------------------ */}}
{{/* Annotations for one object: commonAnnotations ← the object's own.   */}}
{{/* Empty map → callers skip the key entirely.                          */}}
{{/* ------------------------------------------------------------------ */}}
{{- define "gateway.annotations" -}}
{{- mergeOverwrite (deepCopy (.root.Values.commonAnnotations | default dict)) (.extra | default dict) | toYaml -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* EnvoyProxy defaults, shared by the class-level and exposure-level   */}}
{{/* proxy blocks — they are the same object in two places.              */}}
{{/* ------------------------------------------------------------------ */}}
{{- define "gateway.proxy.defaults" -}}
enabled: true
name: ""
annotations: {}
filterOrder: []
replicas: 3
podDisruptionBudget:
  maxUnavailable: 1
  minAvailable: null
passthrough: false
pod:
  tolerations: []
  nodeSelector: {}
  affinity: {}
  topologySpreadConstraints: []
  zoneSpread: ""
service:
  name: ""
  type: ClusterIP
  clusterIP: ""
  annotations: {}
  labels: {}
  loadBalancerClass: ""
  loadBalancerSourceRanges: []
  externalTrafficPolicy: ""
  patch: {}
useListenerPortAsContainerPort: false
shutdown:
  drainTimeout: ""
  minDrainDuration: ""
  healthCheckFailureDelay: ""
backendTLS:
  clientCertificateRef:
    name: ""
    namespace: ""
accessLog:
  extraFields: {}
extraSpec: {}
{{- end -}}

{{/*
Envoy Gateway's own JSON access-log fields: what a proxy logs when its
EnvoyProxy sets no telemetry. Copied from the controller
(internal/xds/translator/accesslog.go, EnvoyJSONLogFields) because a JSON
format REPLACES that default rather than extending it: adding one field
without these would silently drop the other twenty-four. Re-check on a
controller upgrade.
*/}}
{{- define "gateway.proxy.accessLogDefaultFields" -}}
start_time: "%START_TIME%"
method: "%REQ(:METHOD)%"
x-envoy-origin-path: "%REQ(X-ENVOY-ORIGINAL-PATH?:PATH)%"
protocol: "%PROTOCOL%"
response_code: "%RESPONSE_CODE%"
response_flags: "%RESPONSE_FLAGS%"
response_code_details: "%RESPONSE_CODE_DETAILS%"
connection_termination_details: "%CONNECTION_TERMINATION_DETAILS%"
upstream_transport_failure_reason: "%UPSTREAM_TRANSPORT_FAILURE_REASON%"
bytes_received: "%BYTES_RECEIVED%"
bytes_sent: "%BYTES_SENT%"
duration: "%DURATION%"
x-envoy-upstream-service-time: "%RESP(X-ENVOY-UPSTREAM-SERVICE-TIME)%"
x-forwarded-for: "%REQ(X-FORWARDED-FOR)%"
user-agent: "%REQ(USER-AGENT)%"
x-request-id: "%REQ(X-REQUEST-ID)%"
":authority": "%REQ(:AUTHORITY)%"
upstream_host: "%UPSTREAM_HOST%"
upstream_cluster: "%UPSTREAM_CLUSTER%"
upstream_local_address: "%UPSTREAM_LOCAL_ADDRESS%"
downstream_local_address: "%DOWNSTREAM_LOCAL_ADDRESS%"
downstream_remote_address: "%DOWNSTREAM_REMOTE_ADDRESS%"
requested_server_name: "%REQUESTED_SERVER_NAME%"
route_name: "%ROUTE_NAME%"
{{- end -}}

{{/*
Resolve one proxy block: the chart's defaults, then the values' `proxyDefaults`
(the settings every proxy of the install shares), then the block itself.
`defaultName` and `defaultServiceName` supply the deterministic names a
consumer can alias onto.
  {{- $p := include "gateway.proxy.resolve" (dict "root" $ "spec" $x.proxy "defaultName" "a" "defaultServiceName" "b") | fromYaml }}

`passthrough` marks a fleet that carries TLS passthrough entries. A passthrough
connection is raw TCP to Envoy: it cannot be told to go away, only closed, so a
rolling proxy has to keep accepting until the load balancer has stopped sending
it new flows, and then give open connections a bounded time to finish. The
shutdown fields it leaves empty default to: failing readiness 10s before the
drain starts (the endpoint removal reaches the load balancer and kube-proxy),
never exiting earlier than 30s (a load balancer's deregistration delay is
usually 30s, and an idle proxy exiting sooner refuses the flows still routed to
it), and closing a client's keep-alive connection after 60s at the latest.
*/}}
{{- define "gateway.proxy.resolve" -}}
{{- $d := include "gateway.proxy.defaults" . | fromYaml -}}
{{- $p := mergeOverwrite $d (deepCopy (.root.Values.proxyDefaults | default dict)) (.spec | default dict) -}}
{{- if $p.passthrough -}}
{{- if not $p.shutdown.healthCheckFailureDelay }}{{- $_ := set $p.shutdown "healthCheckFailureDelay" "10s" }}{{- end -}}
{{- if not $p.shutdown.minDrainDuration }}{{- $_ := set $p.shutdown "minDrainDuration" "30s" }}{{- end -}}
{{- if not $p.shutdown.drainTimeout }}{{- $_ := set $p.shutdown "drainTimeout" "60s" }}{{- end -}}
{{- end -}}
{{- if not $p.name }}{{- $_ := set $p "name" .defaultName }}{{- end -}}
{{- if not $p.service.name }}{{- $_ := set $p.service "name" .defaultServiceName }}{{- end -}}
{{- toYaml $p -}}
{{- end -}}

{{/*
Build EnvoyProxy .spec from a resolved proxy block. `merge` is the class's
mergeGateways flag and is rendered only on a class-level proxy: a merged
class has exactly one proxy, so the flag has no meaning per exposure.
  {{- include "gateway.proxy.spec" (dict "proxy" $p "merge" true "renderMerge" true "selector" $labels) }}

`selector` is the label set that selects this proxy's pods, for the zone
spread constraint (`pod.zoneSpread`): one pod of the fleet per zone, as far as
the autoscaler can provide, with `DoNotSchedule` making it a hard rule (the
autoscaler then adds a node in the zone that lacks a proxy instead of stacking
two in one zone) and `ScheduleAnyway` a soft one. The constraint carries
`matchLabelKeys: [pod-template-hash]`, so a rolling update spreads the new
ReplicaSet's pods by themselves and ends one per zone.

The PodDisruptionBudget is `maxUnavailable` (one proxy at a time, whatever the
replica count), unless `minAvailable` is set: then `minAvailable` alone, and
`minAvailable: 0` renders no budget at all. EnvoyProxy takes one of the two,
never both.
*/}}
{{- define "gateway.proxy.spec" -}}
{{- $p := .proxy -}}
{{- $svc := dict "name" $p.service.name "type" $p.service.type -}}
{{- with $p.service.annotations }}{{- $_ := set $svc "annotations" . }}{{- end -}}
{{- with $p.service.labels }}{{- $_ := set $svc "labels" . }}{{- end -}}
{{- with $p.service.loadBalancerClass }}{{- $_ := set $svc "loadBalancerClass" . }}{{- end -}}
{{- with $p.service.loadBalancerSourceRanges }}{{- $_ := set $svc "loadBalancerSourceRanges" . }}{{- end -}}
{{- with $p.service.externalTrafficPolicy }}{{- $_ := set $svc "externalTrafficPolicy" . }}{{- end -}}
{{- /* A pinned ClusterIP is not an envoyService field: it reaches the
       Service through the controller's own patch hook. A caller-supplied
       patch merges over it, so both can be used together. */ -}}
{{- $patch := dict -}}
{{- with $p.service.clusterIP }}
{{- $patch = dict "type" "StrategicMerge" "value" (dict "spec" (dict "clusterIP" .)) -}}
{{- end -}}
{{- $patch = mergeOverwrite $patch ($p.service.patch | default dict) -}}
{{- with $patch }}{{- $_ := set $svc "patch" . }}{{- end -}}
{{- $pod := dict -}}
{{- with $p.pod.tolerations }}{{- $_ := set $pod "tolerations" . }}{{- end -}}
{{- with $p.pod.nodeSelector }}{{- $_ := set $pod "nodeSelector" . }}{{- end -}}
{{- with $p.pod.affinity }}{{- $_ := set $pod "affinity" . }}{{- end -}}
{{- $spread := $p.pod.topologySpreadConstraints | default list -}}
{{- if $p.pod.zoneSpread -}}
{{- $spread = append $spread (dict
      "maxSkew" 1
      "topologyKey" "topology.kubernetes.io/zone"
      "whenUnsatisfiable" $p.pod.zoneSpread
      "labelSelector" (dict "matchLabels" .selector)
      "matchLabelKeys" (list "pod-template-hash")) -}}
{{- end -}}
{{- with $spread }}{{- $_ := set $pod "topologySpreadConstraints" . }}{{- end -}}
{{- $deploy := dict "replicas" $p.replicas -}}
{{- with $pod }}{{- $_ := set $deploy "pod" . }}{{- end -}}
{{- $k8s := dict "envoyService" $svc "envoyDeployment" $deploy -}}
{{- $pdb := $p.podDisruptionBudget -}}
{{- if not (kindIs "invalid" $pdb.minAvailable) -}}
{{- if $pdb.minAvailable }}{{- $_ := set $k8s "envoyPDB" (dict "minAvailable" $pdb.minAvailable) }}{{- end -}}
{{- else if $pdb.maxUnavailable -}}
{{- $_ := set $k8s "envoyPDB" (dict "maxUnavailable" $pdb.maxUnavailable) -}}
{{- end -}}
{{- if $p.useListenerPortAsContainerPort }}{{- $_ := set $k8s "useListenerPortAsContainerPort" true }}{{- end -}}
{{- $s := dict "provider" (dict "type" "Kubernetes" "kubernetes" $k8s) -}}
{{- if .renderMerge }}{{- $_ := set $s "mergeGateways" .merge }}{{- end -}}
{{- with $p.filterOrder }}{{- $_ := set $s "filterOrder" . }}{{- end -}}
{{- $shutdown := dict -}}
{{- with $p.shutdown.drainTimeout }}{{- $_ := set $shutdown "drainTimeout" . }}{{- end -}}
{{- with $p.shutdown.minDrainDuration }}{{- $_ := set $shutdown "minDrainDuration" . }}{{- end -}}
{{- with $p.shutdown.healthCheckFailureDelay }}{{- $_ := set $shutdown "healthCheckFailureDelay" . }}{{- end -}}
{{- with $shutdown }}{{- $_ := set $s "shutdown" . }}{{- end -}}
{{- with $p.backendTLS.clientCertificateRef.name }}
{{- $ref := dict "kind" "Secret" "name" . -}}
{{- with $p.backendTLS.clientCertificateRef.namespace }}{{- $_ := set $ref "namespace" . }}{{- end -}}
{{- $_ := set $s "backendTLS" (dict "clientCertificateRef" $ref) -}}
{{- end -}}
{{- /* Extra access-log fields: the controller's default JSON line plus
       these, to stdout, which is where the default goes. Nothing is
       rendered without them, so the controller's default stays in charge. */ -}}
{{- with $p.accessLog.extraFields }}
{{- $fields := mergeOverwrite (include "gateway.proxy.accessLogDefaultFields" $ | fromYaml) . -}}
{{- $setting := dict
      "format" (dict "type" "JSON" "json" $fields)
      "sinks" (list (dict "type" "File" "file" (dict "path" "/dev/stdout"))) -}}
{{- $_ := set $s "telemetry" (dict "accessLog" (dict "settings" (list $setting))) -}}
{{- end -}}
{{- $s = mergeOverwrite $s ($p.extraSpec | default dict) -}}
{{- toYaml $s -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* Class                                                               */}}
{{/* ------------------------------------------------------------------ */}}
{{- define "gateway.class.defaults" -}}
enabled: true
namespace: envoy-gateway-system
annotations: {}
mergeGateways: false
proxy: {}
{{- end -}}

{{- define "gateway.class.resolve" -}}
{{- $d := include "gateway.class.defaults" . | fromYaml -}}
{{- $c := mergeOverwrite $d (.spec | default dict) -}}
{{- $_ := set $c "name" .name -}}
{{- /* A merged class needs its class proxy: it is the only one. A split
       class does not, because every exposure brings its own — so the class
       proxy defaults to the merge flag, and an explicit `enabled` still
       wins if an estate wants a fallback for Gateways it does not own. */ -}}
{{- $rawProxy := (.spec | default dict).proxy | default dict -}}
{{- if not (hasKey $rawProxy "enabled") }}{{- $_ := set $c.proxy "enabled" $c.mergeGateways }}{{- end -}}
{{- $_ := set $c "proxy" (include "gateway.proxy.resolve" (dict
      "root" .root
      "spec" $c.proxy
      "defaultName" (printf "%s-config" .name)
      "defaultServiceName" (printf "gateway-%s" .name)) | fromYaml) -}}
{{- toYaml $c -}}
{{- end -}}

{{/* ------------------------------------------------------------------ */}}
{{/* Exposure                                                            */}}
{{/* ------------------------------------------------------------------ */}}
{{- define "gateway.exposure.defaults" -}}
enabled: true
class: ""
namespace: ""
gatewayName: ""
annotations: {}
labels: {}
allowedListeners:
  namespaces:
    from: Same
health:
  listenerName: ""
  hostname: ""
  port: 443
  protocol: HTTPS
  tls:
    secretName: ""
  certificate:
    enabled: true
    name: ""
    annotations: {}
    labels: {}
    duration: ""
    renewBefore: ""
    privateKey:
      rotationPolicy: Always
    usages: []
    issuerRef:
      kind: ClusterIssuer
      group: cert-manager.io
  allowedRoutes:
    namespaces:
      from: Same
    kinds:
      - group: gateway.networking.k8s.io
        kind: HTTPRoute
  directResponse:
    enabled: false
    routeName: ""
    filterName: ""
    annotations: {}
    labels: {}
    path: /healthz
    statusCode: 200
    contentType: text/plain
    body: ok
clientTrafficPolicy:
  enabled: false
  name: ""
  annotations: {}
  labels: {}
  tls:
    enabled: true
    minVersion: "1.3"
    maxVersion: "1.3"
    clientValidation:
      enabled: false
      optional: false
      allowExpiredCertificate: false
      caCertificateRefs: []
proxy: {}
networkPolicy:
  enabled: false
  name: ""
  annotations: {}
  ingress: []
  xds:
    enabled: true
    controllerPodLabels:
      control-plane: envoy-gateway
    port: 18000
  egress: []
  egressTo: []
services: []
egressPolicies: []
{{- end -}}

{{/*
One `egressTo` row as a NetworkPolicy egress rule. A fleet's egress is an
allow-list, and publishing a backend needs BOTH halves of its path: the route
and the backend's own ingress policy are not enough, because nothing in the
Gateway API opens the proxy's way out to the pod. Every such rule has the same
shape, so a row names only what differs:

  namespace             the backend's namespace by name; "*" is every namespace
  namespaceExpressions  matchExpressions over namespace labels, instead of a
                        namespace name
  podLabels             the backend pods' labels; none selects the whole
                        namespace
  port, protocol        the POD's port (a NetworkPolicy never names the
                        Service's); none allows every port
*/}}
{{- define "gateway.egress.row" -}}
{{- $ns := dict -}}
{{- if .namespaceExpressions -}}
{{- $_ := set $ns "matchExpressions" .namespaceExpressions -}}
{{- else if ne (.namespace | default "") "*" -}}
{{- $_ := set $ns "matchLabels" (dict "kubernetes.io/metadata.name" .namespace) -}}
{{- end -}}
{{- $peer := dict "namespaceSelector" $ns -}}
{{- with .podLabels }}{{- $_ := set $peer "podSelector" (dict "matchLabels" .) }}{{- end -}}
{{- $rule := dict "to" (list $peer) -}}
{{- if .port }}{{- $_ := set $rule "ports" (list (dict "port" .port "protocol" (.protocol | default "TCP"))) }}{{- end -}}
{{- toYaml $rule -}}
{{- end -}}

{{/*
Resolve one exposure against its class.
  {{- $e := include "gateway.exposure.resolve" (dict "root" $ "name" $n "spec" $s "class" $c) | fromYaml }}
*/}}
{{- define "gateway.exposure.resolve" -}}
{{- $d := include "gateway.exposure.defaults" . | fromYaml -}}
{{- $e := mergeOverwrite $d (.spec | default dict) -}}
{{- $_ := set $e "name" .name -}}
{{- if not $e.namespace }}{{- $_ := set $e "namespace" .class.namespace }}{{- end -}}
{{- if not $e.gatewayName }}{{- $_ := set $e "gatewayName" .name }}{{- end -}}
{{- if not $e.health.listenerName }}{{- $_ := set $e.health "listenerName" (lower $e.health.protocol) }}{{- end -}}
{{- if not $e.health.tls.secretName }}{{- $_ := set $e.health.tls "secretName" (printf "%s-health-tls" $e.gatewayName) }}{{- end -}}
{{- if not $e.health.certificate.name }}{{- $_ := set $e.health.certificate "name" $e.health.tls.secretName }}{{- end -}}
{{- $healthName := printf "%s-health" $e.gatewayName | trunc 63 | trimSuffix "-" -}}
{{- if not $e.health.directResponse.routeName }}{{- $_ := set $e.health.directResponse "routeName" $healthName }}{{- end -}}
{{- if not $e.health.directResponse.filterName }}{{- $_ := set $e.health.directResponse "filterName" $healthName }}{{- end -}}
{{- if not $e.clientTrafficPolicy.name }}{{- $_ := set $e.clientTrafficPolicy "name" (printf "%s-tls" $e.gatewayName) }}{{- end -}}
{{- if not $e.networkPolicy.name }}{{- $_ := set $e.networkPolicy "name" (printf "envoy-%s" $e.name) }}{{- end -}}
{{- $_ := set $e "proxy" (include "gateway.proxy.resolve" (dict
      "root" .root
      "spec" $e.proxy
      "defaultName" (printf "%s-proxy" $e.name)
      "defaultServiceName" (printf "gateway-%s" $e.name)) | fromYaml) -}}
{{- toYaml $e -}}
{{- end -}}

{{/*
Does this exposure render its own EnvoyProxy? Only a non-merged class can
have one: a merged class collapses every Gateway onto the class proxy, and
`spec.infrastructure` is not honoured there.
Returns "true" or "".
*/}}
{{/*
Resolve one passthroughs entry.
  {{- $p := include "gateway.passthrough.resolve" (dict "name" $n "spec" $s) | fromYaml }}
*/}}
{{- define "gateway.passthrough.defaults" -}}
enabled: true
class: ""
namespace: ""
gatewayName: ""
annotations: {}
labels: {}
listenerName: tls
hostname: ""
port: 443
route:
  name: ""
  annotations: {}
  labels: {}
  backend:
    name: ""
    port: 0
    weight: 1
{{- end -}}

{{- define "gateway.passthrough.resolve" -}}
{{- $p := mergeOverwrite (include "gateway.passthrough.defaults" . | fromYaml) (deepCopy (.spec | default dict)) -}}
{{- $_ := set $p "name" .name -}}
{{- if not $p.gatewayName }}{{- $_ := set $p "gatewayName" .name }}{{- end -}}
{{- if not $p.route.name }}{{- $_ := set $p.route "name" $p.gatewayName }}{{- end -}}
{{- toYaml $p -}}
{{- end -}}

{{- define "gateway.exposure.ownProxy" -}}
{{- if and (not .class.mergeGateways) .exposure.proxy.enabled -}}true{{- end -}}
{{- end -}}

{{/*
The pod labels that select this exposure's proxies. Envoy Gateway stamps
owning-gatewayclass on a MERGED fleet and owning-gateway-name/-namespace on
a per-Gateway one, so a NetworkPolicy must follow the mode.
*/}}
{{- define "gateway.podSelector" -}}
app.kubernetes.io/name: envoy
{{- if .class.mergeGateways }}
gateway.envoyproxy.io/owning-gatewayclass: {{ .class.name }}
{{- else }}
gateway.envoyproxy.io/owning-gateway-name: {{ .exposure.gatewayName }}
gateway.envoyproxy.io/owning-gateway-namespace: {{ .exposure.namespace }}
{{- end }}
{{- end -}}

{{/*
Normalize RouteGroupKind entries so an accepted short form renders
explicitly — a GitOps controller diffs forever against a server default.
*/}}
{{- define "gateway.allowedRoutes.resolve" -}}
{{- $routes := deepCopy . -}}
{{- $kinds := list -}}
{{- range $kind := $routes.kinds -}}
{{- $normalized := deepCopy $kind -}}
{{- if not (hasKey $normalized "group") }}{{- $_ := set $normalized "group" "gateway.networking.k8s.io" }}{{- end -}}
{{- $kinds = append $kinds $normalized -}}
{{- end -}}
{{- $_ := set $routes "kinds" $kinds -}}
{{- toYaml $routes -}}
{{- end -}}

{{/*
Claim one object identity in a registry, failing when two entries collide.
A dict is a reference, so the registry accumulates across one render.
  {{- include "gateway.claim" (dict "registry" $r "kind" "Gateway" "namespace" $ns "name" $n "by" "exposures.public") }}
*/}}
{{- define "gateway.claim" -}}
{{- $key := printf "%s/%s/%s" .kind .namespace .name -}}
{{- if hasKey .registry $key -}}
{{- fail (printf "%s %s/%s is claimed by both %s and %s — two objects cannot share one name" .kind .namespace .name (get .registry $key) .by) -}}
{{- end -}}
{{- $_ := set .registry $key .by -}}
{{- end -}}

{{/*
The metricRelabelings of the proxy PodMonitor: the keep rule built from the
keep list (none when the list is empty), then the estate's own rules. Every
rule must write `action`: the prometheus-operator CRD defaults it, a
converter may not.
*/}}
{{- define "gateway.podMonitor.rules" -}}
{{- /* .keep: list of regexes, .extra: verbatim relabel rules. */ -}}
{{- $rules := list -}}
{{- if .keep -}}
{{- $rules = append $rules (dict "action" "keep" "sourceLabels" (list "__name__") "regex" (join "|" .keep)) -}}
{{- end -}}
{{- range $i, $r := .extra -}}
{{- if not (hasKey $r "action") -}}
{{- fail (printf "metrics.podMonitor.proxy.metricRelabelings[%d]: every rule must write `action` explicitly" $i) -}}
{{- end -}}
{{- $rules = append $rules $r -}}
{{- end -}}
{{- toYaml $rules -}}
{{- end -}}
