import React from 'react';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import '@testing-library/jest-dom';
import ConfigRuleFindings from '@components/optimise-new/ConfigRuleFindings';

const mockGet = jest.fn();

jest.mock('@api1/recommendation', () => ({
  __esModule: true,
  default: {
    getK8sRecommendation: (...args: any[]) => mockGet(...args),
  },
}));

const page = (count: number) => ({
  data: {
    recommendation: Array.from({ length: 5 }, (_, i) => ({
      id: `rec-${i}`,
      severity: 'High',
      resource_name: `resource-${i}`,
      account_id: 'acct-a',
      updated_at: '2026-08-20T00:00:00Z',
    })),
    recommendation_aggregate: { aggregate: { count } },
  },
});

describe('ConfigRuleFindings', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mockGet.mockResolvedValue(page(1233));
  });

  const renderView = (props: Partial<React.ComponentProps<typeof ConfigRuleFindings>> = {}) =>
    render(
      <ConfigRuleFindings ruleName='aws_lambda_tracing' accountId={['acct-a']} status={['Open']} onSelectRecommendation={jest.fn()} {...props} />
    );

  it('asks only for the check it is expanded under', async () => {
    renderView();

    await waitFor(() => expect(mockGet).toHaveBeenCalled());
    const query = mockGet.mock.calls[0][0];
    expect(query.category).toBe('Configuration');
    expect(query.ruleName).toEqual(['aws_lambda_tracing']);
    expect(query.offset).toBe(0);
  });

  it('pages through the findings rather than capping them', async () => {
    renderView();

    await waitFor(() => expect(screen.getByText('resource-0')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /page 3/i }));

    await waitFor(() => expect(mockGet).toHaveBeenCalledTimes(2));
    expect(mockGet.mock.calls[1][0].offset).toBe(10);
  });

  it('returns to the first page when the filters narrow', async () => {
    const { rerender } = renderView();

    await waitFor(() => expect(screen.getByText('resource-0')).toBeInTheDocument());
    fireEvent.click(screen.getByRole('button', { name: /page 3/i }));
    await waitFor(() => expect(mockGet.mock.calls[1][0].offset).toBe(10));

    // Narrowing shrinks the result set — holding the old offset would land past
    // the end and read as "no findings".
    rerender(
      <ConfigRuleFindings
        ruleName='aws_lambda_tracing'
        accountId={['acct-a']}
        status={['Open']}
        severity={['Critical']}
        onSelectRecommendation={jest.fn()}
      />
    );

    await waitFor(() => {
      const latest = mockGet.mock.calls[mockGet.mock.calls.length - 1][0];
      expect(latest.offset).toBe(0);
      expect(latest.severity).toEqual(['Critical']);
    });
  });

  it('opens the individual finding that was clicked', async () => {
    const onSelectRecommendation = jest.fn();
    renderView({ onSelectRecommendation });

    await waitFor(() => expect(screen.getByText('resource-2')).toBeInTheDocument());
    fireEvent.click(screen.getByText('resource-2'));

    expect(onSelectRecommendation).toHaveBeenCalledWith(expect.objectContaining({ id: 'rec-2' }));
  });
});
