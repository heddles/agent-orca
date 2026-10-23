{{/*
Expand the name of the chart.
*/}}
{{- define "agent-orca.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "agent-orca.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "agent-orca.labels" -}}
helm.sh/chart: {{ include "agent-orca.name" . }}-{{ .Chart.Version | replace "+" "_" }}
{{ include "agent-orca.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "agent-orca.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-orca.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Hindsight API URL the operator and model-routers use: the explicit hindsight.url,
else the bundled server's in-cluster service, else empty (memory off).
*/}}
{{- define "agent-orca.hindsight.url" -}}
{{- if .Values.hindsight.url -}}
{{- .Values.hindsight.url -}}
{{- else if .Values.hindsight.enabled -}}
{{- $port := (((.Values.hindsight.api).service).port | default 8888) | int -}}
{{- printf "http://%s-api.%s.svc.cluster.local:%d" .Values.hindsight.fullnameOverride .Release.Namespace $port -}}
{{- end -}}
{{- end }}

{{/*
Secret holding the tenant API key sent to Hindsight. An explicit
hindsight.apiKeySecret wins; otherwise the bundled server's existingSecret.
*/}}
{{- define "agent-orca.hindsight.apiKeySecretName" -}}
{{- if .Values.hindsight.apiKeySecret.name -}}
{{- .Values.hindsight.apiKeySecret.name -}}
{{- else if .Values.hindsight.enabled -}}
{{- .Values.hindsight.existingSecret -}}
{{- end -}}
{{- end }}

{{- define "agent-orca.hindsight.apiKeySecretKey" -}}
{{- default "HINDSIGHT_API_TENANT_API_KEY" .Values.hindsight.apiKeySecret.key -}}
{{- end }}
