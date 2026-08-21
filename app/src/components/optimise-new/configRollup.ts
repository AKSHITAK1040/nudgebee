/**
 * One configuration check, rolled up across the accounts in view.
 *
 * The grouping query returns a row per (rule, severity, account). A tenant with
 * a few hundred Lambda functions produces one finding per function per check, so
 * the flat list runs to thousands of rows for around ninety distinct checks —
 * the fold turns that back into the shape a reader triages in.
 */
export interface ConfigRule {
  ruleName: string;
  /** Worst severity seen for the rule across accounts — what the row displays. */
  severity: string;
  count: number;
  accountIds: string[];
  countByAccount: Record<string, number>;
  /**
   * Findings per severity band. A rule can span bands — popeye's
   * `misconfigurations` runs Critical through Info — so filtering on the worst
   * band alone would hide a rule from the very band it has findings in.
   */
  countBySeverity: Record<string, number>;
}

const SEVERITY_RANK: Record<string, number> = { Critical: 5, High: 4, Medium: 3, Low: 2, Info: 1, Unknown: 0 };

const rank = (s: string) => SEVERITY_RANK[s] ?? 0;

/**
 * Fold the per-(rule, severity, account) grouping rows into one row per rule.
 *
 * Ordered by blast radius before severity, the opposite of the cloud posture
 * rollup. Configuration findings carry no savings and — since the rule
 * severities were calibrated to their published baselines — cluster in the lower
 * bands, so the count is what separates "every Lambda in the estate" from a
 * single stray resource. Severity breaks ties.
 */
export const foldConfigRules = (rows: any[]): ConfigRule[] => {
  const byRule = new Map<string, ConfigRule>();

  for (const row of rows || []) {
    const ruleName = row?.rule_name;
    if (!ruleName) continue;
    const accountId = row?.account_id;
    const count = Number(row?.count) || 0;
    const severity = row?.severity || 'Unknown';

    const existing = byRule.get(ruleName);
    if (!existing) {
      byRule.set(ruleName, {
        ruleName,
        severity,
        count,
        accountIds: accountId ? [accountId] : [],
        countByAccount: accountId ? { [accountId]: count } : {},
        countBySeverity: { [severity]: count },
      });
      continue;
    }
    existing.count += count;
    existing.countBySeverity[severity] = (existing.countBySeverity[severity] || 0) + count;
    if (rank(severity) > rank(existing.severity)) existing.severity = severity;
    if (accountId) {
      existing.countByAccount[accountId] = (existing.countByAccount[accountId] || 0) + count;
      if (!existing.accountIds.includes(accountId)) existing.accountIds.push(accountId);
    }
  }

  return Array.from(byRule.values()).sort((a, b) => b.count - a.count || rank(b.severity) - rank(a.severity));
};
