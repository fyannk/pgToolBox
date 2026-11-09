{{/*
The API server addresses and ports of a NetworkPolicy in apiServer.mode
"addresses", as JSON ({"addresses": [...], "ports": [...]}). Taken from
apiServer.addresses; when that is empty and apiServer.lookup is true (the default in this minor), from the
`kubernetes` EndpointSlice (discovery.k8s.io/v1) of the default namespace. Helm
fails the whole render with the API server's Forbidden when the installing user
cannot read that object, and a namespace admin usually cannot (Helm's lookup
returns every error but NotFound). Args: dict "np" <the networkPolicy values>
"path" <its values path, for the message>.
*/}}
{{- define "pgtools.apiServerLookupWarning" -}}
{{- if and .np.enabled (eq .np.apiServer.mode "addresses") .np.apiServer.lookup (not .np.apiServer.addresses) }}

WARNING: {{ .path }}.apiServer.lookup is on and no apiServer.addresses are set, so the chart reads the
`kubernetes` EndpointSlice of the default namespace at install time. That needs `get` on
`endpointslices.discovery.k8s.io` in `default`, which a namespace admin usually lacks: Helm then fails with a Forbidden error. Instead:
list {{ .path }}.apiServer.addresses, set {{ .path }}.apiServer.mode: any, or keep lookup as a cluster admin.
The next minor defaults {{ .path }}.apiServer.lookup to false.
{{- end }}
{{- end -}}

{{- define "pgtools.apiServerAddresses" -}}
{{- $addresses := .np.apiServer.addresses | default list -}}
{{- $ports := .np.apiServer.ports -}}
{{- if and (not $addresses) .np.apiServer.lookup -}}
{{- /* The API server's own EndpointSlice (discovery.k8s.io/v1; v1 Endpoints is
     deprecated since 1.33). One egress rule takes every ready address and
     every port. */ -}}
{{- $slice := lookup "discovery.k8s.io/v1" "EndpointSlice" "default" "kubernetes" -}}
{{- if and $slice $slice.endpoints -}}
{{- $ports = list -}}
{{- range $e := $slice.endpoints -}}
{{- if not (and $e.conditions (eq (toString $e.conditions.ready) "false")) -}}
{{- range $a := $e.addresses }}{{ $addresses = append $addresses $a }}{{ end -}}
{{- end -}}
{{- end -}}
{{- range $pt := $slice.ports }}{{ $ports = append $ports $pt.port }}{{ end -}}
{{- $addresses = $addresses | uniq -}}
{{- $ports = $ports | uniq -}}
{{- end -}}
{{- end -}}
{{- if not $addresses -}}
{{- fail (printf "%s.apiServer.mode=addresses has no API server address to allow. Either set %s.apiServer.addresses (the ClusterIP and endpoints of `kubectl get endpointslice kubernetes -n default`; a namespace admin usually cannot read that object, ask the cluster admin), or set %s.apiServer.lookup=true to read it at install time (needs a user allowed to get EndpointSlices in the default namespace; without that Helm stops with a Forbidden error; and helm template or --dry-run have no cluster), or use %s.apiServer.mode: any." .path .path .path .path) -}}
{{- end -}}
{{- dict "addresses" $addresses "ports" $ports | toJson -}}
{{- end -}}

{{/*
A name cut to at most 63 characters (a DNS label). A name that fits is returned
unchanged, so upgrades keep their objects. A longer one keeps its first 54
characters and gets "-" and the first 8 hex digits of the SHA-256 of the whole
name, so two long names that differ only at the end stay distinct.
*/}}
{{- define "pgtools.safeName" -}}
{{- if le (len .) 63 -}}
{{- . -}}
{{- else -}}
{{- printf "%s-%s" (trunc 54 . | trimSuffix "-") (trunc 8 (sha256sum .)) -}}
{{- end -}}
{{- end -}}

