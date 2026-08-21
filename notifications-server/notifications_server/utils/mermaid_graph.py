"""Render Mermaid ``flowchart``/``graph`` diagrams as an actual image, via a
strict node/edge parser plus Graphviz - the Image tier in the Slack-native ->
Image -> code-block fallback chain (see mermaid_chart.py's render_mermaid_code).

Mermaid's full flowchart grammar (subgraphs, six-plus arrow styles, a dozen
node shapes, style/classDef/click directives, ...) is too large to safely
replicate, and the dangerous failure mode isn't "fails to parse" - that's
safe, it falls through to the existing code-block fallback - it's a
*partial* parse: silently dropping a subgraph or an edge label and still
rendering a clean-looking picture that misrepresents the real diagram. That
would be worse than today's raw-text fallback, which is at least honest
about what it shows. So this parser is deliberately all-or-nothing: any
line that isn't recognized fails the whole parse (returns None), and the
caller falls through to the code-block fallback exactly as it did before
this module existed.

Scope is intentionally narrow: the subset the VisualizationAgent's own prompt
documents and its validation tool enforces
(llm/llm-server/agents/agent_visualization.go,
llm/llm-server/tools/tool_mermaid_validation.go) - node labels (quoted or
unquoted, same tolerance mermaid_chart.py's xychart/pie parsers already
have, since real generated diagrams don't always quote), `graph`/`flowchart`
TD/TB/BT/RL/LR, `subgraph "Title"` / `subgraph ID ["Title"]` blocks, and one
edge per line with a small set of arrow styles (including bidirectional).
Anything outside that (chained multi-arrow lines, style/classDef/click
directives, a label containing a literal bracket character while unquoted)
fails closed rather than being approximated.
"""

import logging
import re
from dataclasses import dataclass, field
from typing import Dict, List, Optional, Tuple

try:
    import graphviz
except ImportError:  # pragma: no cover - graphviz is a hard runtime dependency in prod
    graphviz = None

LOG = logging.getLogger(__name__)

# Diagram types this module can attempt to render as an image (everything
# else - classDiagram, erDiagram, gantt, sequenceDiagram, timeline, journey -
# stays on the code-block fallback; they either aren't graph-shaped or their
# Mermaid syntax is too different to share this parser).
SUPPORTED_GRAPH_TYPES = {"graph", "flowchart"}

# Safety caps so a pathological/adversarial diagram can't produce a huge
# image or take a long time to lay out - same spirit as mermaid_chart.py's
# _SLACK_MAX_* caps, but bounding render cost rather than Slack's limits.
_MAX_NODES = 60
_MAX_EDGES = 100

_DIRECTIONS = {"TD": "TB", "TB": "TB", "BT": "BT", "RL": "RL", "LR": "LR"}

_HEADER_RE = re.compile(r"^(?:graph|flowchart)\s+(TD|TB|BT|RL|LR)\s*$", re.IGNORECASE)
_COMMENT_RE = re.compile(r"^%%")
_SUBGRAPH_START_RE = re.compile(r'^subgraph\s+(?:"([^"]*)"|\w+\s*\["([^"]*)"\])\s*$')
_SUBGRAPH_END_RE = re.compile(r"^end\s*$")

# Opening bracket -> required closing bracket, for the node shapes the
# VisualizationAgent's prompt documents. Sorted longest-first so a 2-char
# opener (e.g. "((") is tried before its 1-char prefix ("(").
_OPEN_TO_CLOSE = {
    "[(": ")]",  # cylinder
    "((": "))",  # circle
    "{{": "}}",  # hexagon
    "[[": "]]",  # subroutine
    "([": "])",  # stadium
    "[": "]",  # rect
    "(": ")",  # rounded
    "{": "}",  # rhombus
}
_OPENERS = sorted(_OPEN_TO_CLOSE, key=len, reverse=True)
_OPEN_RE = "|".join(re.escape(o) for o in _OPENERS)

