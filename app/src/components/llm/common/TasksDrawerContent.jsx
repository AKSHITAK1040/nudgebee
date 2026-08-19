import React from 'react';
import { Box, Typography } from '@mui/material';
import PropTypes from 'prop-types';
import { ds } from '@utils/colors';
import Tooltip from '@ui/Tooltip';
import MessageItem from '../MessageItem';

const taskKeyOf = (task) => String(task.tool_id ?? task.id ?? task.originalIndex ?? '');
const byCreated = (a, b) => {
  const ca = a.task?.created_at || '';
  const cb = b.task?.created_at || '';
  return ca < cb ? -1 : ca > cb ? 1 : 0;
};

const buildTaskTree = (tasks) => {
  const byId = new Map();
  tasks.forEach((t) => {
    const id = taskKeyOf(t);
    if (id) {
      byId.set(id, t);
    }
  });

  const childrenOf = new Map(); // node key -> child nodes[]
  const roots = [];

  const addChild = (parentKey, node) => {
    if (!childrenOf.has(parentKey)) {
      childrenOf.set(parentKey, []);
    }
    childrenOf.get(parentKey).push(node);
  };

  tasks.forEach((task) => {
    const node = { key: 'task:' + taskKeyOf(task), task };
    const parentId = task.parentId != null ? String(task.parentId) : null;
    // Nest under the parent row when it resolves; guard against a self-parent so a bad link
    // can't make a row its own child.
    if (parentId && parentId !== taskKeyOf(task) && byId.has(parentId)) {
      addChild('task:' + parentId, node);
    } else {
      roots.push(node);
    }
  });

  return { roots, childrenOf };
};

const flattenTree = ({ roots, childrenOf }) => {
  const out = [];
  const visited = new Set();
  const visit = (node, depth) => {
    if (visited.has(node.key)) {
      return;
    }
    visited.add(node.key);
    out.push({ node, depth });
    (childrenOf.get(node.key) || [])
      .slice()
      .sort(byCreated)
      .forEach((child) => visit(child, depth + 1));
  };
  roots
    .slice()
    .sort(byCreated)
    .forEach((root) => visit(root, 0));
  return { out, visited };
};

// Re-home orphans as flat root rows: only genuinely-unreachable rows (cyclic/broken parent link)
// qualify. Kept separate from `flattenTree` so that stays a pure tree walk.
const flattenWithOrphans = (tasks, tree) => {
  const { out, visited } = flattenTree(tree);
  tasks.forEach((task) => {
    const key = 'task:' + taskKeyOf(task);
    if (!visited.has(key)) {
      visited.add(key);
      out.push({ node: { key, task }, depth: 0 });
    }
  });
  return out;
};

const LEVEL_COLOR = ['var(--ds-gray-700)', '#6B7280', '#9AA0A8', '#BCBFC4'];
const VIEWBOX_W = 21;
const VIEWBOX_H = 24;
const MAX_LEVEL = 4;
const ORIGIN_X = 1.5;
const ARROW_RUN = 9;
const ARROW_HEAD = 2.4;
const STEP_X = ARROW_RUN / 2;
const STEP_Y = 6;
const ARROW_HEAD_SCALE = [1, 0.85, 0.7];
const BRANCH_WIDTH = 1.2;
const TRUNK_WIDTH = 2.4;
const INDICATOR_COL_W = 36; // fixed gutter width (px) — keeps every row's title aligned
const INDICATOR_H = 24;
const INDICATOR_W = (INDICATOR_H * VIEWBOX_W) / VIEWBOX_H;

const DepthIndicator = ({ depth }) => {
  const level = Math.min(Math.max(depth + 1, 1), MAX_LEVEL); // drawer depth 0 → level 1 (main); clamp at 4
  const color = LEVEL_COLOR[level - 1];
  const arrowCount = level - 1;
  const headSize = ARROW_HEAD * (ARROW_HEAD_SCALE[arrowCount - 1] ?? 1);
  const top = (VIEWBOX_H - (arrowCount * STEP_Y + headSize)) / 2;

  return (
    <svg width={INDICATOR_W} height={INDICATOR_H} viewBox={`0 0 ${VIEWBOX_W} ${VIEWBOX_H}`} fill='none' aria-hidden style={{ display: 'block' }}>
      {level === 1 ? (
        <line x1={ORIGIN_X} y1={3} x2={ORIGIN_X} y2={VIEWBOX_H - 3} strokeLinecap='round' style={{ stroke: color, strokeWidth: TRUNK_WIDTH }} />
      ) : (
        Array.from({ length: arrowCount }, (_, i) => {
          const x = ORIGIN_X + i * STEP_X;
          const y = top + (i + 1) * STEP_Y;
          const toX = x + ARROW_RUN;
          const elbow = `M${x} ${top + i * STEP_Y}V${y}H${toX}`;
          const head = `M${toX - headSize} ${y - headSize}L${toX} ${y}L${toX - headSize} ${y + headSize}`;
          return (
            <path
              key={i}
              d={`${elbow}${head}`}
              strokeLinecap='round'
              strokeLinejoin='round'
              fill='none'
              style={{ stroke: color, strokeWidth: BRANCH_WIDTH }}
            />
          );
        })
      )}
    </svg>
  );
};

