# pgAdmin

The embedded pgAdmin is composed into the console Pod and reached at
`/pgadmin` on the console's own origin, so it crosses the same boundary as
every other request: the proxy admits it only with a session at or above
`spec.pgAdmin.accessMinLevel`.

It does not ask for credentials again. The proxy has already established
who the user is, and pgAdmin takes that identity from the
`X-Forwarded-User` header it sets — trustworthy for exactly the reason the
console's copy is, and no other: the proxy strips any client-supplied copy
before setting its own, and the generated NetworkPolicy leaves no route to
pgAdmin that bypasses the proxy.

## Server provisioning is being rebuilt

The operator used to provision a pgAdmin account and a server definition
per `PgToolBoxUser`, each carrying a postgres role materialized for that
user. That was built on a premise that does not hold: a `PgToolBoxRole`
and a `PgToolBoxUser` configure the proxy and have no postgres backing, so
there was never a per-user database identity for pgAdmin to use.

What replaces it is a shared server list built from the cluster's own
credentials — the application user, the superuser where one is enabled,
and the owners of any declared databases — visible to every session the
proxy admits. Until that lands, the console reports `PgAdminSynced` as
unknown with the reason, rather than claiming a sync it is not doing, and
a dba can add a connection by hand in the meantime.
