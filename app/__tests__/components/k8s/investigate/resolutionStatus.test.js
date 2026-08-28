import { describeResolution, describeActionKind, describeActionEffect, RESOLUTION_SLOW_AFTER_MS } from '@components/k8s/investigate/resolutionStatus';

// The event page previously rendered the raw enum ("LAST DEPLOYMENT CHANGE: IN PROGRESS") and threw
// away resolver and error, so an operator could not tell who acted or why it failed.
describe('describeResolution', () => {
  const NOW = new Date('2026-08-28T10:00:00Z').getTime();
  const at = (msAgo) => new Date(NOW - msAgo).toISOString();

  it('maps stored statuses onto one vocabulary', () => {
    expect(describeResolution({ status: 'Success' }, NOW)).toMatchObject({ label: 'Done', tone: 'success' });
    expect(describeResolution({ status: 'Failed' }, NOW)).toMatchObject({ label: 'Failed', tone: 'critical' });
    expect(describeResolution({ status: 'InProgress' }, NOW)).toMatchObject({ label: 'Running', tone: 'info' });
  });

  // "Configuring" is a real stored value, and future writers may add more. None of them should reach
  // the screen as a bare enum.
  it('treats any non-terminal status as running', () => {
    expect(describeResolution({ status: 'Configuring' }, NOW)).toMatchObject({ label: 'Running', tone: 'info' });
    expect(describeResolution({ status: 'SomethingNew' }, NOW)).toMatchObject({ label: 'Running', tone: 'info' });
  });

  describe('actor', () => {
    it('names the person for a user-driven run', () => {
      expect(describeResolution({ status: 'Success', resolver_type: 'User', resolver_display_name: 'Mayank' }, NOW).actor).toBe('Mayank');
    });

    it('falls back when the display name is missing', () => {
      expect(describeResolution({ status: 'Success', resolver_type: 'User' }, NOW).actor).toBe('a user');
    });

    // Nubi and automations act without a person, and the type is the useful label there.
    it('names the system for an autonomous run', () => {
      expect(describeResolution({ status: 'Success', resolver_type: 'NBLLM' }, NOW).actor).toBe('NBLLM');
      expect(describeResolution({ status: 'InProgress', resolver_type: 'AutoRunbook' }, NOW).actor).toBe('AutoRunbook');
    });
  });

  describe('slow runs', () => {
    // Retry is offered on Failed only, so a run that never reports back would otherwise leave the
    // user with nothing to do and nothing to read.
    it('flags a run still going past the threshold', () => {
      const r = { status: 'InProgress', updated_at: at(RESOLUTION_SLOW_AFTER_MS + 1000) };
      expect(describeResolution(r, NOW).isSlow).toBe(true);
    });

    it('does not flag a run inside the threshold', () => {
      const r = { status: 'InProgress', updated_at: at(RESOLUTION_SLOW_AFTER_MS - 1000) };
      expect(describeResolution(r, NOW).isSlow).toBe(false);
    });

    it('never flags a finished run, however old', () => {
      const old = at(RESOLUTION_SLOW_AFTER_MS * 100);
      expect(describeResolution({ status: 'Success', updated_at: old }, NOW).isSlow).toBe(false);
      expect(describeResolution({ status: 'Failed', updated_at: old }, NOW).isSlow).toBe(false);
    });

    it('does not flag when there is no usable timestamp', () => {
      expect(describeResolution({ status: 'InProgress' }, NOW).isSlow).toBe(false);
      expect(describeResolution({ status: 'InProgress', updated_at: 'not-a-date' }, NOW).isSlow).toBe(false);
    });

    it('falls back to created_at when updated_at is absent', () => {
      const r = { status: 'InProgress', created_at: at(RESOLUTION_SLOW_AFTER_MS + 1000) };
      expect(describeResolution(r, NOW).isSlow).toBe(true);
    });
  });

  it('survives a missing resolution', () => {
    expect(describeResolution(undefined, NOW)).toMatchObject({ label: 'Running', isSlow: false });
  });
});

// A fix removes the cause; a mitigation only restores service. The remediation panel already tells
// operators this about Nubi's actions — the card actions should not be silent about it.
describe('describeActionKind', () => {
  it('classifies the actions we can classify honestly', () => {
    expect(describeActionKind('LastDeploymentCard')).toMatchObject({ kind: 'fix', text: 'Fix', tone: 'success' });
    expect(describeActionKind('MemoryAllocationCard')).toMatchObject({ kind: 'fix', tone: 'success' });
  });

  // An unlabelled action is honest; a wrongly labelled one tells the operator the cause is gone
  // when it may not be.
  it('returns nothing rather than guessing', () => {
    expect(describeActionKind('AskAiCard')).toBeNull();
    expect(describeActionKind('SomeFutureCard')).toBeNull();
    expect(describeActionKind(undefined)).toBeNull();
  });

  it('explains the difference in words, not just colour', () => {
    expect(describeActionKind('LastDeploymentCard').help).toMatch(/removes the cause/i);
  });
});

// The confirmation dialog shows a diff and a Submit button, and never says the change replaces every
// running pod. The effect line is where that is stated.
describe('describeActionEffect', () => {
  it('names the consequence, not just the change', () => {
    expect(describeActionEffect('LastDeploymentCard')).toMatch(/rolling restart/i);
    expect(describeActionEffect('MemoryAllocationCard')).toMatch(/rolling restart/i);
  });

  it('says whether the action can be undone', () => {
    expect(describeActionEffect('LastDeploymentCard')).toMatch(/reversible/i);
  });

  // A wrong effect description is worse than none: it would be approved on the strength of it.
  it('returns nothing for actions we have not described', () => {
    expect(describeActionEffect('AskAiCard')).toBeNull();
    expect(describeActionEffect(undefined)).toBeNull();
  });
});