DepthIndicator.propTypes = {
  depth: PropTypes.number,
};

// Tooltip on the depth glyph naming its sub-level. Root (depth 0) is the main task; everything below
// is a sub-task numbered by how deep it nests.
const subLevelLabel = (depth) => (depth <= 0 ? 'Task' : `Sub-task · level ${depth}`);

const TaskRow = ({ task, depth, collapsed, onToggleCollapse, accountId, conversationId, isLast, isActive, onOpenToolDetails, itemProps }) => {
  const isHeader = depth === 0 && task.nodeKind === 'agent';
  const isActionable = (task.nodeKind === 'agent' || task.nodeKind === 'tool') && !isHeader;
  const collapsible = collapsed !== undefined;
  const openDetails = isActionable ? () => onOpenToolDetails(task) : undefined;
  const onClick = collapsible ? onToggleCollapse : openDetails;
  const clickable = collapsible || isActionable;
  return (
    <Box
      onClick={onClick}
      sx={{
        display: 'flex',
        cursor: clickable ? 'pointer' : 'default',
        ...(clickable ? { '& [id^="task-card-"] *': { cursor: 'pointer' } } : {}),
      }}
    >
      <Box sx={{ flexShrink: 0, width: INDICATOR_COL_W, pl: ds.space[2], display: 'flex', alignItems: 'center', justifyContent: 'flex-start' }}>
        <Tooltip title={subLevelLabel(depth)} placement='top'>
          <Box component='span' aria-hidden sx={{ display: 'inline-flex', lineHeight: 0 }}>
            <DepthIndicator depth={depth} />
          </Box>
        </Tooltip>
      </Box>
      {/* Only the task box carries the hover/active highlight — the depth gutter stays outside it. */}
      <Box
        sx={{
          flex: 1,
          minWidth: 0,
          borderRadius: ds.radius.lg,
          transition: 'background-color 0.15s ease, box-shadow 0.15s ease',
          '& [id^="task-card-"] > div': {
            backgroundColor: 'transparent !important',
          },
          // Drop the card's own bottom divider on the active box so the inset ring isn't doubled by a
          // stray grey line near its base (which read as top-heavy).
          '& [id^="task-card-"]': { borderBottom: isActive ? 'none' : undefined },
          // The (repeated) Details button reveal is actionable-only; the hover highlight applies to any
          // clickable row (Details rows and collapsible headers).
          ...(isActionable
            ? {
                '& #tool-details-btn': { opacity: 0, transition: 'opacity 0.15s ease' },
                '&:hover #tool-details-btn, &:focus-within #tool-details-btn': { opacity: 1 },
              }
            : {}),
          ...(clickable ? { '&:hover': { backgroundColor: 'var(--ds-background-100)' } } : {}),
          backgroundColor: isActive ? 'var(--ds-background-100)' : 'transparent',
          boxShadow: isActive ? 'inset 0 0 0 1px var(--ds-blue-200)' : 'none',
        }}
      >
        <MessageItem
          message={task}
          index={task.originalIndex ?? task.id ?? 0}
          isLastInGroup={isLast}
          isLastTaskOfLastGroup={false}
          isCollapsed={false}
          collapsedObj={{}}
          onToggle={openDetails}
          showFullText={false}
          onShowFullText={() => {}}
          accountId={accountId}
          conversationId={conversationId}
          sessionId={itemProps?.sessionId}
          generateQuestionText={itemProps?.generateQuestionText}
          handleShare={itemProps?.handleShare}
          agentTokenData={itemProps?.getAgentTokenDataForMessage?.(task)}
          messageTokenData={itemProps?.messageTokenData?.[task.id]}
          handleTokenUsageHover={itemProps?.handleTokenUsageHover}
          isFetchingTokenData={itemProps?.isFetchingTokenData}
          selectedModel={itemProps?.selectedModel}
          conversationStatus={itemProps?.conversationStatus}
          onOpenToolDetails={openDetails}
          indentDepth={depth}
          collapsed={collapsed}
          hideTimeline
        />
      </Box>
    </Box>
  );
};

