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
Namespace the OpenShell gateway runs in: openshell.gateway.namespace, or the
release namespace when empty (where the bundled gateway subchart installs).
*/}}
{{- define "agent-orca.openshell.gatewayNamespace" -}}
{{- .Values.openshell.gateway.namespace | default .Release.Namespace -}}
{{- end }}

{{/*
gRPC address of the OpenShell gateway Service: openshell.gateway.address, or
derived from the gateway Service name ("openshell", the subchart's
nameOverride), namespace and port when empty.
*/}}
{{- define "agent-orca.openshell.gatewayAddress" -}}
{{- $ns := include "agent-orca.openshell.gatewayNamespace" . -}}
{{- printf "openshell.%s.svc.cluster.local:%s" $ns (.Values.openshell.gateway.port | toString) -}}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "agent-orca.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-orca.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}
