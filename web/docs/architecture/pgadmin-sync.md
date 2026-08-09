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

## Where the connections come from

The operator used to provision a pgAdmin account and a server definition
per `PgToolBoxUser`, each carrying a postgres role materialized for that
user. That was built on a premise that does not hold: a `PgToolBoxUser`
configures the proxy and has no postgres backing, so there was never a
per-user database identity for pgAdmin to use.

What replaced it is the set of credentials the cluster itself publishes:
the application user, the superuser where `enableSuperuserAccess` puts one
in a Secret, and every `DatabaseRole` carrying a password. Each becomes
one connection, and every pgAdmin account is given the same list.

It is a list per account rather than one shared list, because pgAdmin
strips `passfile` and the TLS file paths out of a shared server the moment
it materializes one for anyone but the owner — deliberately, so that each
user configures their own. A shared entry can therefore carry visibility
but never credentials.
