{{- /* The (path, Secret name, destination) triples of a PgToolBoxConsole, a
PgAdmin or an EvidenceAgent, read from the object named by `.` ("object" or
"oldObject"). has() guards every optional field; a triple is a map so that a
Secret moved to another field is a change. 'd' is everything that decides
where the workload sends that Secret and whom it trusts there (an issuer and
its CA ConfigMap, a URL, a mail server and its TLS mode, a registry, the
Cluster, ObjectStore and pgBackRest endpoint an agent reads), '' where the
spec names none: a kept reference with a changed destination is a new
reference, checked again. The parts of 'd' are joined by a space; only the
first may contain one (a URL), the others are names, enums, numbers and
host:port, so two different destinations never read the same. */ -}}
{{- define "pgtoolbox.admission.secretRefs" -}}
{{- $o := . -}}
request.resource.resource == 'pgtoolboxconsoles' ? (
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oidc) ? [{'p': 'auth.oidc.clientSecretRef', 'n': string({{ $o }}.spec.auth.oidc.clientSecretRef.name), 'd': string({{ $o }}.spec.auth.oidc.issuerURL) + ' ' + (has({{ $o }}.spec.auth.oidc.caConfigMapRef) ? string({{ $o }}.spec.auth.oidc.caConfigMapRef.name) : '')}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oidc) && has({{ $o }}.spec.auth.oidc.grantClient) ? [{'p': 'auth.oidc.grantClient.clientSecretRef', 'n': string({{ $o }}.spec.auth.oidc.grantClient.clientSecretRef.name), 'd': string({{ $o }}.spec.auth.oidc.issuerURL) + ' ' + (has({{ $o }}.spec.auth.oidc.caConfigMapRef) ? string({{ $o }}.spec.auth.oidc.caConfigMapRef.name) : '')}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.openshift) && has({{ $o }}.spec.auth.openshift.clientSecretRef) ? [{'p': 'auth.openshift.clientSecretRef', 'n': string({{ $o }}.spec.auth.openshift.clientSecretRef.name), 'd': (has({{ $o }}.spec.auth.openshift.caConfigMapRef) ? string({{ $o }}.spec.auth.openshift.caConfigMapRef.name) : '')}] : []) +
  (has({{ $o }}.spec.prometheus) && has({{ $o }}.spec.prometheus.tokenSecretRef) ? [{'p': 'prometheus.tokenSecretRef', 'n': string({{ $o }}.spec.prometheus.tokenSecretRef.name), 'd': string({{ $o }}.spec.prometheus.url) + ' ' + (has({{ $o }}.spec.prometheus.caConfigMapRef) ? string({{ $o }}.spec.prometheus.caConfigMapRef.name) : '')}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.chatWebhookSecretRef) ? [{'p': 'digest.delivery.chatWebhookSecretRef', 'n': string({{ $o }}.spec.digest.delivery.chatWebhookSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.webhookSecretRef) ? [{'p': 'digest.delivery.webhookSecretRef', 'n': string({{ $o }}.spec.digest.delivery.webhookSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.smtp) && has({{ $o }}.spec.digest.delivery.smtp.passwordSecretRef) ? [{'p': 'digest.delivery.smtp.passwordSecretRef', 'n': string({{ $o }}.spec.digest.delivery.smtp.passwordSecretRef.name), 'd': string({{ $o }}.spec.digest.delivery.smtp.address) + ' ' + (has({{ $o }}.spec.digest.delivery.smtp.tls) ? string({{ $o }}.spec.digest.delivery.smtp.tls) : '')}] : []) +
  (has({{ $o }}.spec.tracing) && has({{ $o }}.spec.tracing.headersSecretRef) ? [{'p': 'tracing.headersSecretRef', 'n': string({{ $o }}.spec.tracing.headersSecretRef.name), 'd': string({{ $o }}.spec.tracing.endpoint)}] : []) +
  (has({{ $o }}.spec.restoreDrills) && has({{ $o }}.spec.restoreDrills.sealKeySecretRef) ? [{'p': 'restoreDrills.sealKeySecretRef', 'n': string({{ $o }}.spec.restoreDrills.sealKeySecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.exposure) && has({{ $o }}.spec.exposure.tls) && has({{ $o }}.spec.exposure.tls.certificateSecretRef) ? [{'p': 'exposure.tls.certificateSecretRef', 'n': string({{ $o }}.spec.exposure.tls.certificateSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.image) && has({{ $o }}.spec.image.pullSecrets) ? {{ $o }}.spec.image.pullSecrets.map(s, {'p': 'image.pullSecrets', 'n': string(s.name), 'd': string({{ $o }}.spec.image.repository)}) : [])
) : request.resource.resource == 'pgadmins' ? (
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oauth2) ? [{'p': 'auth.oauth2.clientSecretRef', 'n': string({{ $o }}.spec.auth.oauth2.clientSecretRef.name), 'd': string({{ $o }}.spec.auth.oauth2.issuerURL)}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.openshiftOAuthProxy) && has({{ $o }}.spec.auth.openshiftOAuthProxy.image) && has({{ $o }}.spec.auth.openshiftOAuthProxy.image.pullSecrets) ? {{ $o }}.spec.auth.openshiftOAuthProxy.image.pullSecrets.map(s, {'p': 'auth.openshiftOAuthProxy.image.pullSecrets', 'n': string(s.name), 'd': string({{ $o }}.spec.auth.openshiftOAuthProxy.image.repository)}) : []) +
  (has({{ $o }}.spec.exposure) && has({{ $o }}.spec.exposure.tls) && has({{ $o }}.spec.exposure.tls.certificateSecretRef) ? [{'p': 'exposure.tls.certificateSecretRef', 'n': string({{ $o }}.spec.exposure.tls.certificateSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.image) && has({{ $o }}.spec.image.pullSecrets) ? {{ $o }}.spec.image.pullSecrets.map(s, {'p': 'image.pullSecrets', 'n': string(s.name), 'd': string({{ $o }}.spec.image.repository)}) : [])
) : request.resource.resource == 'evidenceagents' ? (
  (has({{ $o }}.spec.allowedSecrets) ? {{ $o }}.spec.allowedSecrets.map(s, {'p': 'allowedSecrets', 'n': string(s), 'd': string({{ $o }}.spec.clusterRef.name) + ' ' + (has({{ $o }}.spec.objectStoreRef) ? string({{ $o }}.spec.objectStoreRef.name) : '') + ' ' + (has({{ $o }}.spec.allowInsecureEndpoint) && {{ $o }}.spec.allowInsecureEndpoint ? 'insecure' : '') + ' ' + (has({{ $o }}.spec.pgbackrest) ? 'pgbackrest ' + (has({{ $o }}.spec.pgbackrest.repository) ? string({{ $o }}.spec.pgbackrest.repository) : '1') + ' ' + (has({{ $o }}.spec.pgbackrest.endpoint) ? string({{ $o }}.spec.pgbackrest.endpoint) : '') + ' ' + (has({{ $o }}.spec.pgbackrest.endpointCA) ? string({{ $o }}.spec.pgbackrest.endpointCA.name) + ' ' + string({{ $o }}.spec.pgbackrest.endpointCA.key) : '') : '')}) : [])
) : []
{{- end }}

{{- /* The credentials a PgAdmin offers of the Clusters of its own namespace:
the kinds (spec.credentials, with the CRD's defaults: application and
databaseRoles on, superuser off) and the sources (each clusterRefs name, and
"selector" for a clusterSelector, whose content is compared on its own).
crossNamespaceClusterRefs are not here: the PgAdminAccess grant of the target
namespace, created by its owner, decides those. */ -}}
{{- define "pgtoolbox.admission.credentialKinds" -}}
{{- $o := . -}}
((!has({{ $o }}.spec.credentials) || !has({{ $o }}.spec.credentials.application) || {{ $o }}.spec.credentials.application) ? ['application'] : []) +
((has({{ $o }}.spec.credentials) && has({{ $o }}.spec.credentials.superuser) && {{ $o }}.spec.credentials.superuser) ? ['superuser'] : []) +
((!has({{ $o }}.spec.credentials) || !has({{ $o }}.spec.credentials.databaseRoles) || {{ $o }}.spec.credentials.databaseRoles) ? ['databaseRoles'] : [])
{{- end }}
{{- define "pgtoolbox.admission.credentialSources" -}}
{{- $o := . -}}
(has({{ $o }}.spec.clusterRefs) ? {{ $o }}.spec.clusterRefs.map(r, string(r.name)) : []) +
(has({{ $o }}.spec.clusterSelector) ? ['selector'] : [])
{{- end }}


{{- /* The Prometheus destination of a PgToolBoxConsole's projected
ServiceAccount token, read from `.` ("object" or "oldObject"): the URL, the
CA ConfigMap and the audience; '' when no token is projected. A projected
token is the console ServiceAccount's own credential, so it is checked like a
Secret reference: whoever sends it somewhere new must be able to mint it. */ -}}
{{- define "pgtoolbox.admission.prometheusTokenTo" -}}
{{- $o := . -}}
request.resource.resource == 'pgtoolboxconsoles' && has({{ $o }}.spec.prometheus) && has({{ $o }}.spec.prometheus.projectedToken) && {{ $o }}.spec.prometheus.projectedToken ?
  string({{ $o }}.spec.prometheus.url) + ' ' + (has({{ $o }}.spec.prometheus.caConfigMapRef) ? string({{ $o }}.spec.prometheus.caConfigMapRef.name) : '') + ' ' + (has({{ $o }}.spec.prometheus.tokenAudience) ? string({{ $o }}.spec.prometheus.tokenAudience) : '') : ''
{{- end }}

{{- /* Whether an UPDATE of a PgAdmin leaves who may sign in, who becomes
pgAdmin administrator, and how long a revoked person keeps a session, as it
was: every field of spec.auth but the cosmetic auth.oauth2.displayName, and
spec.admins. The OpenShift proxy's sessionMaxAge (1h when unset) may only go
down: a longer session keeps a person whose access was revoked in pgAdmin
longer, so raising it is checked like widening the audience.
TestPgAdminAudienceFieldsClassified
in internal/operator/admissionpolicy fails when a field of spec.auth is
neither compared here nor listed as cosmetic. */ -}}
{{- define "pgtoolbox.admission.pgadminAudienceKept" -}}
oldObject != null &&
has(object.spec.auth.oauth2) == has(oldObject.spec.auth.oauth2) &&
has(object.spec.auth.openshiftOAuthProxy) == has(oldObject.spec.auth.openshiftOAuthProxy) &&
(!has(object.spec.auth.oauth2) || (
  object.spec.auth.oauth2.issuerURL == oldObject.spec.auth.oauth2.issuerURL &&
  object.spec.auth.oauth2.clientID == oldObject.spec.auth.oauth2.clientID &&
  object.spec.auth.oauth2.clientSecretRef == oldObject.spec.auth.oauth2.clientSecretRef &&
  object.spec.auth.oauth2.requiredClaims == oldObject.spec.auth.oauth2.requiredClaims &&
  (has(object.spec.auth.oauth2.usernameClaim) ? object.spec.auth.oauth2.usernameClaim : '') == (has(oldObject.spec.auth.oauth2.usernameClaim) ? oldObject.spec.auth.oauth2.usernameClaim : '') &&
  (has(object.spec.auth.oauth2.scopes) ? object.spec.auth.oauth2.scopes : []) == (has(oldObject.spec.auth.oauth2.scopes) ? oldObject.spec.auth.oauth2.scopes : []))) &&
(!has(object.spec.auth.openshiftOAuthProxy) || (
  has(object.spec.auth.openshiftOAuthProxy.image) == has(oldObject.spec.auth.openshiftOAuthProxy.image) &&
  (!has(object.spec.auth.openshiftOAuthProxy.image) || object.spec.auth.openshiftOAuthProxy.image == oldObject.spec.auth.openshiftOAuthProxy.image) &&
  duration(has(object.spec.auth.openshiftOAuthProxy.sessionMaxAge) ? string(object.spec.auth.openshiftOAuthProxy.sessionMaxAge) : '1h') <= duration(has(oldObject.spec.auth.openshiftOAuthProxy.sessionMaxAge) ? string(oldObject.spec.auth.openshiftOAuthProxy.sessionMaxAge) : '1h'))) &&
(has(object.spec.admins) ? object.spec.admins : []) == (has(oldObject.spec.admins) ? oldObject.spec.admins : [])
{{- end }}

{{- /* Whether the annotation key `.` (a CEL expression) is one an ingress
controller reads as configuration that changes who reaches the workload or
where its traffic goes: configuration snippets of any controller (keys that
contain "snippet": ingress-nginx's configuration-, server-, auth- and
stream-snippet, NGINX Inc's server- and location-snippets, HAProxy's
backend- and frontend-config-snippet), ingress-nginx's external and basic
authentication (auth-*), its global-authentication opt-out and satisfy,
request mirroring (mirror-*) and upstream client certificate
(proxy-ssl-secret); haproxy-ingress's raw configuration (config-*) and auth-*;
Traefik's router middlewares; Kong's plugins. The list is documented in
docs/site/docs/security.md; a controller's other annotations (timeouts,
body size, affinity, rewrite) pass. */ -}}
{{- define "pgtoolbox.admission.exposureAnnotationDenied" -}}
({{ . }}.contains('snippet') ||
{{ . }}.startsWith('nginx.ingress.kubernetes.io/auth-') ||
{{ . }} == 'nginx.ingress.kubernetes.io/enable-global-auth' ||
{{ . }} == 'nginx.ingress.kubernetes.io/satisfy' ||
{{ . }}.startsWith('nginx.ingress.kubernetes.io/mirror-') ||
{{ . }} == 'nginx.ingress.kubernetes.io/proxy-ssl-secret' ||
{{ . }}.startsWith('haproxy-ingress.github.io/config-') ||
{{ . }}.startsWith('haproxy-ingress.github.io/auth-') ||
{{ . }}.startsWith('haproxy.org/auth-') ||
{{ . }} == 'traefik.ingress.kubernetes.io/router.middlewares' ||
{{ . }} == 'konghq.com/plugins')
{{- end }}

{{- /* Whether every denied annotation (pgtoolbox.admission.exposureAnnotationDenied)
that object puts on its exposure object (spec.exposure.annotations, and the
annotations of the metadataPropagation rules for All or Exposure) was there,
with the same value, before, on an object exposed the same way. A
clusterIP object renders no exposure object. */ -}}
{{- define "pgtoolbox.admission.exposureAnnotationsKept" -}}
variables.exposureType == 'clusterIP' || (
(!has(object.spec.exposure.annotations) || object.spec.exposure.annotations.all(k,
  !{{ include "pgtoolbox.admission.exposureAnnotationDenied" "k" }} ||
  (variables.oldExposureType == variables.exposureType && has(oldObject.spec.exposure.annotations) &&
   k in oldObject.spec.exposure.annotations && oldObject.spec.exposure.annotations[k] == object.spec.exposure.annotations[k]))) &&
(!has(object.spec.metadataPropagation) || object.spec.metadataPropagation.all(r,
  !(r.target == 'All' || r.target == 'Exposure') || !has(r.annotations) || r.annotations.all(k,
    !{{ include "pgtoolbox.admission.exposureAnnotationDenied" "k" }} ||
    (variables.oldExposureType == variables.exposureType && has(oldObject.spec.metadataPropagation) &&
     oldObject.spec.metadataPropagation.exists(o, (o.target == 'All' || o.target == 'Exposure') && has(o.annotations) &&
       k in o.annotations && o.annotations[k] == r.annotations[k])))))
)
{{- end }}
