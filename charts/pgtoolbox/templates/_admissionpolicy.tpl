{{- /* The (path, Secret name, destination) triples of a PgToolBoxConsole, a
PgAdmin or an EvidenceAgent, read from the object named by `.` ("object" or
"oldObject"). has() guards every optional field; a triple is a map so that a
Secret moved to another field is a change. 'd' is where the workload sends that
Secret (an issuer, a URL, a mail server, a registry), '' where the spec names
none: a kept reference with a changed destination is a new reference, checked
again. */ -}}
{{- define "pgtoolbox.admission.secretRefs" -}}
{{- $o := . -}}
request.resource.resource == 'pgtoolboxconsoles' ? (
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oidc) ? [{'p': 'auth.oidc.clientSecretRef', 'n': string({{ $o }}.spec.auth.oidc.clientSecretRef.name), 'd': string({{ $o }}.spec.auth.oidc.issuerURL)}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oidc) && has({{ $o }}.spec.auth.oidc.grantClient) ? [{'p': 'auth.oidc.grantClient.clientSecretRef', 'n': string({{ $o }}.spec.auth.oidc.grantClient.clientSecretRef.name), 'd': string({{ $o }}.spec.auth.oidc.issuerURL)}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.openshift) && has({{ $o }}.spec.auth.openshift.clientSecretRef) ? [{'p': 'auth.openshift.clientSecretRef', 'n': string({{ $o }}.spec.auth.openshift.clientSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.prometheus) && has({{ $o }}.spec.prometheus.tokenSecretRef) ? [{'p': 'prometheus.tokenSecretRef', 'n': string({{ $o }}.spec.prometheus.tokenSecretRef.name), 'd': string({{ $o }}.spec.prometheus.url)}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.chatWebhookSecretRef) ? [{'p': 'digest.delivery.chatWebhookSecretRef', 'n': string({{ $o }}.spec.digest.delivery.chatWebhookSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.webhookSecretRef) ? [{'p': 'digest.delivery.webhookSecretRef', 'n': string({{ $o }}.spec.digest.delivery.webhookSecretRef.name), 'd': ''}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.smtp) && has({{ $o }}.spec.digest.delivery.smtp.passwordSecretRef) ? [{'p': 'digest.delivery.smtp.passwordSecretRef', 'n': string({{ $o }}.spec.digest.delivery.smtp.passwordSecretRef.name), 'd': string({{ $o }}.spec.digest.delivery.smtp.address)}] : []) +
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
  (has({{ $o }}.spec.allowedSecrets) ? {{ $o }}.spec.allowedSecrets.map(s, {'p': 'allowedSecrets', 'n': string(s), 'd': ''}) : [])
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