# One node token: an alphanumeric/underscore ID, optionally followed by a
# shape-bracketed label - quoted ("...") or unquoted, the same
# quoted-or-unquoted tolerance mermaid_chart.py's xychart/pie parsers already
# use, since real-world generated diagrams don't always quote a label that
# has no spaces/punctuation needing it (the VisualizationAgent's prompt says
# to always quote, but - like xychart/pie before this - real output doesn't
# reliably follow that). Unquoted content excludes quote/bracket characters
# so it can't be ambiguous with the closing bracket that ends it; a label
# that needs a literal bracket character still must be quoted, and simply
# fails closed (see module docstring) if it isn't. A bare ID with no
# brackets refers back to a node declared elsewhere. The closing side is
# matched loosely (any run of closing-bracket chars) and verified against
# _OPEN_TO_CLOSE afterward, rather than building a second alternation -
# equivalent, and considerably simpler to get right.
_UNQUOTED_LABEL = r'[^"\[\(\{\)\}\]]*'
_NODE_TOKEN = r"(\w+)(?:(" + _OPEN_RE + r')(?:"([^"]*)"|(' + _UNQUOTED_LABEL + r"))([\]\)\}]+))?"

# Bidirectional arrows added because real diagrams use them, not because the
# prompt documents them. Capturing (not `(?:...)`) so the matched style
# survives to _build_graphviz via _ARROW_GRAPHVIZ_ATTRS below - Graphviz's
# default edge is always a single forward arrowhead, so without this a
# "---"/"<-->" edge would silently render identical to a plain "-->".
_EDGE_ARROW = r"(<-->|<-\.->|<==>|-->|---|-\.->|-\.-|==>|===)"
# Mermaid arrow style -> Graphviz edge attributes. Omitted keys keep
# Graphviz's own default (dir=forward, solid line).
_ARROW_GRAPHVIZ_ATTRS = {
    "-->": {},
    "-.->": {"style": "dashed"},
    "==>": {"penwidth": "2"},
    "---": {"dir": "none"},
    "-.-": {"dir": "none", "style": "dashed"},
    "===": {"dir": "none", "penwidth": "2"},
    "<-->": {"dir": "both"},
    "<-.->": {"dir": "both", "style": "dashed"},
    "<==>": {"dir": "both", "penwidth": "2"},
}
# Edge label: quoted or unquoted, same convention as node labels - content
# excludes "|" so it can't swallow the closing pipe.
_EDGE_RE = re.compile(rf'^{_NODE_TOKEN}\s*{_EDGE_ARROW}\s*(?:\|(?:"([^"]*)"|([^|]*))\|\s*)?{_NODE_TOKEN}$')
_NODE_DECL_RE = re.compile(rf"^{_NODE_TOKEN}$")

# Mermaid's compound-node edge syntax: either side of an arrow may list
# several nodes joined by " & " (e.g. `A & B --> C` means both A-->C and
# B-->C; `A & B --> C & D` means all four combinations). Captured loosely
# here (each side as raw text, split and validated by _compound_side_tokens
# below) rather than trying to repeat _NODE_TOKEN an unknown number of times
# in one regex, since Python's re only keeps the last match of a repeated
# group anyway. Non-greedy on the left so it stops at the FIRST arrow, in
# case a quoted label elsewhere on the line happens to contain "&".
_COMPOUND_EDGE_RE = re.compile(rf'^(.+?)\s*{_EDGE_ARROW}\s*(?:\|(?:"([^"]*)"|([^|]*))\|\s*)?(.+)$')
_AMP_SPLIT_RE = re.compile(r"\s*&\s*")

_SHAPE_TO_GRAPHVIZ = {
    "[": "box",
    "(": "ellipse",
    "([": "box",  # stadium - Graphviz has no true stadium shape
    "{{": "hexagon",
    "[[": "box",  # subroutine - closest built-in approximation
    "[(": "cylinder",
    "((": "doublecircle",
    "{": "diamond",
}


@dataclass
class _Edge:
    source: str
    target: str
    label: Optional[str]
    arrow: str


@dataclass
class _Subgraph:
    title: str
    node_ids: List[str] = field(default_factory=list)
    children: List["_Subgraph"] = field(default_factory=list)


def _register_node(node_id, opener, quoted_label, unquoted_label, closer, labels, shapes, placed, scope) -> bool:
    """Record a node's label/shape (if this mention carries one) and, on its
    first mention anywhere, place it in the currently open subgraph scope
    for clustering. Returns False on a bracket mismatch (e.g. "[..." closed
    with "}")."""
    if opener is not None:
        if _OPEN_TO_CLOSE.get(opener) != closer:
            return False
        label = quoted_label if quoted_label is not None else unquoted_label
        labels[node_id] = label.replace("<br/>", "\n").replace("<br>", "\n")
        shapes[node_id] = opener
    if node_id not in placed:
        placed.add(node_id)
        scope.node_ids.append(node_id)
    return True