{{/*
The console's object name: console.fullnameOverride, else the release name when
it already says "console", else "<release>-console".
*/}}
{{- define "pgtools.console.fullname" -}}
{{- if .Values.console.fullnameOverride -}}
{{- include "pgtools.safeName" .Values.console.fullnameOverride -}}
{{- else if contains "console" .Release.Name -}}
{{- include "pgtools.safeName" .Release.Name -}}
{{- else -}}
{{- include "pgtools.safeName" (printf "%s-console" .Release.Name) -}}
{{- end -}}
{{- end -}}

{{/*
Selector labels of the console pods. The name differs from the operator's
("pgtoolbox") so the two charts, installed in the same namespace, never
select each other's pods.
*/}}
{{- define "pgtools.console.selectorLabels" -}}
app.kubernetes.io/name: pgtoolbox-console
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: console
{{- end -}}

{{- define "pgtools.console.labels" -}}
{{ include "pgtools.console.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: pgtoolbox
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
The watched namespaces: console.watchNamespaces, else the release namespace;
deduplicated, in the order given.
*/}}
{{- define "pgtools.console.watchNamespaces" -}}
{{- $ns := .Values.console.watchNamespaces | default (list) -}}
{{- if not $ns -}}{{- $ns = list .Release.Namespace -}}{{- end -}}
{{- toJson (uniq $ns) -}}
{{- end -}}

{{/*
The image reference. A digest wins over a tag; global.imageRegistry, when set,
replaces the registry host of the repository.
*/}}
{{- define "pgtools.console.image" -}}
{{- $repo := .Values.console.image.repository -}}
{{- with .Values.global.imageRegistry -}}
{{- $repo = printf "%s/%s" (trimSuffix "/" .) (regexReplaceAll "^[^/]+/" $repo "") -}}
{{- end -}}
{{- if .Values.console.image.digest -}}
{{- printf "%s@%s" $repo .Values.console.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.Values.console.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/*
Replica count, with or without persistent history: a persistent console is a
StatefulSet whose replicas each have their own history volume.
*/}}
{{- define "pgtools.console.replicas" -}}
{{ .Values.console.replicas }}
{{- end -}}

{{/* The Lease whose holder delivers the daily digests. */}}
{{- define "pgtools.console.digestLease" -}}
{{- include "pgtools.console.fullname" . }}-digest
{{- end -}}

{{/* Names of the revocation ConfigMap and the generated session-key Secret. */}}
{{- define "pgtools.console.revocations" -}}
{{- .Values.console.revocations.configMapName | default (printf "%s-revocations" (include "pgtools.console.fullname" .)) -}}
{{- end -}}

{{/* The state ConfigMap the console's replicas share. */}}
{{- define "pgtools.console.state" -}}
{{- printf "%s-state" (include "pgtools.console.fullname" .) -}}
{{- end -}}

{{/* The insight pack's ConfigMap: per release when the chart creates it,
     else the name of deploy/insight-pack/insight-pack.yaml. */}}
{{- define "pgtools.insightPack.configMapName" -}}
{{- $p := .Values.insightPack -}}
{{- if $p.configMapName -}}
{{- $p.configMapName -}}
{{- else if $p.enabled -}}
{{- printf "%s-insight-pack" .Release.Name -}}
{{- else -}}
pgtoolbox-insight-pack
{{- end -}}
{{- end -}}

{{- define "pgtools.console.lifecycle" -}}
{{- .Values.console.lifecycle.configMapName | default (printf "%s-lifecycle" (include "pgtools.console.fullname" .)) -}}
{{- end -}}

{{- define "pgtools.console.sessionSecret" -}}
{{- .Values.console.auth.sessionKey.existingSecret | default (printf "%s-session" (include "pgtools.console.fullname" .)) -}}
{{- end -}}

{{/*
Cluster-scoped names must be unique across releases and namespaces: two team
consoles may well both be called "pgtoolbox-console".
*/}}
{{- define "pgtools.console.clusterRoleName" -}}
{{- printf "%s:%s" .Release.Namespace (include "pgtools.console.fullname" .) -}}
{{- end -}}

