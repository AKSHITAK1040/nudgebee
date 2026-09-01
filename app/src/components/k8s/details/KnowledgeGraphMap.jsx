import { useEffect, useState, useMemo, useCallback, memo } from 'react';
import ReactFlow, {
  ReactFlowProvider,
  Controls,
  Background,
  BackgroundVariant,
  MiniMap,
  useNodesState,
  useEdgesState,
  Handle,
  Position,
} from 'reactflow';
import 'reactflow/dist/style.css';
import { Box, Typography } from '@mui/material';
import { ds } from 'src/utils/colors';

const ELK_OPTIONS = {
  'elk.algorithm': 'layered',
  'elk.direction': 'RIGHT',
  'elk.edgeRouting': 'ORTHOGONAL',
  'elk.hierarchyHandling': 'INCLUDE_CHILDREN',
  'elk.layered.spacing.edgeNodeBetweenLayers': '30',
  'elk.spacing.nodeNode': '60',
  'elk.spacing.nodeNodeBetweenLayers': '80',
  'elk.layered.nodePlacement.strategy': 'BRANDES_KOEPF',
  'elk.layered.mergeEdges': 'true',
};

const NODE_TYPE_COLORS = {
  Service: 'var(--ds-blue-500)',
  Workload: 'var(--ds-blue-500)',
  Database: 'var(--ds-teal-500)',
  ExternalService: 'var(--ds-amber-400)',
  MessageQueue: 'var(--ds-purple-400)',
  Cache: 'var(--ds-purple-400)',
  Queue: 'var(--ds-purple-400)',
  Topic: 'var(--ds-purple-400)',
  LoadBalancer: 'var(--ds-teal-400)',
  ServerlessFunction: 'var(--ds-pink-400)',
};

const NODE_TYPE_LABELS = {
  Service: 'SVC',
  Workload: 'WKL',
  Database: 'DB',
  ExternalService: 'EXT',
  MessageQueue: 'MQ',
  Cache: 'CACHE',
  Queue: 'QUEUE',
  Topic: 'TOPIC',
  LoadBalancer: 'LB',
  ServerlessFunction: 'FN',
};

// Readable form of a node's specific_type, shown under the name. The badge above
// carries the ontological type (WKL, K8S) and cannot separate a Deployment from a
// StatefulSet, nor say what a K8sService actually is — which matters because a
// workload and the service in front of it share a name and a namespace, so on a
// dependency graph they are otherwise two identical-looking nodes.
const SPECIFIC_TYPE_LABELS = {
  KubernetesDeployment: 'Deployment',
  KubernetesStatefulSet: 'StatefulSet',
  KubernetesDaemonSet: 'DaemonSet',
  KubernetesJob: 'Job',
  KubernetesCronJob: 'CronJob',
  KubernetesService: 'Service',
};

// specificTypeLabel returns what to print under the node name, or '' when there is
// nothing to add. Types whose specific_type merely repeats node_type (Service,
// Storage, ExternalService, Database, MessageQueue) contribute no information and
// are skipped; a cloud type with no mapping falls back to its raw specific_type,
// which is already short and self-describing (EC2Instance, S3Bucket, RDSInstance).
export function specificTypeLabel(specificType, nodeType) {
  if (!specificType || specificType === nodeType) {
    return '';
  }
  return SPECIFIC_TYPE_LABELS[specificType] || specificType;
}

// Cached promise of the dynamically-loaded ELK instance. The constructor spawns
// a worker, so keeping it to one instance across re-layouts avoids repeated
// worker spawn cost. Reset to null on import failure so a transient network
// blip can be retried.
let elkPromise = null;

// elkjs is loaded dynamically so the 1.4MB bundle ships only when the
// knowledge-graph map actually mounts, not on every page via the static
// import chain.
const getLayoutedElements = async (nodes, edges) => {
  const graph = {
    id: 'root',
    layoutOptions: ELK_OPTIONS,
    // ELK positions nodes from the dimensions given here, so the height has to
    // track how many subtitle lines the node actually renders — a node carrying
    // both a specific type and a namespace/cluster line is taller than one with
    // neither, and understating it makes neighbours overlap. 42 is the name line
    // plus padding; each subtitle line adds 18. Two lines therefore still measure
    // the 60 this used to hardcode.
    children: nodes.map((node) => ({
      ...node,
      targetPosition: 'left',
      sourcePosition: 'right',
      width: 220,
      height: 42 + [node.data?.specificType, node.data?.scope].filter(Boolean).length * 18,
    })),
    edges: edges,
  };

  try {
    if (!elkPromise) {
      elkPromise = import('elkjs/lib/elk.bundled.js')
        .then((M) => new M.default())
        .catch((err) => {
          elkPromise = null;
          throw err;
        });
    }
    const elk = await elkPromise;
    const layoutedGraph = await elk.layout(graph);
    return {
      nodes: layoutedGraph.children.map((node) => ({
        ...node,
        position: { x: node.x, y: node.y },
      })),
      edges: layoutedGraph.edges,
    };
  } catch (err) {
    console.error('ELK Layout Failed:', err);
    return { nodes, edges };
  }
};

