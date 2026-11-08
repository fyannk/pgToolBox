{{/*
A name cut to at most 63 characters (a DNS label). A name that fits is returned
unchanged, so upgrades keep their objects. A longer one keeps its first 54
characters and gets "-" and the first 8 hex digits of the SHA-256 of the whole
name, so two long names that differ only at the end stay distinct.
*/}}
{{- define "pgtoolbox.safeName" -}}
{{- if le (len .) 63 -}}
{{- . -}}
{{- else -}}
{{- printf "%s-%s" (trunc 54 . | trimSuffix "-") (trunc 8 (sha256sum .)) -}}
{{- end -}}
{{- end -}}

{{/* The port of a bind address (":8080", "127.0.0.1:8080"). */}}
{{- define "pgtoolbox.bindPort" -}}
{{- . | splitList ":" | last | int -}}
{{- end -}}

{{/*
A probe as configured. The default httpGet.port (8081) is replaced by the port of
healthProbeBindAddress, so the probes follow it; any other port is kept.
*/}}
{{- define "pgtoolbox.probe" -}}
{{- $p := deepCopy .probe -}}
{{- if and (hasKey $p "httpGet") (eq (toString (dig "httpGet" "port" "" $p)) "8081") -}}
{{- $_ := set $p.httpGet "port" (include "pgtoolbox.bindPort" $.addr | int) -}}
{{- end -}}
{{- toYaml $p -}}
{{- end -}}

{{/* The operator metrics Service name. */}}
{{- define "pgtoolbox.metricsServiceName" -}}
{{- include "pgtoolbox.safeName" (printf "%s-metrics" (include "pgtoolbox.fullname" .)) -}}
{{- end -}}

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
{{- include "pgtoolbox.safeName" .Values.fullnameOverride -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- include "pgtoolbox.safeName" .Release.Name -}}
{{- else -}}
{{- include "pgtoolbox.safeName" (printf "%s-%s" .Release.Name $name) -}}
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
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/*
The default EvidenceAgent image (--default-evidence-agent-image): the agent is
built and released from this repository with the operator, so it follows the
chart's appVersion unless operator.evidenceAgent.image says otherwise.
*/}}
{{- define "pgtoolbox.evidenceAgentImage" -}}
{{- if .Values.operator.evidenceAgent.image -}}
{{- .Values.operator.evidenceAgent.image -}}
{{- else -}}
{{- printf "%s:%s" .Values.operator.evidenceAgent.imageRepository .Chart.AppVersion -}}
{{- end -}}
{{- end -}}

{{/* The tlsConfig of the operator's monitors: verify against the CA when one is given, else the self-signed default. */}}
{{- define "pgtoolbox.operator.tlsConfig" -}}
{{- $m := .Values.operator.metrics -}}
{{- if $m.tls.caSecret.name }}
ca:
  secret:
    name: {{ $m.tls.caSecret.name }}
    key: {{ $m.tls.caSecret.key }}
serverName: {{ $m.tls.serverName | default (printf "%s.%s.svc" (include "pgtoolbox.metricsServiceName" .) (include "pgtoolbox.namespace" .)) | quote }}
{{- else }}
insecureSkipVerify: true
{{- end }}
{{- end -}}

{{/*
The default PgToolBoxConsole image (--default-console-image): operator.console.image,
a digest winning over the tag, the tag defaulting to the chart's appVersion.
dig: values reused from a release before operator.console.image existed lack it.
*/}}
{{- define "pgtoolbox.consoleImage" -}}
{{- $img := dig "console" "image" dict .Values.operator -}}
{{- $repo := $img.repository | default "ghcr.io/fyannk/pgtoolbox-console" -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $repo $img.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo ($img.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}
