import React from 'react';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
import '@testing-library/jest-dom';
import ResolutionsView from '@components/optimise-new/ResolutionsView';

const mockGetResolutions = jest.fn();
const mockGetStatusCounts = jest.fn();
const mockGetDistinct = jest.fn();

jest.mock('@api1/recommendation', () => ({
  __esModule: true,
  default: {
    getRecommendationResolution: (...args: any[]) => mockGetResolutions(...args),
    getRecommendationResolutionStatusCounts: (...args: any[]) => mockGetStatusCounts(...args),
    getDistinctResolverTypes: (...args: any[]) => mockGetDistinct(...args),
    retryRecommendationResolution: jest.fn(),
  },
}));

jest.mock('@api1/home', () => ({
  __esModule: true,
  default: { getCloudAccounts: () => Promise.resolve([]) },
}));

jest.mock('@api1/user', () => ({
  __esModule: true,
  default: { getUserPreferencesTablePageSize: () => 10 },
}));

jest.mock('next/router', () => ({
  __esModule: true,
  useRouter: () => ({ isReady: true, query: {}, pathname: '/optimise', push: jest.fn(), replace: jest.fn() }),
}));

jest.mock('@components/cloudaccount/CommandExecutionHistory', () => ({
  __esModule: true,
  default: () => <div data-testid='command-history' />,
}));

const emptyListing = {
  data: { data: { recommendation_resolution: [], recommendation_resolution_aggregate: { aggregate: { count: 0 } } } },
};

describe('ResolutionsView status cards', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mockGetResolutions.mockResolvedValue(emptyListing);
    mockGetDistinct.mockResolvedValue({ data: { data: { recommendation_resolution: [] } } });
    mockGetStatusCounts.mockResolvedValue({ Success: 1100, InProgress: 12, Failed: 7 });
  });

  const card = (testid: string) => screen.getByTestId(testid);

  it('surfaces the status split the paginated table cannot show', async () => {
    render(<ResolutionsView />);

    await waitFor(() => expect(within(card('resolutions-card-success')).getByText('1,100')).toBeInTheDocument());
    expect(within(card('resolutions-card-inprogress')).getByText('12')).toBeInTheDocument();
    expect(within(card('resolutions-card-failed')).getByText('7')).toBeInTheDocument();
  });

  it('totals every status the backend reports, not only the carded ones', async () => {
    mockGetStatusCounts.mockResolvedValue({ Success: 10, InProgress: 2, Failed: 1, Cancelled: 4 });
    render(<ResolutionsView />);

    // 17, not 13 — an unrecognised status still has to be counted somewhere.
    await waitFor(() => expect(within(card('resolutions-card-all')).getByText('17')).toBeInTheDocument());
  });

  it('filters the listing to a status when its card is clicked, and clears on a second click', async () => {
    render(<ResolutionsView />);
    await waitFor(() => expect(mockGetResolutions).toHaveBeenCalled());

    fireEvent.click(card('resolutions-card-failed'));
    await waitFor(() => expect(mockGetResolutions).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'Failed' })));
    expect(card('resolutions-card-failed')).toHaveAttribute('aria-pressed', 'true');

    fireEvent.click(card('resolutions-card-failed'));
    await waitFor(() => expect(mockGetResolutions).toHaveBeenLastCalledWith(expect.objectContaining({ status: '' })));
    expect(card('resolutions-card-all')).toHaveAttribute('aria-pressed', 'true');
  });

  it('does not re-query the split when the status filter changes', async () => {
    render(<ResolutionsView />);
    await waitFor(() => expect(mockGetStatusCounts).toHaveBeenCalledTimes(1));

    fireEvent.click(card('resolutions-card-failed'));
    await waitFor(() => expect(mockGetResolutions).toHaveBeenLastCalledWith(expect.objectContaining({ status: 'Failed' })));

    // The cards describe every status, so the selected one is not part of their
    // scope — re-querying here would also collapse them to the filtered count.
    expect(mockGetStatusCounts).toHaveBeenCalledTimes(1);
  });

  it('asks for the split without a status, so the cards never narrow to their own selection', async () => {
    render(<ResolutionsView />);

    await waitFor(() => expect(mockGetStatusCounts).toHaveBeenCalled());
    expect(mockGetStatusCounts.mock.calls[0][0]).not.toHaveProperty('status');
  });

  it('mutes a status with nothing in it rather than offering an empty filter', async () => {
    mockGetStatusCounts.mockResolvedValue({ Success: 5, InProgress: 0, Failed: 0 });
    render(<ResolutionsView />);

    await waitFor(() => expect(card('resolutions-card-failed')).toHaveAttribute('aria-disabled', 'true'));
    fireEvent.click(card('resolutions-card-failed'));
    expect(card('resolutions-card-failed')).toHaveAttribute('aria-pressed', 'false');
  });

  it('does not tell an inert card it can be clicked', async () => {
    mockGetStatusCounts.mockResolvedValue({ Success: 5, InProgress: 3, Failed: 0 });
    render(<ResolutionsView />);

    await waitFor(() => expect(card('resolutions-card-failed')).toHaveAttribute('aria-disabled', 'true'));

    const mutedInfo = screen.getByLabelText('Info about Failed');
    fireEvent.mouseOver(mutedInfo);
    expect(await screen.findByRole('tooltip')).not.toHaveTextContent('Click to filter');

    // Closed before opening the next one — two live tooltips make getByRole ambiguous.
    fireEvent.mouseLeave(mutedInfo);
    await waitFor(() => expect(screen.queryByRole('tooltip')).not.toBeInTheDocument());

    // The contrast is the point: a card that does respond still says so.
    fireEvent.mouseOver(screen.getByLabelText('Info about In Progress'));
    expect(await screen.findByRole('tooltip')).toHaveTextContent('Click to filter');
  });

  it('renders the cards rather than failing the tab when the split cannot be loaded', async () => {
    mockGetStatusCounts.mockRejectedValue(new Error('boom'));
    render(<ResolutionsView />);

    await waitFor(() => expect(within(card('resolutions-card-all')).getByText('0')).toBeInTheDocument());
  });
});
