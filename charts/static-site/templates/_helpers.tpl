{{- define "static-site.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "static-site.fullname" -}}
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

{{- define "static-site.serviceAccountName" -}}
{{- default (include "static-site.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/* The selector is immutable on a live Deployment: identity only. */}}
{{- define "static-site.selectorLabels" -}}
app.kubernetes.io/name: {{ include "static-site.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "static-site.labels" -}}
{{ include "static-site.selectorLabels" . }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "static-site.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- if .Values.image.digest -}}
{{- printf "%s:%s@%s" .Values.image.repository $tag .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end -}}
{{- end -}}

{{/* The placeholder file; its checksum rolls the pods when a value changes. */}}
{{- define "static-site.placeholders" -}}
{{- toPrettyJson (include "static-site.inlinePlaceholders" . | fromJson) -}}
{{- end -}}

{{/* The inline name -> value placeholders: placeholders minus the Secret keys. */}}
{{- define "static-site.inlinePlaceholders" -}}
{{- toJson (omit (default dict .Values.placeholders) "secretName" "secretKeys" "optionalKeys" "externalSecret") -}}
{{- end -}}

{{/* Refuse a Secret source that declares no names. */}}
{{- define "static-site.validatePlaceholders" -}}
{{- if .Values.placeholders.secretName -}}
{{- if not (or .Values.placeholders.secretKeys .Values.placeholders.optionalKeys) -}}
{{- fail "placeholders.secretName needs placeholders.secretKeys or placeholders.optionalKeys: the names the Secret provides" -}}
{{- end -}}
{{- end -}}
{{- if or .Values.placeholders.secretKeys .Values.placeholders.optionalKeys -}}
{{- if not .Values.placeholders.secretName -}}
{{- fail "placeholders.secretKeys and placeholders.optionalKeys need placeholders.secretName" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Refuse an externalSecret that does not fill exactly the Secret's keys. */}}
{{- define "static-site.validateExternalSecret" -}}
{{- with .Values.placeholders.externalSecret -}}
{{- if not $.Values.placeholders.secretName -}}
{{- fail "placeholders.externalSecret needs placeholders.secretName: the Secret it fills" -}}
{{- end -}}
{{- $want := concat (default list $.Values.placeholders.secretKeys) (default list $.Values.placeholders.optionalKeys) | uniq | sortAlpha -}}
{{- $have := keys .properties | sortAlpha -}}
{{- if ne (toJson $want) (toJson $have) -}}
{{- fail (printf "placeholders.externalSecret.properties has the keys %s, but placeholders.secretKeys plus optionalKeys are %s: they must be the same names" (join ", " $have) (join ", " $want)) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/* Refuse registry settings that contradict each other. */}}
{{- define "static-site.validateRegistry" -}}
{{- if and (ne .Values.registry.auth "dockerconfig") .Values.site.pullSecretName -}}
{{- fail (printf "site.pullSecretName is the Docker config of registry.auth=dockerconfig; registry.auth is %q" .Values.registry.auth) -}}
{{- end -}}
{{- if and (eq .Values.registry.auth "ecr") (not .Values.serviceAccount.create) (not .Values.serviceAccount.name) -}}
{{- fail "registry.auth=ecr binds AWS credentials to the pod's ServiceAccount: set serviceAccount.create, or serviceAccount.name of an existing one that the cluster has bound" -}}
{{- end -}}
{{- if and .Values.registry.ack.enabled (ne .Values.registry.auth "ecr") -}}
{{- fail "registry.ack.enabled gives the pod AWS credentials for ECR: it needs registry.auth=ecr" -}}
{{- end -}}
{{- end -}}

{{- define "static-site.validateLive" -}}
{{- if and .Values.live.enabled (not .Values.live.files) -}}
{{- fail "live.enabled needs live.files: the file names served from the live volume" -}}
{{- end -}}
{{- if and .Values.live.files (not .Values.live.enabled) -}}
{{- fail "live.files needs live.enabled" -}}
{{- end -}}
{{- range .Values.extraContainers -}}
{{- if eq .name "static-site" -}}
{{- fail "extraContainers: the name static-site is the server's" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
"true" when the pod's cloud identity is managed by the static-site-infra chart:
it renders the ACK objects and the ServiceAccount, this chart only names the
account and checks, in a hook, that a pod admitted now has the identity.
Derived, never a switch: registry.auth=ecr with registry.ack.enabled.
*/}}
{{- define "static-site.identityManaged" -}}
{{- if and (eq .Values.registry.auth "ecr") .Values.registry.ack.enabled -}}true{{- end -}}
{{- end -}}
