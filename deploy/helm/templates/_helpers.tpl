{{/*
Source Gateway <https://sourcegateway.dev>
Copyright The Source Gateway Authors
SPDX-License-Identifier: Apache-2.0
*/}}
{{- define "sourcegateway.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- define "sourcegateway.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := include "sourcegateway.name" . -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- define "sourcegateway.labels" -}}
app.kubernetes.io/name: {{ include "sourcegateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end -}}
{{- define "sourcegateway.selectorLabels" -}}
app.kubernetes.io/name: {{ include "sourcegateway.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
{{- define "sourcegateway.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}{{ default (include "sourcegateway.fullname" .) .Values.serviceAccount.name }}{{ else }}{{ required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name }}{{ end -}}
{{- end -}}
{{- define "sourcegateway.runtimeConfig" -}}
{{- $git := dict -}}
{{- range $name, $credential := .Values.config.credentials.gitRepository -}}
{{- $runtimeCredential := dict "kind" $credential.kind "namespaces" $credential.namespaces -}}
{{- with $credential.origin }}{{- $_ := set $runtimeCredential "origin" . }}{{- end -}}
{{- $_ := set $git $name $runtimeCredential -}}
{{- end -}}
{{- $object := dict -}}
{{- range $name, $credential := .Values.config.credentials.objectStorageBucket -}}
{{- $runtimeCredential := dict "kind" $credential.kind "namespaces" $credential.namespaces -}}
{{- with $credential.origin }}{{- $_ := set $runtimeCredential "origin" . }}{{- end -}}
{{- $_ := set $object $name $runtimeCredential -}}
{{- end -}}
{{- dict "tokenAudience" .Values.config.tokenAudience "objectCacheCapacity" .Values.config.objectCacheCapacity "maxObjectSize" .Values.config.maxObjectSize "credentialsDirectory" .Values.config.credentialsDirectory "githubUserAnnotation" .Values.config.githubUserAnnotation "listenAddress" .Values.config.listenAddress "metricsAddress" .Values.config.metricsAddress "storageDirectory" .Values.config.storageDirectory "tlsCertificateFile" (printf "/var/run/sourcegateway/tls/%s" .Values.servingTLS.certificateFile) "tlsPrivateKeyFile" (printf "/var/run/sourcegateway/tls/%s" .Values.servingTLS.privateKeyFile) "credentials" (dict "gitRepository" $git "objectStorageBucket" $object) | toPrettyJson -}}
{{- end -}}
