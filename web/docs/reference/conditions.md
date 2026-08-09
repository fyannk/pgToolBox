# Conditions

Condition types and reasons are frozen API surface: they are extended,
never renamed. Only the conditions and reasons the controllers actually
set are listed here.

## PgConsole

`Ready`, `Progressing`, `ConfigurationValid`, `RouteReady`,
`ProxyConfigReady`, `ClusterReady`, `PgAdminSynced`,
`RepositoryEvidenceReady`.

Common reasons: `AsExpected`, `Reconciling`, `ReconciliationSkipped`,
`RolloutInProgress`, `ConfigurationInvalid`,
`SecretNotFound`, `SecretKeyMissing`, `RenderFailed`, `NotAdmitted`,
`NotAccepted`, `GatewayNotFound`, `HostnameConflict`, `NotRequested`,
`SomeDegraded`, `NoneConfigured`, `ClusterNotFound`,
`PendingRollout`, `SyncFailed`, `UnsupportedCredentialMode`,
`ObjectStoreNotFound`, `ImageRequired`, `EvidenceDisabled`.

## PgToolBoxUser

`RoleReady`, `ProxySynced`, `PgAdminSynced`.

Reasons: `AsExpected`, `RoleNotFound`, `SomeDegraded`,
`ConfigurationInvalid`.

## PgToolBoxAccessRequest

`Decided`, `UserReady`.

Reasons: `Pending`, `Approved`, `Denied`, `NotRequested`,
`PgConsoleNotFound`, `RoleNotFound`, `ConfigurationInvalid`.