TaskRow.propTypes = {
  task: PropTypes.object.isRequired,
  depth: PropTypes.number,
  collapsed: PropTypes.bool,
  onToggleCollapse: PropTypes.func,
  accountId: PropTypes.string,
  conversationId: PropTypes.string,
  isLast: PropTypes.bool,
  isActive: PropTypes.bool,
  onOpenToolDetails: PropTypes.func.isRequired,
  itemProps: PropTypes.object,
};

const matchesActiveKey = (task, activeTaskKey) => {
  if (activeTaskKey == null) {
    return false;
  }
  const candidates = [task.id, task.tool_id, task.originalIndex];
  return candidates.some((c) => c != null && String(c) === String(activeTaskKey));
};

const hasChildren = (key, childrenOf) => (childrenOf.get(key) || []).length > 0;

const EXPANDABLE_MIN_DEPTH = 1;
const EXPANDABLE_MAX_DEPTH = 2;

const TasksDrawerContent = ({ tasks, accountId, conversationId, activeTaskKey, onOpenToolDetails, itemProps }) => {
  const tree = React.useMemo(() => buildTaskTree(tasks ?? []), [tasks]);
  const rows = React.useMemo(() => flattenWithOrphans(tasks ?? [], tree), [tasks, tree]);

  const isExpandable = React.useCallback(
    (row) => row.depth >= EXPANDABLE_MIN_DEPTH && row.depth <= EXPANDABLE_MAX_DEPTH && hasChildren(row.node.key, tree.childrenOf),
    [tree]
  );

  const [expandedKeys, setExpandedKeys] = React.useState(() => new Set());
  const toggleExpand = React.useCallback((key) => {
    setExpandedKeys((prev) => {
      const next = new Set(prev);
      if (next.has(key)) {
        next.delete(key);
      } else {
        next.add(key);
      }
      return next;
    });
  }, []);

  const visibleRows = React.useMemo(() => {
    const out = [];
    let closedAtDepth = null;
    rows.forEach((row) => {
      if (closedAtDepth !== null && row.depth > closedAtDepth) {
        return;
      }
      closedAtDepth = null;
      out.push(row);
      if (isExpandable(row) && !expandedKeys.has(row.node.key)) {
        closedAtDepth = row.depth;
      }
    });
    return out;
  }, [rows, expandedKeys, isExpandable]);

  if (!tasks || tasks.length === 0) {
    return (
      <Typography
        sx={{
          fontSize: 'var(--ds-text-body)',
          color: 'var(--ds-gray-500)',
          fontFamily: ds.font.sans,
          textAlign: 'center',
          mt: ds.space[5],
        }}
      >
        No tool calls for this response.
      </Typography>
    );
  }
  return (
    <Box>
      {visibleRows.map((row, idx) => {
        const { node, depth } = row;
        const expandable = isExpandable(row);
        return (
          <TaskRow
            key={node.key}
            task={node.task}
            depth={depth}
            collapsed={expandable ? !expandedKeys.has(node.key) : undefined}
            onToggleCollapse={expandable ? () => toggleExpand(node.key) : undefined}
            accountId={accountId}
            conversationId={conversationId}
            isLast={idx === visibleRows.length - 1}
            isActive={matchesActiveKey(node.task, activeTaskKey)}
            onOpenToolDetails={onOpenToolDetails}
            itemProps={itemProps}
          />
        );
      })}
    </Box>
  );
};

TasksDrawerContent.propTypes = {
  tasks: PropTypes.array.isRequired,
  accountId: PropTypes.string,
  conversationId: PropTypes.string,
  activeTaskKey: PropTypes.oneOfType([PropTypes.string, PropTypes.number]),
  onOpenToolDetails: PropTypes.func.isRequired,
  itemProps: PropTypes.object,
};

export default TasksDrawerContent;

// Exported for unit tests only. Not part of the public component API.
export { buildTaskTree, flattenWithOrphans };