const KGNode = memo(({ data }) => {
  const color = data.color || ds.gray[500];
  const isTarget = data.isTarget;

  return (
    <Box
      sx={{
        padding: 'var(--ds-space-2) var(--ds-space-3)',
        borderRadius: 'var(--ds-radius-md)',
        background: 'var(--ds-background-100)',
        border: `2px solid ${isTarget ? color : 'var(--ds-brand-150)'}`,
        boxShadow: isTarget ? `0 0 0 ${ds.space[0]} ${color}33` : `0 1px 3px ${ds.gray.alpha[200]}`,
        minWidth: ds.space.mul(0, 80),
        display: 'flex',
        alignItems: 'center',
        gap: 'var(--ds-space-2)',
      }}
    >
      <Handle type='target' position={Position.Left} style={{ background: color, width: 6, height: 6 }} />
      <Box
        sx={{
          width: ds.space.mul(1, 7),
          height: ds.space.mul(1, 7),
          borderRadius: 'var(--ds-radius-sm)',
          background: `${color}1a`,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          flexShrink: 0,
        }}
      >
        <Typography sx={{ fontSize: 'var(--ds-text-caption)', fontWeight: 'var(--ds-font-weight-semibold)', color: color, lineHeight: 1 }}>
          {data.typeLabel}
        </Typography>
      </Box>
      <Box sx={{ overflow: 'hidden' }}>
        <Typography
          sx={{
            fontSize: 'var(--ds-text-small)',
            fontWeight: isTarget ? 600 : 500,
            color: ds.blue[500],
            whiteSpace: 'nowrap',
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            maxWidth: ds.space.mul(0, 70),
          }}
          title={data.name}
        >
          {data.name}
        </Typography>
        {data.specificType ? (
          <Typography
            sx={{
              fontSize: 'var(--ds-text-caption)',
              color: ds.gray[600],
              whiteSpace: 'nowrap',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              maxWidth: ds.space.mul(0, 70),
            }}
          >
            {data.specificType}
          </Typography>
        ) : null}
        {data.scope ? (
          <Typography
            sx={{
              fontSize: 'var(--ds-text-caption)',
              color: ds.gray[700],
              whiteSpace: 'nowrap',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              maxWidth: ds.space.mul(0, 70),
            }}
            title={data.scope}
          >
            {data.scope}
          </Typography>
        ) : null}
      </Box>
      <Handle type='source' position={Position.Right} style={{ background: color, width: 6, height: 6 }} />
    </Box>
  );
});

KGNode.displayName = 'KGNode';

function transformKGData(kgNodes, kgEdges, targetService) {
  const nodeMap = new Map();
  kgNodes.forEach((n) => {
    nodeMap.set(n.id, n);
  });

  const rfNodes = kgNodes.map((node) => {
    const name = node.properties?.name || node.unique_key || node.id;
    const namespace = node.properties?.namespace || '';
    const nodeType = node.node_type || 'Service';
    const color = NODE_TYPE_COLORS[nodeType] || ds.gray[500];
    const typeLabel = NODE_TYPE_LABELS[nodeType] || nodeType.substring(0, 3).toUpperCase();
    const isTarget = name === targetService;
    const specificType = specificTypeLabel(node.specific_type, nodeType);
    // Namespace and cluster on one line. The cluster is what separates two nodes
    // that are otherwise identical on screen — the same workload in a dev and a
    // prod cluster under one tenant — so it is shown whenever the graph carries it.
    const scope = [namespace, node.properties?.cluster].filter(Boolean).join(' · ');

    return {
      id: node.id,
      type: 'kg-node',
      position: { x: 0, y: 0 },
      data: { name, namespace, scope, specificType, color, typeLabel, isTarget, nodeType },
    };
  });

  const rfEdges = kgEdges
    .filter((edge) => nodeMap.has(edge.source_node_id) && nodeMap.has(edge.dest_node_id))
    .map((edge, idx) => ({
      id: `kg-edge-${idx}`,
      source: edge.source_node_id,
      target: edge.dest_node_id,
      animated: edge.relationship_type === 'CALLS',
      style: { stroke: edge.relationship_type === 'CALLS' ? ds.blue[500] : ds.gray[500], strokeWidth: 1.5 },
      markerEnd: { type: 'arrow', color: edge.relationship_type === 'CALLS' ? ds.blue[500] : ds.gray[500] },
      label: edge.relationship_type !== 'CALLS' ? edge.relationship_type : '',
      labelStyle: { fontSize: 9, fill: 'var(--ds-gray-600)' },
    }));

  return { nodes: rfNodes, edges: rfEdges };
}

