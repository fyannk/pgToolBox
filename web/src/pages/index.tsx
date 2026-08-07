import React from 'react';
import Layout from '@theme/Layout';
import Link from '@docusaurus/Link';

export default function Home(): React.ReactElement {
  return (
    <Layout
      title="pgToolBox"
      description="One declarative access stack per CloudNativePG cluster">
      <main style={{maxWidth: 'var(--ifm-container-width)', margin: '0 auto', padding: '4rem 1rem'}}>
        <h1>pgToolBox</h1>
        <p>
          One declarative access stack per CloudNativePG cluster — an
          authentication proxy, an observation console, and embedded pgAdmin —
          with declarative user and role provisioning.
        </p>
        <p>
          <Link className="button button--primary button--lg" to="/docs/">
            Read the documentation
          </Link>
        </p>
      </main>
    </Layout>
  );
}
