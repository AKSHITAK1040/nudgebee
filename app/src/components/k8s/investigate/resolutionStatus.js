// One status vocabulary for every remediation path on an event.
//
// The stored values differ by which system ran the fix: the card/agent_task path writes
// InProgress/Success/Failed, the remediation panel writes SUCCESS/FAILED/RUNNING/INCONCLUSIVE, and
// agents write PullRequest lifecycles. An operator should read one set of words regardless, so the
// mapping lives here rather than being re-derived at each call site.

// A run still InProgress after this long gets a "taking longer than expected" note. Retry is offered
// on Failed only, so without it a task that never reports back leaves no affordance at all — the
// state every revert sat in while the agent was rejecting the payload.
export const RESOLUTION_SLOW_AFTER_MS = 2 * 60 * 1000;

/**
 * Map a stored resolution onto the words shown to the operator.
 *
 * @param {object} resolution  row from listEventResolutions
 * @param {number} now         injectable clock, so the slow-run threshold is testable
 * @returns {{label: string, tone: string, actor: string, isSlow: boolean}}
 */
export const describeResolution = (resolution, now = Date.now()) => {
  const status = resolution?.status;
  const startedAt = resolution?.updated_at || resolution?.created_at;
  const startedMs = startedAt ? new Date(startedAt).getTime() : NaN;
  const isRunning = status !== 'Success' && status !== 'Failed';
  const isSlow = isRunning && Number.isFinite(startedMs) && now - startedMs > RESOLUTION_SLOW_AFTER_MS;

  // resolver_type distinguishes a person from Nubi or an automation acting on its own. For a person
  // the display name answers "who", which is the question actually asked; for the others the type is
  // the more useful label.
  const actor = resolution?.resolver_type === 'User' ? resolution?.resolver_display_name || 'a user' : resolution?.resolver_type || '';

  if (status === 'Success') return { label: 'Done', tone: 'success', actor, isSlow: false };
  if (status === 'Failed') return { label: 'Failed', tone: 'critical', actor, isSlow: false };
  // Configuring, InProgress, and anything a future writer adds all read as in-flight rather than as
  // a bare enum leaking to the screen.
  return { label: 'Running', tone: 'info', actor, isSlow };
};

// Whether an action removes the cause or only restores service. The wording matches what the
// remediation panel already tells the operator about Nubi's actions:
//
//   "A fix removes the root cause; a mitigation only restores service while the cause survives,
//    so the problem recurs."
//
// Only the card actions are classified here — Nubi's carry a server-assigned kind of their own.
// Anything not listed shows no chip: an unlabelled action is honest, a wrongly labelled one is not.
const ACTION_KIND_BY_CARD = {
  // Putting the workload back on the spec that was running before the change removes the change
  // itself, so if the change caused the problem, the cause is gone.
  LastDeploymentCard: 'fix',
  // Raising the limit stops this class of OOM rather than papering over it — the workload genuinely
  // needed the headroom. It is a fix in the sense that matters here: the condition does not return.
  MemoryAllocationCard: 'fix',
};

export const ACTION_KIND_META = {
  fix: { text: 'Fix', tone: 'success', help: 'Removes the cause' },
  mitigation: { text: 'Mitigation', tone: 'warning', help: 'Restores service; the cause survives' },
};

/**
 * Classify a card-based action, or return null when it cannot be classified honestly.
 * @param {string} cardId
 */
export const describeActionKind = (cardId) => {
  const kind = ACTION_KIND_BY_CARD[cardId];
  return kind ? { kind, ...ACTION_KIND_META[kind] } : null;
};

// What running an action actually does to the cluster, in the operator's terms.
//
// The confirmation step names the object and shows a diff, but never says the change replaces every
// running pod. Someone approving a revert at 3am should not have to know that a pod-template change
// triggers a rolling restart in order to understand what they are agreeing to.
//
// Kept deliberately non-numeric: replica counts and field counts vary per event, and a wrong number
// here is worse than no number.
const ACTION_EFFECT_BY_CARD = {
  LastDeploymentCard: 'Puts the workload back on the spec it ran before the change · rolling restart · reversible',
  MemoryAllocationCard: 'Changes the container resource requests and limits · rolling restart · reversible',
};

/**
 * One line describing an action's effect, or null when we have nothing accurate to say.
 * @param {string} cardId
 */
export const describeActionEffect = (cardId) => ACTION_EFFECT_BY_CARD[cardId] || null;
