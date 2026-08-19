import React from 'react';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import '@testing-library/jest-dom';
import SecurityView from '@components/optimise-new/SecurityView';

const mockGetCloudAccounts = jest.fn();
jest.mock('@api1/home', () => ({
  __esModule: true,
  default: {
    getCloudAccounts: (...args: any[]) => mockGetCloudAccounts(...args),
  },
}));

const lastProps: Record<string, any> = { imageScan: null, cis: null, vm: null };

jest.mock('@components/recommendations/KubernetesSecurity', () => ({
  __esModule: true,
  default: (props: any) => {
    lastProps.imageScan = props;
    return (
      <div data-testid='child-image-scan'>
        {props.leadingFilters}
        {JSON.stringify(props.kubernetes.id)}
      </div>
    );
  },
}));

jest.mock('@components/recommendations/KubernetesCisSecurityV2', () => ({
  __esModule: true,
  default: (props: any) => {
    lastProps.cis = props;
    return (
      <div data-testid='child-cis'>
        {props.leadingFilters}
        {JSON.stringify(props.kubernetes.id)}
      </div>
    );
  },
}));

jest.mock('@components/vm/VmVulnerabilities', () => ({
  __esModule: true,
  default: (props: any) => {
    lastProps.vm = props;
    return (
      <div data-testid='child-vm'>
        {props.leadingFilters}
        {JSON.stringify(props.accountId)}
      </div>
    );
  },
}));

jest.mock('@ui/FilterDropdown', () => ({
  __esModule: true,
  default: ({ label, options = [], onSelect }: any) => (
    <button data-testid={`filter-${label}`} onClick={() => onSelect?.({}, options.slice(0, 1))}>
      {label}
      <span data-testid={`filter-${label}-count`}>{options.length}</span>
    </button>
  ),
}));

jest.mock('@shared/icons/CloudIcon', () => ({ __esModule: true, default: () => <span /> }));
jest.mock('@ui/Skeleton', () => ({ __esModule: true, Skeleton: () => <div data-testid='skeleton' /> }));
jest.mock('@utils/colors');

const ACCOUNTS = [
  { id: 'k8s-1', account_name: 'prod-cluster', cloud_provider: 'AWS' },
  { id: 'k8s-2', account_name: 'staging-cluster', cloud_provider: 'GCP' },
  { id: 'vm-1', account_name: 'vm-fleet', cloud_provider: 'SelfHosted' },
];

beforeEach(() => {
  jest.clearAllMocks();
  lastProps.imageScan = null;
  lastProps.cis = null;
  lastProps.vm = null;
  mockGetCloudAccounts.mockResolvedValue(ACCOUNTS);
});

describe('SecurityView', () => {
  it('scopes the Image Scan tab to every non-VM account when nothing is picked', async () => {
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('child-image-scan')).toBeInTheDocument());
    expect(lastProps.imageScan.kubernetes.id).toEqual(['k8s-1', 'k8s-2']);
  });

  it('never sends an unscoped query — the account list is always explicit', async () => {
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('child-image-scan')).toBeInTheDocument());
    const scope = lastProps.imageScan.kubernetes.id;
    expect(Array.isArray(scope)).toBe(true);
    expect(scope.length).toBeGreaterThan(0);
  });

  it('passes an id → display-name map so rows can name their cluster', async () => {
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('child-image-scan')).toBeInTheDocument());
    expect(lastProps.imageScan.accountsById).toEqual({
      'k8s-1': 'prod-cluster',
      'k8s-2': 'staging-cluster',
      'vm-1': 'vm-fleet',
    });
  });

  it('scopes the VM tab to SelfHosted accounts only', async () => {
    render(<SecurityView subTab={2} />);
    await waitFor(() => expect(screen.getByTestId('child-vm')).toBeInTheDocument());
    expect(lastProps.vm.accountId).toEqual(['vm-1']);
  });

  it('offers only the active tab’s accounts in the picker', async () => {
    const { rerender } = render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('filter-Account-count')).toHaveTextContent('2'));
    rerender(<SecurityView subTab={2} />);
    await waitFor(() => expect(screen.getByTestId('filter-Account-count')).toHaveTextContent('1'));
  });

  it('narrows the scope to the picked account', async () => {
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('filter-Account')).toBeInTheDocument());
    fireEvent.click(screen.getByTestId('filter-Account'));
    await waitFor(() => expect(lastProps.imageScan.kubernetes.id).toEqual(['k8s-1']));
  });

  it('renders the account picker inside the view toolbar, not above it', async () => {
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('child-image-scan')).toBeInTheDocument());
    expect(lastProps.imageScan.leadingFilters).toBeTruthy();
  });

  it('keeps the scope object identity stable across re-renders so children do not refetch', async () => {
    const { rerender } = render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('child-image-scan')).toBeInTheDocument());
    const first = lastProps.imageScan.kubernetes;
    rerender(<SecurityView subTab={0} />);
    expect(lastProps.imageScan.kubernetes).toBe(first);
  });

  it('shows an empty state instead of querying when the tenant has no matching account', async () => {
    mockGetCloudAccounts.mockResolvedValue([{ id: 'vm-1', account_name: 'vm-fleet', cloud_provider: 'SelfHosted' }]);
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('optimise-security-empty')).toBeInTheDocument());
    expect(screen.queryByTestId('child-image-scan')).not.toBeInTheDocument();
  });

  it('survives getCloudAccounts resolving to a non-array error value', async () => {
    mockGetCloudAccounts.mockResolvedValue(new Error('boom'));
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('optimise-security-empty')).toBeInTheDocument());
  });

  it('shows the empty state rather than a permanent skeleton if the accounts fetch rejects', async () => {
    const consoleError = jest.spyOn(console, 'error').mockImplementation(() => {});
    mockGetCloudAccounts.mockRejectedValue(new Error('network down'));
    render(<SecurityView subTab={0} />);
    await waitFor(() => expect(screen.getByTestId('optimise-security-empty')).toBeInTheDocument());
    expect(screen.queryByTestId('optimise-security-loading')).not.toBeInTheDocument();
    consoleError.mockRestore();
  });
});