const KnowledgeGraphMapInner = ({ nodes: kgNodes, edges: kgEdges, targetService }) => {
  const [nodes, setNodes, onNodesChange] = useNodesState([]);
  const [edges, setEdges, onEdgesChange] = useEdgesState([]);
  const [isLayoutLoading, setIsLayoutLoading] = useState(true);

  const nodeTypes = useMemo(() => ({ 'kg-node': KGNode }), []);

  const { nodes: rawNodes, edges: rawEdges } = useMemo(() => transformKGData(kgNodes, kgEdges, targetService), [kgNodes, kgEdges, targetService]);

  useEffect(() => {
    if (rawNodes.length === 0) {
      setIsLayoutLoading(false);
      return;
    }

    const runLayout = async () => {
      setIsLayoutLoading(true);
      const { nodes: lNodes, edges: lEdges } = await getLayoutedElements(rawNodes, rawEdges);
      setNodes(lNodes);
      setEdges(lEdges);
      setIsLayoutLoading(false);
    };

    runLayout();
  }, [rawNodes, rawEdges, setNodes, setEdges]);

  const [highlightedEdges, setHighlightedEdges] = useState([]);

  const onNodeMouseEnter = useCallback(
    (_, node) => {
      const connectedIds = edges.filter((e) => e.source === node.id || e.target === node.id).map((e) => e.id);
      setHighlightedEdges(connectedIds);
    },
    [edges]
  );

  const onNodeMouseLeave = useCallback(() => {
    setHighlightedEdges([]);
  }, []);

  const styledEdges = useMemo(() => {
    if (highlightedEdges.length === 0) {
      return edges;
    }
    return edges.map((e) => ({
      ...e,
      style: {
        ...e.style,
        opacity: highlightedEdges.includes(e.id) ? 1 : 0.15,
        strokeWidth: highlightedEdges.includes(e.id) ? 2.5 : 1.5,
      },
    }));
  }, [edges, highlightedEdges]);

  if (isLayoutLoading) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '400px' }}>
        <Typography sx={{ color: 'var(--ds-gray-600)', fontSize: 'var(--ds-text-body)' }}>Computing layout...</Typography>
      </Box>
    );
  }

  if (nodes.length === 0) {
    return (
      <Box sx={{ display: 'flex', justifyContent: 'center', alignItems: 'center', height: '200px' }}>
        <Typography sx={{ color: 'var(--ds-gray-600)', fontSize: 'var(--ds-text-body)' }}>No service dependency data available</Typography>
      </Box>
    );
  }

  return (
    <Box sx={{ height: '450px', width: '100%', border: '1px solid var(--ds-brand-150)', borderRadius: 'var(--ds-radius-lg)', overflow: 'hidden' }}>
      <ReactFlow
        nodes={nodes}
        edges={styledEdges}
        onNodesChange={onNodesChange}
        onEdgesChange={onEdgesChange}
        onNodeMouseEnter={onNodeMouseEnter}
        onNodeMouseLeave={onNodeMouseLeave}
        nodeTypes={nodeTypes}
        fitView
        fitViewOptions={{ padding: 0.2 }}
        minZoom={0.1}
        maxZoom={2}
        proOptions={{ hideAttribution: true }}
      >
        <Controls showInteractive={false} />
        <Background variant={BackgroundVariant.Dots} gap={16} size={1} color={ds.gray[200]} />
        <MiniMap
          nodeStrokeWidth={2}
          nodeColor={(node) => (node.data?.isTarget ? node.data.color : 'var(--ds-brand-150)')}
          style={{ height: 80, width: 120 }}
        />
      </ReactFlow>
    </Box>
  );
};

const KnowledgeGraphMap = (props) => (
  <ReactFlowProvider>
    <KnowledgeGraphMapInner {...props} />
  </ReactFlowProvider>
);

export default KnowledgeGraphMap;
