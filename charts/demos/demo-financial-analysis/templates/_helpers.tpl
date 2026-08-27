{{/*
Common helpers shared by all demo charts.
Named templates use the "demo" prefix to avoid conflicts with agent-orca-resources
when both charts are deployed into the same namespace.
*/}}

{{- define "demo.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "demo.labels" -}}
helm.sh/chart: {{ include "demo.name" . }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "demo.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
