{{/*
Common helpers shared by all demo charts.
Named templates use the "demo" prefix to avoid conflicts with agent-orc-resources
when both charts are deployed into the same namespace.
*/}}

{{- define "demo-experiment-evals.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "demo-experiment-evals.labels" -}}
helm.sh/chart: {{ include "demo-experiment-evals.name" . }}-{{ .Chart.Version | replace "+" "_" }}
app.kubernetes.io/name: {{ include "demo-experiment-evals.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}