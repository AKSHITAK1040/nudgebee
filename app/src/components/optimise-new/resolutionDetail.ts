/**
 * Reading a resolution's payload back as a change.
 *
 * A resolution stores the spec it *applied*, keyed by container:
 * `{ web: { cpu: { request }, memory: { request, limit } } }`. On its own that
 * is a set of target numbers — and half of them arrive as raw bytes, so the
 * panel used to render `402258739` where the rest of the product says `384 Mi`.
 * The recommendation it resolved holds the other half of the story (what the
 * container had *before*), in the different shape `rightSizingData` parses.
 * These helpers join the two so a resolution reads as the change it made.
 */
import { extractContainerData, formatCpuShort, formatMemShort } from './rightSizingData';

export type QuantityKind = 'cpu' | 'memory';

/**
 * Render one applied value.
 *
 * The payload mixes types: CPU tends to arrive as an already-suffixed Kubernetes
 * quantity string (`81m`, `1.5`), memory as a raw byte count. A string carrying
 * a unit is passed through rather than re-derived — reformatting `81m` as though
 * it were 81 cores is how a panel starts lying.
 */
export const formatQuantity = (value: unknown, kind: QuantityKind): string => {
  if (value === null || value === undefined || value === '') return '—';
  if (typeof value === 'string') {
    if (/[a-zA-Z]$/.test(value.trim())) return value.trim();
    const numeric = Number(value);
    if (!Number.isFinite(numeric)) return value;
    return kind === 'cpu' ? formatCpuShort(numeric) : formatMemShort(numeric);
  }
  if (typeof value === 'number') {
    return kind === 'cpu' ? formatCpuShort(value) : formatMemShort(value);
  }
  return String(value);
};

export interface AppliedChangeRow {
  label: string;
  isMem: boolean;
  /** Numeric so the shared ResourceChangeCell formats it the same way the recommendation panel does. */
  before: number | null;
  after: number | null;
  /** Only used when `after` could not be parsed — better a raw string than a dash. */
  afterText: string;
}

export interface AppliedChange {
  containerName: string;
  rows: AppliedChangeRow[];
}

const SUFFIX_MULTIPLIER: Record<string, number> = {
  m: 0.001,
  k: 1e3,
  M: 1e6,
  G: 1e9,
  T: 1e12,
  Ki: 1024,
  Mi: 1024 ** 2,
  Gi: 1024 ** 3,
  Ti: 1024 ** 4,
};

/**
 * A Kubernetes quantity as a plain number, for comparing before against after.
 *
 * Only used to compute the size of a change, so anything it cannot parse
 * confidently returns null and the change simply reports no percentage —
 * a wrong percentage is worse than an absent one.
 */
export const parseQuantity = (value: unknown): number | null => {
  if (typeof value === 'number') return Number.isFinite(value) ? value : null;
  if (typeof value !== 'string') return null;
  const match = /^\s*(-?[\d.]+)\s*([a-zA-Z]{0,2})\s*$/.exec(value);
  if (!match) return null;
  const magnitude = Number(match[1]);
  if (!Number.isFinite(magnitude)) return null;
  const suffix = match[2];
  if (!suffix) return magnitude;
  const multiplier = SUFFIX_MULTIPLIER[suffix];
  return multiplier === undefined ? null : magnitude * multiplier;
};

const FIELDS: { kind: QuantityKind; field: 'request' | 'limit'; label: string }[] = [
  { kind: 'cpu', field: 'request', label: 'CPU request' },
  { kind: 'cpu', field: 'limit', label: 'CPU limit' },
  { kind: 'memory', field: 'request', label: 'Memory request' },
  { kind: 'memory', field: 'limit', label: 'Memory limit' },
];

/**
 * Join an applied spec to the recommendation's before-state, one group per
 * container. `before` is null when the recommendation is not loaded or does not
 * cover that container — the applied value still renders, in readable units,
 * rather than the row disappearing.
 *
 * Returns [] for a payload that is not container-keyed (config fixes, cloud
 * right-sizing, anything else), so the caller can fall back to a generic view
 * instead of showing an empty "what changed" section.
 */
export const buildAppliedChanges = (appliedData: any, recommendationData?: any): AppliedChange[] => {
  if (!appliedData || typeof appliedData !== 'object' || Array.isArray(appliedData)) return [];

  const before = new Map(extractContainerData(recommendationData).map((entry) => [entry.containerName, entry]));
  // The recommendation's older payload shape collapses to a single synthetic
  // container literally named "default", so a named container still finds its
  // before-state there. Gated on that name rather than on "there is only one
  // entry": a recommendation that names one REAL container which the resolution
  // did not touch would otherwise lend its allocated values to a different
  // container, and print a before-state that never existed.
  const soleBefore = before.size === 1 ? before.get('default') : undefined;

  const changes: AppliedChange[] = [];
  for (const [containerName, spec] of Object.entries(appliedData)) {
    if (!spec || typeof spec !== 'object' || Array.isArray(spec)) continue;
    const applied = spec as Record<string, any>;
    if (!applied.cpu && !applied.memory) continue;

    const priorEntry = before.get(containerName) ?? soleBefore;
    const rows = FIELDS.reduce<AppliedChange['rows']>((kept, { kind, field, label }) => {
      const after = applied[kind]?.[field];
      if (after === undefined || after === null || after === '') return kept;
      const priorRaw = priorEntry?.[kind]?.allocated?.[field];
      kept.push({
        label,
        isMem: kind === 'memory',
        before: priorRaw === undefined || priorRaw === null ? null : parseQuantity(priorRaw),
        after: parseQuantity(after),
        afterText: formatQuantity(after, kind),
      });
      return kept;
    }, []);

    if (rows.length > 0) changes.push({ containerName, rows });
  }
  return changes;
};

/**
 * How long a resolution has been running, or ran for.
 *
 * Coarse on purpose — the question this answers is "is this stuck?", and a
 * to-the-second figure invites reading precision into two timestamps written by
 * different processes. Returns null when either end is missing or unparseable,
 * so the caller omits the row rather than printing "NaN".
 */
export const formatDuration = (from: unknown, to: unknown): string | null => {
  const start = from ? new Date(from as string).getTime() : NaN;
  const end = to ? new Date(to as string).getTime() : NaN;
  if (!Number.isFinite(start) || !Number.isFinite(end)) return null;
  const ms = end - start;
  if (ms < 0) return null;
  const seconds = Math.floor(ms / 1000);
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  const days = Math.floor(hours / 24);
  return `${days}d ${hours % 24}h`;
};
