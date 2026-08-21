import { useCallback, useEffect, useMemo, useState } from 'react';
import { Box } from '@mui/material';
import CustomTable from '@shared/tables/CustomTable';
import Text from '@shared/format/Text';
import recommendationApi from '@api1/recommendation';
import { useLatestRequest } from '@components/vm/common';
import SeverityBadge, { type SeverityLevel } from './SeverityBadge';
import { formatRuleName } from './utils';
import { type ConfigRule, foldConfigRules } from './configRollup';

const TABLE_ID = 'optimise-config-rules';

const HEADERS = [
  { name: 'Severity', width: '12%' },
  { name: 'Check', width: '52%' },
  { name: 'Accounts', width: '16%' },
  { name: 'Findings', width: '20%' },
];

const SEVERITY_LEVELS: SeverityLevel[] = ['Critical', 'High', 'Medium', 'Low', 'Info'];

const toSeverityLevel = (severity: string): SeverityLevel =>
  (SEVERITY_LEVELS.find((level) => level.toLowerCase() === (severity || '').toLowerCase()) as SeverityLevel) || 'Info';

interface ConfigRuleRollupProps {
  /** Accounts in view — the page's Account filter, or every account when unset. */
  accountId: string | string[];
  status: string[];
  severity?: string[];
  /** Opens the flat, per-resource list for one check. */
  onSelectRule: (ruleName: string) => void;
}

/**
 * Configuration findings across the accounts in view, one row per check.
 *
 * These findings are emitted per resource, so a tenant with a few hundred Lambda
 * functions carries one row per function per check — thousands of rows for around
 * ninety distinct checks, none of which carry savings. Grouping by check restores
 * the scale a reader can act on; the row drills into the resources.
 */
const ConfigRuleRollup = ({ accountId, status, severity, onSelectRule }: ConfigRuleRollupProps) => {
  const [rules, setRules] = useState<ConfigRule[]>([]);
  const [loading, setLoading] = useState(false);
  const beginRequest = useLatestRequest();

  const load = useCallback(async () => {
    const scope = accountId;
    if (!scope || (Array.isArray(scope) && scope.length === 0)) {
      setRules([]);
      return;
    }
    setLoading(true);
    const isLatest = beginRequest();
    try {
      const rows = await recommendationApi.listRecommendationRuleRollup({
        accountId: scope,
        category: 'Configuration',
        status,
      });
      if (!isLatest()) return;
      setRules(foldConfigRules(rows));
    } catch (error) {
      console.error('Failed to load configuration rules:', error);
      if (isLatest()) setRules([]);
    } finally {
      if (isLatest()) setLoading(false);
    }
  }, [accountId, status, beginRequest]);

  useEffect(() => {
    load();
  }, [load]);

  // Severity is filtered here rather than in the query: the aggregate returns a
  // row per (rule, severity, account), so narrowing server-side would drop a
  // rule's other bands and understate its count. A check matches when it has any
  // finding in a selected band, not when its worst band happens to be selected —
  // and the count then reports only the findings in those bands, so a check with
  // 2 Critical and 159 Medium reads as 2 under a Critical filter, not 161.
  const visible = useMemo(() => {
    if (!severity?.length) return rules;
    const matching = rules.reduce<ConfigRule[]>((kept, rule) => {
      const matched = severity.reduce((sum, band) => sum + (rule.countBySeverity[band] || 0), 0);
      if (matched > 0) kept.push({ ...rule, count: matched });
      return kept;
    }, []);
    // Re-sorted because the counts just changed: the fold ordered by the total,
    // and the list has to be ordered by the number it is showing.
    return matching.sort((a, b) => b.count - a.count);
  }, [rules, severity]);

  const tableData = useMemo(
    () =>
      visible.map((rule) => {
        const catalog: any = recommendationApi.getRecommendationDetails('Configuration', rule.ruleName) || {};
        return [
          {
            component: (
              <Box sx={{ display: 'flex', justifyContent: 'center' }}>
                <SeverityBadge severity={toSeverityLevel(rule.severity)} />
              </Box>
            ),
            drilldownQuery: { ruleName: rule.ruleName },
            data: rule.severity,
          },
          {
            component: (
              <Box>
                <Text value={catalog.title || formatRuleName(rule.ruleName, 'Configuration')} showAutoEllipsis />
                <Text secondaryText value={rule.ruleName} showAutoEllipsis />
              </Box>
            ),
            data: rule.ruleName,
          },
          { component: <Text value={String(rule.accountIds.length)} />, data: rule.accountIds.length },
          { component: <Text value={rule.count.toLocaleString()} />, data: rule.count },
        ];
      }),
    [visible]
  );

  return (
    <CustomTable
      id={TABLE_ID}
      headers={HEADERS}
      tableData={tableData}
      loading={loading}
      rowsPerPage={tableData.length}
      totalRows={tableData.length}
      tableHeadingCenter={['Severity']}
      onRowClick={(query: any) => query?.ruleName && onSelectRule(query.ruleName)}
      showUpdatedEmptyData={tableData.length === 0}
      emptyHeading='No configuration findings'
      emptySubHeading='Checks appear here once an account has been scanned.'
    />
  );
};

export default ConfigRuleRollup;
