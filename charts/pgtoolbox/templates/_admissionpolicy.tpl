{{- /* The (path, Secret name) pairs of a PgToolBoxConsole or PgAdmin, read from the
object named by `.` ("object" or "oldObject"). has() guards every optional
field; a pair is a map so that a Secret moved to another field is a change. */ -}}
{{- define "pgtoolbox.admission.secretRefs" -}}
{{- $o := . -}}
request.resource.resource == 'pgtoolboxconsoles' ? (
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oidc) ? [{'p': 'auth.oidc.clientSecretRef', 'n': string({{ $o }}.spec.auth.oidc.clientSecretRef.name)}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.openshift) && has({{ $o }}.spec.auth.openshift.clientSecretRef) ? [{'p': 'auth.openshift.clientSecretRef', 'n': string({{ $o }}.spec.auth.openshift.clientSecretRef.name)}] : []) +
  (has({{ $o }}.spec.prometheus) && has({{ $o }}.spec.prometheus.tokenSecretRef) ? [{'p': 'prometheus.tokenSecretRef', 'n': string({{ $o }}.spec.prometheus.tokenSecretRef.name)}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.chatWebhookSecretRef) ? [{'p': 'digest.delivery.chatWebhookSecretRef', 'n': string({{ $o }}.spec.digest.delivery.chatWebhookSecretRef.name)}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.webhookSecretRef) ? [{'p': 'digest.delivery.webhookSecretRef', 'n': string({{ $o }}.spec.digest.delivery.webhookSecretRef.name)}] : []) +
  (has({{ $o }}.spec.digest) && has({{ $o }}.spec.digest.delivery) && has({{ $o }}.spec.digest.delivery.smtp) && has({{ $o }}.spec.digest.delivery.smtp.passwordSecretRef) ? [{'p': 'digest.delivery.smtp.passwordSecretRef', 'n': string({{ $o }}.spec.digest.delivery.smtp.passwordSecretRef.name)}] : []) +
  (has({{ $o }}.spec.exposure) && has({{ $o }}.spec.exposure.tls) && has({{ $o }}.spec.exposure.tls.certificateSecretRef) ? [{'p': 'exposure.tls.certificateSecretRef', 'n': string({{ $o }}.spec.exposure.tls.certificateSecretRef.name)}] : []) +
  (has({{ $o }}.spec.image) && has({{ $o }}.spec.image.pullSecrets) ? {{ $o }}.spec.image.pullSecrets.map(s, {'p': 'image.pullSecrets', 'n': string(s.name)}) : [])
) : request.resource.resource == 'pgadmins' ? (
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.oauth2) ? [{'p': 'auth.oauth2.clientSecretRef', 'n': string({{ $o }}.spec.auth.oauth2.clientSecretRef.name)}] : []) +
  (has({{ $o }}.spec.auth) && has({{ $o }}.spec.auth.openshiftOAuthProxy) && has({{ $o }}.spec.auth.openshiftOAuthProxy.image) && has({{ $o }}.spec.auth.openshiftOAuthProxy.image.pullSecrets) ? {{ $o }}.spec.auth.openshiftOAuthProxy.image.pullSecrets.map(s, {'p': 'auth.openshiftOAuthProxy.image.pullSecrets', 'n': string(s.name)}) : []) +
  (has({{ $o }}.spec.exposure) && has({{ $o }}.spec.exposure.tls) && has({{ $o }}.spec.exposure.tls.certificateSecretRef) ? [{'p': 'exposure.tls.certificateSecretRef', 'n': string({{ $o }}.spec.exposure.tls.certificateSecretRef.name)}] : []) +
  (has({{ $o }}.spec.image) && has({{ $o }}.spec.image.pullSecrets) ? {{ $o }}.spec.image.pullSecrets.map(s, {'p': 'image.pullSecrets', 'n': string(s.name)}) : [])
) : []
{{- end }}
