import React from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';

const capabilities = [
  {
    title: 'One stack per cluster, never shared',
    body: 'Each CloudNativePG cluster gets its own proxy, its own console, its own pgAdmin and its own credentials. Security isolation beats resource sharing, and nothing one cluster holds is reachable from another.',
  },
  {
    title: 'One authentication boundary',
    body: 'OIDC, OpenShift OAuth and local accounts, in any combination at once — so a local account is still the way in when the identity provider is down. The proxy strips any forged identity header before setting its own.',
  },
  {
    title: 'Capabilities carry their authority',
    body: 'Switching a console capability off removes the matching rules from the generated Roles. RBAC denies the operation whatever the application is told, so the flag and the permission cannot drift apart.',
  },
  {
    title: 'Nobody is locked out, and nobody is implicit',
    body: 'Every console declares its first administrator, and the operator puts that user back if it is deleted. Everyone else is granted per identity, by declaration or by a dba approving a request.',
  },
];

export default function Home(): React.ReactElement {
  return (
    <Layout
      title="pgToolBox"
      description="A Kubernetes operator that gives one CloudNativePG cluster a complete access stack: authentication proxy, observation console, and embedded pgAdmin.">
      <header className="ptb-hero">
        <div className="ptb-hero__inner">
          <img
            className="ptb-hero__mark"
            src={useBaseUrl('img/logo.png')}
            alt=""
            width={220}
            height={166}
          />
          <div className="ptb-hero__copy">
            <h1 className="ptb-hero__title">
              pg<span>ToolBox</span>
            </h1>
            <p className="ptb-hero__tagline">
              One declarative access stack per CloudNativePG cluster.
            </p>
            <div className="ptb-hero__actions">
              <Link className="button button--primary button--lg" to="/docs/tutorials/getting-started">
                Get started
              </Link>
              <Link
                className="button button--outline button--lg ptb-hero__ghost"
                to="/docs/">
                What it is
              </Link>
            </div>
          </div>
        </div>
      </header>

      <main className="ptb-main">
        <p className="ptb-lede">
          pgToolBox is a Kubernetes and OpenShift operator. One{' '}
          <code>PgConsole</code> composes an authentication proxy, the pgConsole
          observation UI, an embedded pgAdmin and an optional backup-evidence
          sidecar into a single Pod, with the Service, RBAC, NetworkPolicy and
          exposure they need — for exactly one CloudNativePG cluster.
        </p>

        <div className="ptb-grid">
          {capabilities.map((capability) => (
            <section className="ptb-card" key={capability.title}>
              <h2>{capability.title}</h2>
              <p>{capability.body}</p>
            </section>
          ))}
        </div>

        <div className="ptb-note">
          <p>
            <strong>Scope.</strong> pgToolBox provisions access, not databases.
            It never creates postgres roles and never writes to your data:
            CloudNativePG owns the cluster, and the levels pgToolBox grants —{' '}
            <code>view</code>, <code>poweruser</code>, <code>dba</code> — decide
            what a person may see and do in the console and in pgAdmin.
          </p>
        </div>
      </main>
    </Layout>
  );
}