{{/* "k=v,k2=v2" from a label map, sorted. */}}
{{- define "pgtools.console.selectorString" -}}
{{- $map := . -}}
{{- $parts := list -}}
{{- range $k := keys $map | sortAlpha -}}
{{- $parts = append $parts (printf "%s=%v" $k (index $map $k)) -}}
{{- end -}}
{{- join "," $parts -}}
{{- end -}}

{{/* A Go duration ("90s", "1h", "1h30m") as whole seconds, for annotations that want a number. */}}
{{- define "pgtools.seconds" -}}
{{- $d := . | toString -}}
{{- $total := 0 -}}
{{- range $m := regexFindAll "[0-9]+[hms]" $d -1 -}}
{{- $n := int (trimSuffix (substr (sub (len $m) 1 | int) (len $m) $m) $m) -}}
{{- $u := substr (sub (len $m) 1 | int) (len $m) $m -}}
{{- if eq $u "h" }}{{ $total = add $total (mul $n 3600) }}{{ else if eq $u "m" }}{{ $total = add $total (mul $n 60) }}{{ else }}{{ $total = add $total $n }}{{ end -}}
{{- end -}}
{{- $total -}}
{{- end -}}

{{/*
Evidence agents without the operator. One agent per
evidence.agents entry; its objects are named "<release>-evidence-<cluster>".
*/}}
{{- define "pgtools.evidence.name" -}}
{{- include "pgtools.safeName" (printf "%s-evidence-%s" .root.Release.Name .agent.cluster) -}}
{{- end -}}

{{/*
Selector labels of one agent's pods. app.kubernetes.io/name is the operator's
("evidence-agent"), so the console's NetworkPolicy egress rule
(console.networkPolicy.evidenceAgent) admits both kinds of agent.
*/}}
{{- define "pgtools.evidence.selectorLabels" -}}
app.kubernetes.io/name: evidence-agent
app.kubernetes.io/instance: {{ .root.Release.Name }}
app.kubernetes.io/component: evidence-agent
pgtoolbox.fyannk.dev/cluster: {{ .agent.cluster | quote }}
{{- end -}}

