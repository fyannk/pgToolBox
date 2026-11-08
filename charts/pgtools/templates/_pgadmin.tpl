{{/*
pgAdmin without the operator: the helpers of templates/pgadmin/.
What the operator's PgAdmin reconciler renders in Go (internal/operator/pgadmin)
is rendered here statically; internal/operator/pgadmin/chart_parity_test.go
compares the two, so a setting changed on one side and not the other fails.
*/}}

{{/*
pgAdmin's object name: pgadmin.fullnameOverride, else the release name when it
already says "pgadmin", else "<release>-pgadmin".
*/}}
{{- define "pgtools.pgadmin.fullname" -}}
{{- if .Values.pgadmin.fullnameOverride -}}
{{- .Values.pgadmin.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else if contains "pgadmin" .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-pgadmin" .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "pgtools.pgadmin.selectorLabels" -}}
app.kubernetes.io/name: pgadmin
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: pgadmin
{{- end -}}

{{- define "pgtools.pgadmin.labels" -}}
{{ include "pgtools.pgadmin.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/part-of: pgtoolbox
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- with .Values.commonLabels }}
{{ toYaml . }}
{{- end }}
{{- end -}}

{{/*
An image reference from {repository, tag, digest}: a digest wins over the tag
(a tag written in values is kept beside it, tag@digest, as the operator's
compiled-in pgAdmin default is; the default tag is not); global.imageRegistry
replaces the registry host. Call with
(dict "image" <image values> "tag" <default tag> "root" $).
*/}}
{{- define "pgtools.pgadmin.imageRef" -}}
{{- $repo := .image.repository -}}
{{- with .root.Values.global.imageRegistry -}}
{{- $repo = printf "%s/%s" (trimSuffix "/" .) (regexReplaceAll "^[^/]+/" $repo "") -}}
{{- end -}}
{{- if and .image.tag .image.digest -}}
{{- printf "%s:%s@%s" $repo .image.tag .image.digest -}}
{{- else if .image.digest -}}
{{- printf "%s@%s" $repo .image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (.image.tag | default .tag) -}}
{{- end -}}
{{- end -}}

{{/* pgAdmin's image (the hardened build by default, the operator's compiled-in default). */}}
{{- define "pgtools.pgadmin.image" -}}
{{- include "pgtools.pgadmin.imageRef" (dict "image" .Values.pgadmin.image "tag" "" "root" .) -}}
{{- end -}}

{{/* The pgToolBox image whose binary carries passfile-sync and the registration (tag: appVersion). */}}
{{- define "pgtools.pgadmin.toolsImage" -}}
{{- include "pgtools.pgadmin.imageRef" (dict "image" .Values.pgadmin.tools.image "tag" .Chart.AppVersion "root" .) -}}
{{- end -}}

{{/* The OpenShift oauth-proxy image. */}}
{{- define "pgtools.pgadmin.proxyImage" -}}
{{- include "pgtools.pgadmin.imageRef" (dict "image" .Values.pgadmin.auth.openshift.image "tag" "" "root" .) -}}
{{- end -}}

{{/*
A string as a Python literal for pgAdmin's PGADMIN_CONFIG_* values. The schema
admits only printable ASCII without quotes or backslashes in every value that
reaches one, so single quotes are the whole escaping (the operator's pyString
gives the same text for such a value; the parity test holds them together).
*/}}
{{- define "pgtools.pgadmin.py" -}}
{{- if regexMatch "^[ -~]*$" . -}}
{{- if or (contains "'" .) (contains "\\" .) -}}
{{- fail (printf "pgadmin: %q cannot be written into pgAdmin's configuration (quote or backslash)" .) -}}
{{- end -}}
{{- printf "'%s'" . -}}
{{- else -}}
{{- fail (printf "pgadmin: %q cannot be written into pgAdmin's configuration (printable ASCII only)" .) -}}
{{- end -}}
{{- end -}}

{{/* pgAdmin's authentication source of the people signing in. */}}
{{- define "pgtools.pgadmin.authSource" -}}
{{- if eq .Values.pgadmin.auth.provider "openshift" -}}webserver{{- else -}}oauth2{{- end -}}
{{- end -}}

{{/*
OAUTH2_CONFIG, as the operator's oauth2Config renders it: the client secret is
read from the mounted file when pgAdmin loads its configuration; required claims
sorted by name.
*/}}
{{- define "pgtools.pgadmin.oauth2Config" -}}
{{- $o := .Values.pgadmin.auth.oidc -}}
{{- $issuer := trimSuffix "/" $o.issuerURL -}}
{{- $claims := list -}}
{{- range $key := keys $o.requiredClaims | sortAlpha -}}
{{- $values := list -}}
{{- range $v := index $o.requiredClaims $key -}}
{{- $values = append $values (include "pgtools.pgadmin.py" $v) -}}
{{- end -}}
{{- $claims = append $claims (printf "%s: [%s]" (include "pgtools.pgadmin.py" $key) (join ", " $values)) -}}
{{- end -}}
{{- $fields := list
  (list "OAUTH2_NAME" (include "pgtools.pgadmin.py" "pgtoolbox"))
  (list "OAUTH2_DISPLAY_NAME" (include "pgtools.pgadmin.py" ($o.displayName | default "Single sign-on")))
  (list "OAUTH2_CLIENT_ID" (include "pgtools.pgadmin.py" $o.clientID))
  (list "OAUTH2_CLIENT_SECRET" "open('/run/pgadmin/oauth2/client-secret').read().strip()")
  (list "OAUTH2_SERVER_METADATA_URL" (include "pgtools.pgadmin.py" (printf "%s/.well-known/openid-configuration" $issuer)))
  (list "OAUTH2_API_BASE_URL" (include "pgtools.pgadmin.py" (printf "%s/" $issuer)))
  (list "OAUTH2_SCOPE" (include "pgtools.pgadmin.py" (join " " ($o.scopes | default (list "openid" "email" "profile")))))
  (list "OAUTH2_USERNAME_CLAIM" (include "pgtools.pgadmin.py" ($o.usernameClaim | default "email")))
  (list "OAUTH2_ADDITIONAL_CLAIMS" (printf "{%s}" (join ", " $claims)))
  (list "OAUTH2_SSL_CERT_VERIFICATION" "True") -}}
{{- $parts := list -}}
{{- range $f := $fields -}}
{{- $parts = append $parts (printf "%s: %s" (include "pgtools.pgadmin.py" (index $f 0)) (index $f 1)) -}}
{{- end -}}
{{- printf "[{%s}]" (join ", " $parts) -}}
{{- end -}}

{{/*
The display name of one declared server, as the sidecar and the registration
derive it (internal/operator/pgadmin/declared.Server.DisplayName).
*/}}
{{- define "pgtools.pgadmin.serverName" -}}
{{- $s := . -}}
{{- if $s.name -}}
{{- $s.name -}}
{{- else -}}
{{- $who := $s.user -}}
{{- if not $who -}}
{{- if kindIs "string" $s.credentials -}}
{{- $who = $s.credentials -}}
{{- else if $s.credentials.databaseRole -}}
{{- $who = $s.credentials.databaseRole.name -}}
{{- else -}}
{{- $who = keys $s.credentials | first -}}
{{- end -}}
{{- end -}}
{{- printf "%s %s (%s)" $s.cluster $s.endpoint $who -}}
{{- end -}}
{{- end -}}

{{/*
The declared server list the ConfigMap carries, checked here as far as a
template can (the schema checks each entry's shape; the Pod's tools parse the
list again strictly): every entry names its credential source, secret and none
name a user and a database, names are unique, a server names caSecret or
caConfigMap but not both, and every server of one Cluster names the same
caConfigMap (the Pod keeps one CA file per Cluster).
*/}}
{{- define "pgtools.pgadmin.servers" -}}
{{- $seen := dict -}}
{{- $caOf := dict -}}
{{- range $i, $s := .Values.pgadmin.servers -}}
{{- if not $s.credentials -}}
{{- fail (printf "pgadmin.servers[%d]: credentials is required (application, superuser, databaseRole, secret or none): there is no default" $i) -}}
{{- end -}}
{{- if and $s.caSecret $s.caConfigMap -}}
{{- fail (printf "pgadmin.servers[%d]: caSecret and caConfigMap are exclusive: name one" $i) -}}
{{- end -}}
{{- $ca := $s.caConfigMap | default "" -}}
{{- if and (hasKey $caOf $s.cluster) (ne (get $caOf $s.cluster) $ca) -}}
{{- fail (printf "pgadmin.servers[%d]: every server of Cluster %s must name the same caConfigMap" $i $s.cluster) -}}
{{- end -}}
{{- $_ := set $caOf $s.cluster $ca -}}
{{- $plain := or (and (kindIs "string" $s.credentials) (eq $s.credentials "none")) (and (kindIs "map" $s.credentials) (hasKey $s.credentials "secret")) -}}
{{- if and $plain (not (and $s.user $s.database)) -}}
{{- fail (printf "pgadmin.servers[%d]: credentials secret and none need user and database" $i) -}}
{{- end -}}
{{- $name := include "pgtools.pgadmin.serverName" $s -}}
{{- if hasKey $seen $name -}}
{{- fail (printf "pgadmin.servers[%d]: another server is already named %q; set name" $i $name) -}}
{{- end -}}
{{- $_ := set $seen $name true -}}
{{- end -}}
{{- dict "version" 1 "servers" (.Values.pgadmin.servers | default list) | toJson -}}
{{- end -}}

{{/*
The objects the declared list names, each kind sorted and unique, as JSON
{clusters, databaseRoles, poolers, secrets}: exactly what the Pod's Role grants
by name (declared.Document.Names). The CA Secret of a server is caSecret, else
none with caConfigMap (mounted, not read through the API), else the derived
<cluster>-ca (deprecated; NOTES names each one).
*/}}
{{- define "pgtools.pgadmin.names" -}}
{{- $clusters := list -}}
{{- $roles := list -}}
{{- $poolers := list -}}
{{- $secrets := list -}}
{{- range $s := .Values.pgadmin.servers -}}
{{- $clusters = append $clusters $s.cluster -}}
{{- if not $s.caConfigMap -}}
{{- $secrets = append $secrets ($s.caSecret | default (printf "%s-ca" $s.cluster)) -}}
{{- end -}}
{{- if kindIs "map" $s.credentials -}}
{{- range $kind, $ref := $s.credentials -}}
{{- if eq $kind "secret" -}}
{{- $secrets = append $secrets $ref -}}
{{- else -}}
{{- $secrets = append $secrets $ref.secret -}}
{{- end -}}
{{- if eq $kind "databaseRole" -}}
{{- $roles = append $roles $ref.name -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if hasPrefix "pooler:" $s.endpoint -}}
{{- $poolers = append $poolers (trimPrefix "pooler:" $s.endpoint) -}}
{{- end -}}
{{- end -}}
{{- dict "clusters" ($clusters | uniq | sortAlpha) "databaseRoles" ($roles | uniq | sortAlpha) "poolers" ($poolers | uniq | sortAlpha) "secrets" ($secrets | uniq | sortAlpha) | toJson -}}
{{- end -}}

{{/*
The caConfigMaps the declared list names, sorted and unique, as a JSON array
(declared.Document.CAConfigMaps): each is projected into passfile-sync at
<pgtools.pgadmin.caDir>/<name>/ca.crt.
*/}}
{{- define "pgtools.pgadmin.caConfigMaps" -}}
{{- $out := list -}}
{{- range $s := .Values.pgadmin.servers -}}
{{- if $s.caConfigMap -}}
{{- $out = append $out $s.caConfigMap -}}
{{- end -}}
{{- end -}}
{{- $out | uniq | sortAlpha | toJson -}}
{{- end -}}

{{/* Where passfile-sync finds the caConfigMaps (declared.CAConfigMapFile). */}}
{{- define "pgtools.pgadmin.caDir" -}}/run/pgtoolbox/ca{{- end -}}

{{/*
The CA Secrets the Role grants only because a server sets neither caSecret
nor caConfigMap: the derived <cluster>-ca, deprecated (values do not name it,
and CloudNativePG's own <cluster>-ca also holds ca.key). Sorted, unique, JSON.
*/}}
{{- define "pgtools.pgadmin.derivedCASecrets" -}}
{{- $out := list -}}
{{- range $s := .Values.pgadmin.servers -}}
{{- if not (or $s.caSecret $s.caConfigMap) -}}
{{- $out = append $out (printf "%s-ca" $s.cluster) -}}
{{- end -}}
{{- end -}}
{{- $out | uniq | sortAlpha | toJson -}}
{{- end -}}

{{/* Names of the generated Secrets. */}}
{{- define "pgtools.pgadmin.bootstrapSecret" -}}
{{- printf "%s-bootstrap" (include "pgtools.pgadmin.fullname" .) -}}
{{- end -}}

{{- define "pgtools.pgadmin.cookieSecret" -}}
{{- .Values.pgadmin.auth.openshift.cookieSecret.existingSecret | default (printf "%s-proxy-cookie" (include "pgtools.pgadmin.fullname" .)) -}}
{{- end -}}
