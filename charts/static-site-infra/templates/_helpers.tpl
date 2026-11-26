{{/*
Names and labels are those of the static-site chart for the install called
installName, so that the objects moved here are the live ones, unchanged.
*/}}
{{- define "static-site-infra.installName" -}}
{{- default .Release.Name .Values.installName -}}
{{- end -}}

{{/* The static-site chart's name: its labels carry it, whatever this chart is called. */}}
{{- define "static-site-infra.name" -}}
{{- default "static-site" .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "static-site-infra.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default "static-site" .Values.nameOverride -}}
{{- $install := include "static-site-infra.installName" . -}}
{{- if contains $name $install -}}
{{- $install | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" $install $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "static-site-infra.serviceAccountName" -}}
{{- default (include "static-site-infra.fullname" .) .Values.serviceAccount.name -}}
{{- end -}}

{{/* The labels static-site renders on the same objects. */}}
{{- define "static-site-infra.labels" -}}
app.kubernetes.io/name: {{ include "static-site-infra.name" . }}
app.kubernetes.io/instance: {{ include "static-site-infra.installName" . }}
{{- with .Chart.AppVersion }}
app.kubernetes.io/version: {{ . | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/* "true" when this chart renders anything. */}}
{{- define "static-site-infra.enabled" -}}
{{- if and (eq .Values.registry.auth "ecr") .Values.registry.ack.enabled -}}true{{- end -}}
{{- end -}}

{{/* The IAM Role and Policy name, and the name of their ACK objects. */}}
{{- define "static-site-infra.ackName" -}}
{{- default (printf "%s-%s-%s" .Values.registry.ack.clusterName .Release.Namespace (include "static-site-infra.installName" .)) .Values.registry.ack.rolePrefix -}}
{{- end -}}

{{/* Refuse settings that contradict each other. */}}
{{- define "static-site-infra.validate" -}}
{{- if and .Values.flags.appconfig.configurationArn (not .Values.registry.ack.enabled) -}}
{{- fail "flags.appconfig.configurationArn is a statement on the role registry.ack makes: it needs registry.ack.enabled" -}}
{{- end -}}
{{- with .Values.registry.ack -}}
{{- if .enabled -}}
{{- if ne $.Values.registry.auth "ecr" -}}
{{- fail "registry.ack.enabled gives the pod AWS credentials for ECR: it needs registry.auth=ecr" -}}
{{- end -}}
{{- if not .clusterName -}}
{{- fail "registry.ack.enabled needs registry.ack.clusterName: the EKS cluster of the PodIdentityAssociation" -}}
{{- end -}}
{{- if not .repositoryArns -}}
{{- fail "registry.ack.enabled needs registry.ack.repositoryArns: the ECR repositories the role may pull from" -}}
{{- end -}}
{{- if ne (empty .accountId) (empty .region) -}}
{{- fail "registry.ack.accountId and registry.ack.region pin the trust to the cluster: set both or neither" -}}
{{- end -}}
{{- if gt (len (include "static-site-infra.ackName" $)) 64 -}}
{{- fail (printf "the IAM name %q is longer than 64 characters: set registry.ack.rolePrefix" (include "static-site-infra.ackName" $)) -}}
{{- end -}}
{{- end -}}
{{- /* Outside `enabled`: a list that limits the statements must never be
       skipped because another key is off. */ -}}
{{- if .allowedExtraActions -}}
{{- $allowed := .allowedExtraActions -}}
{{- range $i, $statement := .extraStatements -}}
{{- range $statement.Action -}}
{{- if not (has . $allowed) -}}
{{- fail (printf "registry.ack.extraStatements[%d]: the action %s is not in registry.ack.allowedExtraActions (%s)" $i . (join ", " $allowed)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}