def _compound_side_tokens(raw_side: str) -> Optional[List[Tuple]]:
    """Split one side of a Mermaid `A & B --> ...` compound-node edge into
    its individual node tokens' regex groups, or None if any `&`-separated
    piece isn't a single valid node token on its own."""
    pieces = _AMP_SPLIT_RE.split(raw_side.strip())
    tokens = []
    for piece in pieces:
        piece = piece.strip()
        if not piece:
            return None
        match = _NODE_DECL_RE.match(piece)
        if not match:
            return None
        tokens.append(match.groups())
    return tokens


# Sentinel returned by each _try_* line handler below to mean "this line
# isn't shaped like what I handle - try the next handler", distinct from
# True/False (matched, and succeeded/failed). Keeps _parse_flowchart's main
# loop a flat sequence of "try this shape, then that shape, ..." instead of
# one large branching function - each handler owns its own regex groups and
# _register_node calls.
_NOT_MATCHED = object()


def _try_simple_edge(line: str, labels, shapes, placed, scope, edges: List[_Edge]):
    """One node on each side of the arrow - the common case."""
    edge_match = _EDGE_RE.match(line)
    if not edge_match:
        return _NOT_MATCHED
    (
        a_id,
        a_open,
        a_qlabel,
        a_ulabel,
        a_close,
        arrow,
        edge_qlabel,
        edge_ulabel,
        b_id,
        b_open,
        b_qlabel,
        b_ulabel,
        b_close,
    ) = edge_match.groups()
    ok = _register_node(a_id, a_open, a_qlabel, a_ulabel, a_close, labels, shapes, placed, scope)
    ok = ok and _register_node(b_id, b_open, b_qlabel, b_ulabel, b_close, labels, shapes, placed, scope)
    if not ok:
        return False
    edge_label = edge_qlabel if edge_qlabel is not None else (edge_ulabel or None)
    edges.append(_Edge(a_id, b_id, edge_label, arrow))
    return len(edges) <= _MAX_EDGES


def _try_node_decl(line: str, labels, shapes, placed, scope, edges: List[_Edge]):
    """A standalone node declaration, no arrow. Takes `edges` only to match
    the other handlers' signature for _dispatch_line's uniform call - a
    node declaration never adds one."""
    node_match = _NODE_DECL_RE.match(line)
    if not node_match:
        return _NOT_MATCHED
    node_id, opener, qlabel, ulabel, closer = node_match.groups()
    if opener is None:
        return False  # a bare ID alone on a line isn't meaningful Mermaid
    return _register_node(node_id, opener, qlabel, ulabel, closer, labels, shapes, placed, scope)


def _try_compound_edge(line: str, labels, shapes, placed, scope, edges: List[_Edge]):
    """`A & B --> C & D` (see _COMPOUND_EDGE_RE above) - only reached once
    _try_simple_edge has already failed to match the whole line, so a quoted
    label containing a literal "&" (e.g. "Foo & Bar") is unaffected; it
    already matched as a simple edge."""
    if "&" not in line:
        return _NOT_MATCHED
    compound_match = _COMPOUND_EDGE_RE.match(line)
    if not compound_match:
        return _NOT_MATCHED
    left_raw, arrow, edge_qlabel, edge_ulabel, right_raw = compound_match.groups()
    left_tokens = _compound_side_tokens(left_raw)
    right_tokens = _compound_side_tokens(right_raw)
    if not (left_tokens and right_tokens):
        return _NOT_MATCHED
    ok = True
    for token in left_tokens + right_tokens:
        ok = ok and _register_node(*token, labels, shapes, placed, scope)
    if not ok:
        return False
    edge_label = edge_qlabel if edge_qlabel is not None else (edge_ulabel or None)
    for left_token in left_tokens:
        for right_token in right_tokens:
            edges.append(_Edge(left_token[0], right_token[0], edge_label, arrow))
    return len(edges) <= _MAX_EDGES


_LINE_HANDLERS = (_try_simple_edge, _try_node_decl, _try_compound_edge)


