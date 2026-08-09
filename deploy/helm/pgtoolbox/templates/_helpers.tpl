{{/*
Expand the name of the chart.
*/}}
{{- define "pgtoolbox.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "pgtoolbox.fullname" -}}
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

{{/*
Create the namespace to use.
*/}}
{{- define "pgtoolbox.namespace" -}}
{{- if .Values.namespace.create -}}
{{- .Values.namespace.name -}}
{{- else -}}
{{- .Release.Namespace -}}
{{- end -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "pgtoolbox.labels" -}}
app.kubernetes.io/name: {{ include "pgtoolbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "pgtoolbox.selectorLabels" -}}
app.kubernetes.io/name: {{ include "pgtoolbox.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Manager image reference.
*/}}
{{/*
The manager image. An empty tag means the chart's appVersion, so a chart
installed at 0.1.0 runs 0.1.0 rather than whatever a floating tag points at
on the day. Set image.tag explicitly to pin something else.
*/}}
{{- define "pgtoolbox.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}

{{/*
The default pgtoolbox-proxy image for consoles that name none. The proxy is
built and released from this repository alongside the operator, so it
follows the same version unless overridden.
*/}}
{{- define "pgtoolbox.proxyImage" -}}
{{- if .Values.proxyImage -}}
{{- .Values.proxyImage -}}
{{- else -}}
{{- printf "%s:%s" .Values.proxyImageRepository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}
