import React from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';
import useBaseUrl from '@docusaurus/useBaseUrl';
import ThemedImage from '@theme/ThemedImage';

const capabilities = [
  {
    title: 'Kubernetes decides, not the console',
    body: 'The console signs people in, then asks the API server with a SelfSubjectAccessReview, sent with the person\'s own token, what they may see and do. Writes carry the person\'s own token, so the API server\'s audit log names them. No level, role or group is stored anywhere in the product.',
  },
  {
    title: 'One console per team, never cluster-wide',
    body: 'A console is a Deployment in a team namespace with Roles in the few namespaces it watches and no Secret verb. The reach of a compromised console is the namespaces it watches, not the whole Kubernetes cluster.',
  },
  {
    title: 'A small operator for what must persist',
    body: 'PgAdmin, one pgAdmin per team registered against many clusters; EvidenceAgent, a read-only reader of a backup repository; PgToolBoxConsole, a console declared as an object; PgAccessRequest, access that is approved and expires; PgAdminAccess, a namespace owner\'s grant to a pgAdmin. The operator never holds a database password.',
  },
  {
    title: 'Honest status, including its blind spots',
    body: 'Each fact says where it came from. A cluster the console cannot verify is shown as not verifiable, a source it cannot read is named, and a diagnostic that could not run never counts as clear.',
  },
];

export default function Home(): React.ReactElement {
  return (
    <Layout
      title="pgToolBox"
      description="A per-team console, a small operator and an evidence agent for CloudNativePG clusters, on Kubernetes and OpenShift.">
      <header className="ptb-hero">
        <div className="ptb-hero__inner">
          <img
            className="ptb-hero__mark"
            src={useBaseUrl('img/logo.svg')}
            alt=""
            width={200}
            height={200}
          />
          <div className="ptb-hero__copy">
            <h1 className="ptb-hero__title">
              <span className="pg">pg</span>ToolBox
              <span className="cursor" aria-hidden="true">
                ▍
              </span>
            </h1>
            <p className="ptb-hero__tagline">
              A console, an operator and an evidence agent for CloudNativePG.
            </p>
            <div className="ptb-hero__actions">
              <Link className="button button--primary button--lg" to="/docs/install/">
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
          pgToolBox shows the real state of your CloudNativePG clusters to the
          people who run them, one team console per namespace, and lets those
          who hold the right RBAC act on them. A small operator adds a managed
          pgAdmin per team and a read-only cross-check of the backup repository.
        </p>

        <figure className="ptb-shot">
          <Link to="/docs/console/tour/" className="ptb-shot__frame">
            <ThemedImage
              alt="The console, one Cluster's overview: overall health with where each fact was read, and the wiring of its poolers, instances, backups and storage"
              sources={{
                light: useBaseUrl('/img/screens/overview-healthy-light-1440.png'),
                dark: useBaseUrl('/img/screens/overview-healthy-dark-1440.png'),
              }}
              width={1440}
              height={3386}
            />
          </Link>
          <figcaption>
            One Cluster in the console: each fact with its source, and how the
            Cluster is wired. <Link to="/docs/console/tour/">Tour every screen</Link>.
          </figcaption>
        </figure>

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
            <strong>Scope.</strong> pgToolBox provisions access and shows status.
            It never runs SQL for you: a person's own PostgreSQL role is a
            CloudNativePG DatabaseRole created with their token, CloudNativePG
            owns the cluster, and Kubernetes RBAC decides what
            a person may see and do.
          </p>
        </div>
      </main>
    </Layout>
  );
}
