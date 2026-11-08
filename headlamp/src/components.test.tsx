import { render, screen } from '@testing-library/react';
import { OpenInConsoleAction, OperatorPhaseSection } from './components';

const config: { consoleURL?: string } = {};

vi.mock('./config', () => ({
  store: { useConfig: () => () => config },
}));

vi.mock('@kinvolk/headlamp-plugin/lib/CommonComponents', () => ({
  SectionBox: ({ title, children }: { title: string; children: unknown }) => (
    <section aria-label={title}>{children as never}</section>
  ),
}));

const cnpg = {
  jsonData: {
    apiVersion: 'postgresql.cnpg.io/v1',
    kind: 'Cluster',
    metadata: { name: 'pg main', namespace: 'team-a' },
    status: { phase: 'Cluster in healthy state', instances: 3, readyInstances: 2 },
  },
};

beforeEach(() => {
  delete config.consoleURL;
});

describe('OpenInConsoleAction', () => {
  it('renders nothing for other kinds', () => {
    const { container } = render(
      <OpenInConsoleAction
        item={{ jsonData: { apiVersion: 'v1', kind: 'Pod', metadata: { name: 'p' } } }}
      />
    );
    expect(container.innerHTML).toBe('');
  });

  it('is disabled with an explanation when the URL is unset', () => {
    render(<OpenInConsoleAction item={cnpg} />);
    const button = screen.getByRole('button', { name: 'Open in the pgToolBox console' });
    expect(button).toHaveProperty('disabled', true);
    expect(button.getAttribute('aria-describedby')).toBe('pgtoolbox-console-action-problem');
    expect(screen.getByText('pgToolBox console URL is not configured in the plugin settings')).toBeTruthy();
  });

  it('links to the cluster page in a new tab', () => {
    config.consoleURL = 'https://console.example.com/';
    render(<OpenInConsoleAction item={cnpg} />);
    const link = screen.getByRole('link', { name: 'Open in the pgToolBox console' });
    expect(link.getAttribute('href')).toBe('https://console.example.com/c/team-a/pg%20main');
    expect(link.getAttribute('target')).toBe('_blank');
    expect(link.getAttribute('rel')).toBe('noopener noreferrer');
  });
});

describe('OperatorPhaseSection', () => {
  it('shows the reported phase in words and points to the pgToolBox console for the verdict', () => {
    config.consoleURL = 'https://console.example.com';
    render(<OperatorPhaseSection resource={cnpg} />);
    expect(screen.getByText('Phase: Cluster in healthy state')).toBeTruthy();
    expect(screen.getByText('2/3 instances ready')).toBeTruthy();
    expect(screen.getByText(/not a health verdict/)).toBeTruthy();
    expect(
      screen.getByRole('link', { name: 'See the health verdict in the pgToolBox console' }).getAttribute('href')
    ).toBe('https://console.example.com/c/team-a/pg%20main');
  });

  it('says "not reported" without a phase and names the missing setting', () => {
    render(
      <OperatorPhaseSection resource={{ jsonData: { ...cnpg.jsonData, status: undefined } }} />
    );
    expect(screen.getByText('Phase: not reported')).toBeTruthy();
    expect(screen.getByText(/pgToolBox console URL is not configured in the plugin settings/)).toBeTruthy();
  });
});
