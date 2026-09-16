import { render } from '@testing-library/react';
import Datetime from '@shared/format/Datetime';
import { parseEpochTimestamp } from '@lib/datetime';

// The log-row dropdown renders every label of a log line, and the `timestamp`
// label is encoded differently by every provider. Some pass an ISO-8601 string
// through, which used to be divided by 1e6 into NaN — and the resulting Invalid
// Date threw `RangeError: Invalid time value` out of the tooltip's
// Intl.DateTimeFormat, taking the whole log screen down with it.
describe('log timestamp parsing', () => {
  it('reads epoch values whatever unit the provider used', () => {
    const ms = Date.UTC(2026, 8, 16, 6, 18, 1);
    expect(parseEpochTimestamp(ms)).toBe(ms);
    expect(parseEpochTimestamp(Math.floor(ms / 1000))).toBe(ms);
    expect(parseEpochTimestamp(String(ms * 1e6))).toBe(ms);
    expect(parseEpochTimestamp(ms * 1e6)).toBe(ms);
  });

  it('reports a non-epoch value instead of coercing it to a number', () => {
    expect(parseEpochTimestamp('2026-09-16T06:18:01.630729233Z')).toBeNaN();
    expect(parseEpochTimestamp('')).toBeNaN();
    expect(parseEpochTimestamp('app-dev')).toBeNaN();
  });
});

describe('Datetime', () => {
  it('renders an ISO-8601 timestamp', () => {
    const { container } = render(<Datetime value='2026-09-16T06:18:01.630729233Z' baseDate={new Date('2026-09-16T06:18:31Z')} />);
    expect(container.textContent).toContain('29s');
  });

  it('renders the empty placeholder instead of throwing on an unparseable value', () => {
    expect(() => render(<Datetime value={new Date(NaN)} emptyValue='--' />)).not.toThrow();
    const { container } = render(<Datetime value='not-a-date' emptyValue='--' />);
    expect(container.textContent).toBe('--');
  });
});