{{- define "pgtools.evidence.labels" -}}
{{ include "pgtools.evidence.selectorLabels" . }}
app.kubernetes.io/version: {{ .root.Chart.AppVersion | quote }}
app.kubernetes.io/part-of: pgtoolbox
app.kubernetes.io/managed-by: {{ .root.Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .root.Chart.Name .root.Chart.Version | quote }}
{{- with .root.Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/* The labels every agent of the release shares (NetworkPolicy, Secret). */}}
{{- define "pgtools.evidence.releaseLabels" -}}
app.kubernetes.io/name: evidence-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: evidence-agent
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: pgtoolbox
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
One agent's image: the entry's image values over evidence.image. A digest wins
over a tag; global.imageRegistry replaces the registry host.
*/}}
{{- define "pgtools.evidence.image" -}}
{{- $image := merge (dict) (.agent.image | default dict) .root.Values.evidence.image -}}
{{- $repo := $image.repository -}}
{{- with .root.Values.global.imageRegistry -}}
{{- $repo = printf "%s/%s" (trimSuffix "/" .) (regexReplaceAll "^[^/]+/" $repo "") -}}
{{- end -}}
{{- if $image.digest -}}
{{- printf "%s@%s" $repo $image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo ($image.tag | default .root.Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/* The shared token Secret: evidence.token.existingSecret, else "<release>-evidence-token". */}}
{{- define "pgtools.evidence.tokenSecret" -}}
{{- .Values.evidence.token.existingSecret | default (include "pgtools.safeName" (printf "%s-evidence-token" .Release.Name)) -}}
{{- end -}}

{{/* The console's agents ConfigMap. */}}
{{- define "pgtools.evidence.agentsConfigMap" -}}
{{- include "pgtools.safeName" (printf "%s-evidence-agents" (include "pgtools.console.fullname" .)) -}}
{{- end -}}

{{/*
The agents file the console reads (EVIDENCE_AGENTS_FILE): one entry per agent,
with the URL of its Service and the files the console's mounts provide.
*/}}
{{- define "pgtools.evidence.agentsFile" -}}
{{- $root := . -}}
{{- $mode := .Values.evidence.tls.mode -}}
{{- $entries := list -}}
{{- range $agent := .Values.evidence.agents -}}
{{- $name := include "pgtools.evidence.name" (dict "root" $root "agent" $agent) -}}
{{- $entry := dict "namespace" $root.Release.Namespace "cluster" $agent.cluster "tokenFile" "/var/run/console/evidence-token/token" -}}
{{- if eq $mode "none" -}}
{{- $_ := set $entry "url" (printf "http://%s.%s.svc:%d" $name $root.Release.Namespace 8443) -}}
{{- else -}}
{{- $_ := set $entry "url" (printf "https://%s.%s.svc:%d" $name $root.Release.Namespace 8443) -}}
{{- if eq $mode "openshiftServiceCA" -}}
{{- $_ := set $entry "caFile" "/var/run/console/evidence-ca/service-ca.crt" -}}
{{- else -}}
{{- $_ := set $entry "caFile" (printf "/var/run/console/evidence-ca/%s/ca.crt" $agent.cluster) -}}
{{- end -}}
{{- end -}}
{{- $entries = append $entries $entry -}}
{{- end -}}
{{- toPrettyJson $entries -}}
{{- end -}}

{{/*
pgtools.console.operatorKinds: non-empty when the console's Role grants read
on the pgtoolbox operator's kinds (console.rbac.operatorKinds: auto, true or
false; auto follows the API server's discovery).
*/}}
{{- define "pgtools.console.operatorKinds" -}}
{{- $v := dig "operatorKinds" "auto" (.Values.console.rbac | default dict) -}}
{{- if eq (toString $v) "true" -}}true
{{- else if eq (toString $v) "auto" -}}
{{- if .Capabilities.APIVersions.Has "pgtoolbox.fyannk.dev/v1alpha1" -}}true{{- end -}}
{{- else if ne (toString $v) "false" -}}
{{- fail (printf "console.rbac.operatorKinds must be auto, true or false (got %v)" $v) -}}
{{- end -}}
{{- end -}}

{{/*
RESTORE_DRILL_SERVER_CA: the CA that signs a drill's server certificate, so
that the console can verify the drill against the admin-filled bundle. auto is
secret when a Secret is named, else source when drills run in the source's
namespace (where CloudNativePG can read the source's CA Secret), else own.
*/}}
{{- define "pgtools.console.drillServerCA" -}}
{{- $d := .Values.console.restoreDrills }}
{{- $ca := $d.serverCA | default dict }}
{{- $mode := $ca.mode | default "auto" }}
{{- if eq $mode "auto" }}
{{- if $ca.secret }}{{ $mode = "secret" }}{{- else if not $d.namespace }}{{ $mode = "source" }}{{- else }}{{ $mode = "own" }}{{- end }}
{{- end }}
{{- if and (eq $mode "source") $d.namespace }}
{{- fail "console.restoreDrills.serverCA.mode=source needs drills in the source's namespace (restoreDrills.namespace empty): CloudNativePG reads a Cluster's CA Secret in its own namespace; use mode secret with serverCA.secret" }}
{{- end }}
{{- if and $ca.secret (ne $mode "secret") }}
{{- fail (printf "console.restoreDrills.serverCA.secret is only used with serverCA.mode=secret or auto, not %s" $mode) }}
{{- end }}
{{- $mode }}
{{- end }}

{{/*
The restore-drill namespaces, as a JSON list: console.restoreDrills.namespace,
else every watched namespace. Fails on a namespace the console does not watch.
*/}}
{{- define "pgtools.console.drillNamespaces" -}}
{{- $watched := include "pgtools.console.watchNamespaces" . | fromJsonArray }}
{{- $ns := .Values.console.restoreDrills.namespace }}
{{- if $ns }}
{{- if not (has $ns $watched) }}
{{- fail (printf "console.restoreDrills.namespace %q must be one of the watched namespaces %v: the console checks and deletes a drill through its read model" $ns $watched) }}
{{- end }}
{{- list $ns | toJson }}
{{- else }}
{{- $watched | toJson }}
{{- end }}
{{- end }}