def _dispatch_line(line: str, labels, shapes, placed, scope, edges: List[_Edge]) -> bool:
    """Try each edge/node-declaration line shape in turn. Returns True once
    one handler matches and succeeds, False if none matched (unrecognized -
    fail closed) or a matching handler found the line invalid (e.g. a
    bracket mismatch, or the edge cap exceeded)."""
    for handler in _LINE_HANDLERS:
        result = handler(line, labels, shapes, placed, scope, edges)
        if result is not _NOT_MATCHED:
            return bool(result)
    return False


def _parse_flowchart(
    code: str,
) -> Optional[Tuple[str, _Subgraph, Dict[str, str], Dict[str, str], List[_Edge]]]:
    """Strictly parse Mermaid flowchart/graph syntax into a render-ready
    shape, or None if any line isn't recognized - see the module docstring
    for why this fails closed instead of best-effort."""
    lines = [line.strip() for line in code.splitlines() if line.strip()]
    if not lines:
        return None

    header = _HEADER_RE.match(lines[0])
    if not header:
        return None
    rankdir = _DIRECTIONS[header.group(1).upper()]

    root = _Subgraph(title="")
    stack: List[_Subgraph] = [root]
    labels: Dict[str, str] = {}
    shapes: Dict[str, str] = {}
    placed: set = set()
    edges: List[_Edge] = []

    for line in lines[1:]:
        if _COMMENT_RE.match(line):
            continue

        sub_start = _SUBGRAPH_START_RE.match(line)
        if sub_start:
            title = sub_start.group(1) or sub_start.group(2) or ""
            sg = _Subgraph(title=title)
            stack[-1].children.append(sg)
            stack.append(sg)
            continue

        if _SUBGRAPH_END_RE.match(line):
            if len(stack) == 1:
                return None  # unmatched `end`
            stack.pop()
            continue

        if not _dispatch_line(line, labels, shapes, placed, stack[-1], edges):
            return None  # unrecognized, or matched but invalid - fail closed

    if len(stack) != 1:
        return None  # unclosed subgraph
    if not placed or len(placed) > _MAX_NODES:
        return None

    return rankdir, root, labels, shapes, edges


def _add_scope(
    dot, scope: _Subgraph, labels: Dict[str, str], shapes: Dict[str, str], cluster_counter: List[int]
) -> None:
    for node_id in scope.node_ids:
        label = labels.get(node_id, node_id)
        shape = _SHAPE_TO_GRAPHVIZ.get(shapes.get(node_id, "["), "box")
        dot.node(node_id, label=label, shape=shape)
    for child in scope.children:
        with dot.subgraph(name=f"cluster_{cluster_counter[0]}") as c:
            cluster_counter[0] += 1
            if child.title:
                c.attr(label=child.title)
            _add_scope(c, child, labels, shapes, cluster_counter)


def _build_graphviz(rankdir: str, root: _Subgraph, labels: Dict[str, str], shapes: Dict[str, str], edges: List[_Edge]):
    dot = graphviz.Digraph(format="png")
    dot.attr(rankdir=rankdir)
    dot.attr("node", fontname="Helvetica", fontsize="11")
    dot.attr("edge", fontname="Helvetica", fontsize="10")

    _add_scope(dot, root, labels, shapes, [0])
    for edge in edges:
        attrs = dict(_ARROW_GRAPHVIZ_ATTRS.get(edge.arrow, {}))
        if edge.label:
            attrs["label"] = edge.label
        dot.edge(edge.source, edge.target, **attrs)

    return dot


def render_flowchart_image(code: str) -> Optional[bytes]:
    """Render a Mermaid flowchart/graph as a PNG, or return None if it
    doesn't parse (unsupported syntax) or Graphviz fails to render it -
    either way the caller falls through to the existing code-block
    fallback, so this never needs to raise."""
    if graphviz is None:
        return None
    try:
        parsed = _parse_flowchart(code)
        if parsed is None:
            return None
        rankdir, root, labels, shapes, edges = parsed
        dot = _build_graphviz(rankdir, root, labels, shapes, edges)
        return dot.pipe(format="png")
    except Exception:
        LOG.warning("mermaid_graph: rendering failed, falling back to the code block", exc_info=True)
        return None
