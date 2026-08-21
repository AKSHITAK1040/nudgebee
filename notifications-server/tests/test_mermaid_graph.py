import shutil

import pytest

from notifications_server.utils.mermaid_graph import (
    _MAX_NODES,
    _build_graphviz,
    _parse_flowchart,
    render_flowchart_image,
)

DOT_MISSING = shutil.which("dot") is None


class TestParseFlowchart:
    def test_simple_edge_parses(self):
        parsed = _parse_flowchart('graph TD\n    S1["API Gateway"] --> S2["Auth Service"]\n')
        assert parsed is not None
        rankdir, root, labels, shapes, edges = parsed
        assert rankdir == "TB"
        assert labels == {"S1": "API Gateway", "S2": "Auth Service"}
        assert [(*e.__dict__.values(),) for e in edges] == [("S1", "S2", None, "-->")]

    def test_lr_direction_maps_through(self):
        parsed = _parse_flowchart('graph LR\n    A["a"] --> B["b"]\n')
        assert parsed[0] == "LR"

    def test_subgraph_groups_its_nodes(self):
        code = (
            "graph TD\n"
            '    subgraph "Service Mesh"\n'
            '        S1["API Gateway"] --> S2["Auth Service"]\n'
            '        S2 --> DB1[("User DB")]\n'
            "    end\n"
        )
        parsed = _parse_flowchart(code)
        assert parsed is not None
        _, root, labels, shapes, edges = parsed
        assert len(root.children) == 1
        cluster = root.children[0]
        assert cluster.title == "Service Mesh"
        assert cluster.node_ids == ["S1", "S2", "DB1"]
        assert labels["DB1"] == "User DB"
        assert shapes["DB1"] == "[("

    def test_nested_subgraphs(self):
        code = (
            "graph TD\n"
            '    subgraph "Outer"\n'
            '        A["a"] --> B["b"]\n'
            '        subgraph "Inner"\n'
            '            C["c"] --> D["d"]\n'
            "        end\n"
            "    end\n"
        )
        parsed = _parse_flowchart(code)
        assert parsed is not None
        _, root, *_ = parsed
        outer = root.children[0]
        assert outer.title == "Outer"
        assert outer.node_ids == ["A", "B"]
        inner = outer.children[0]
        assert inner.title == "Inner"
        assert inner.node_ids == ["C", "D"]

    def test_edge_label_is_captured(self):
        parsed = _parse_flowchart('graph LR\n    A["Start"] -->|"yes"| B["End"]\n')
        assert parsed is not None
        edges = parsed[4]
        assert edges[0].label == "yes"

    def test_bare_reference_to_already_labeled_node(self):
        code = 'graph TD\n    A["Start"] --> B["Mid"]\n    B --> C["End"]\n'
        parsed = _parse_flowchart(code)
        assert parsed is not None
        _, _, labels, _, edges = parsed
        assert labels["B"] == "Mid"
        assert [(e.source, e.target) for e in edges] == [("A", "B"), ("B", "C")]

    def test_arrow_without_surrounding_spaces_parses(self):
        assert _parse_flowchart('graph LR\n    A["x"]-->B["y"]\n') is not None

    def test_multiline_label_break_is_normalized(self):
        parsed = _parse_flowchart('graph TD\n    A["Line1<br/>Line2"] --> B["b"]\n')
        assert parsed[2]["A"] == "Line1\nLine2"

    def test_unquoted_node_label_is_parsed(self):
        # Real generated diagrams don't always quote a label, same tolerance
        # mermaid_chart.py's xychart/pie parsers already have (see module
        # docstring) - unquoted content just can't contain bracket chars.
        parsed = _parse_flowchart("graph TD\n    A[Start] --> B[End]\n")
        assert parsed is not None
        assert parsed[2] == {"A": "Start", "B": "End"}

    def test_unquoted_cylinder_label_with_punctuation(self):
        # Exact real-world shape that motivated this: DB[(PostgreSQL
        # Database: nudgebee)] - unquoted, contains a colon and spaces.
        parsed = _parse_flowchart("graph TD\n    A[Start] --> DB[(PostgreSQL Database: nudgebee)]\n")
        assert parsed is not None
        assert parsed[2]["DB"] == "PostgreSQL Database: nudgebee"
        assert parsed[3]["DB"] == "[("

    def test_unquoted_edge_label_is_parsed(self):
        parsed = _parse_flowchart('graph TD\n    A["Start"] -->|HTTPS| B["End"]\n')
        assert parsed is not None
        assert parsed[4][0].label == "HTTPS"

    def test_edge_label_with_slash_and_space(self):
        parsed = _parse_flowchart('graph TD\n    A["Start"] -->|WebSocket / HTTP| B["End"]\n')
        assert parsed is not None
        assert parsed[4][0].label == "WebSocket / HTTP"

    def test_bidirectional_arrow_is_parsed(self):
        parsed = _parse_flowchart('graph TD\n    A["Start"] <--> B["End"]\n')
        assert parsed is not None
        assert [(e.source, e.target) for e in parsed[4]] == [("A", "B")]
        assert parsed[4][0].arrow == "<-->"

    def test_bidirectional_arrow_with_unquoted_label(self):
        parsed = _parse_flowchart('graph TD\n    A["Start"] <-->|WebSocket / HTTP| B["End"]\n')
        assert parsed is not None
        assert parsed[4][0].label == "WebSocket / HTTP"
        assert parsed[4][0].arrow == "<-->"

    def test_undirected_arrow_is_parsed(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] --- B["b"]\n')
        assert parsed is not None
        assert parsed[4][0].arrow == "---"

    def test_dotted_arrow_is_parsed(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] -.-> B["b"]\n')
        assert parsed is not None
        assert parsed[4][0].arrow == "-.->"

    def test_compound_edge_left_side_expands_to_two_edges(self):
        # Mermaid's `A & B --> C` means both A-->C and B-->C, not a single
        # node literally named "A & B" - real-world shape that motivated
        # this (a K8s architecture diagram: `Pods & Nodes --> OTel`).
        code = 'graph TD\n    A["a"] --> B["b"]\n    C["c"] --> D["d"]\n    B & D --> E["e"]\n'
        parsed = _parse_flowchart(code)
        assert parsed is not None
        edges = [(e.source, e.target) for e in parsed[4]]
        assert ("B", "E") in edges
        assert ("D", "E") in edges

    def test_compound_edge_right_side_expands_to_two_edges(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] --> B["b"] & C["c"]\n')
        assert parsed is not None
        edges = [(e.source, e.target) for e in parsed[4]]
        assert set(edges) == {("A", "B"), ("A", "C")}

    def test_compound_edge_both_sides_is_full_cross_product(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] & B["b"] --> C["c"] & D["d"]\n')
        assert parsed is not None
        edges = {(e.source, e.target) for e in parsed[4]}
        assert edges == {("A", "C"), ("A", "D"), ("B", "C"), ("B", "D")}

    def test_compound_edge_label_applies_to_every_expanded_edge(self):
        parsed = _parse_flowchart(
            'graph TD\n    A["a"] --> B["b"]\n    C["c"] --> D["d"]\n    B & D -->|shared| E["e"]\n'
        )
        assert parsed is not None
        labels = {(e.source, e.target): e.label for e in parsed[4]}
        assert labels[("B", "E")] == "shared"
        assert labels[("D", "E")] == "shared"

    def test_compound_edge_with_bare_references(self):
        # Both sides reference already-declared nodes (no inline brackets) -
        # the shape used in the diagram that motivated this fix.
        code = 'graph TD\n    A["a"] --> B["b"]\n    C["c"] --> D["d"]\n    E["e"] --> F["f"]\n    B & D --> E\n'
        parsed = _parse_flowchart(code)
        assert parsed is not None
        edges = {(e.source, e.target) for e in parsed[4]}
        assert ("B", "E") in edges
        assert ("D", "E") in edges

    def test_quoted_label_containing_ampersand_is_unaffected(self):
        # Regression guard: a quoted label with a literal "&" must still
        # match the plain single-node _EDGE_RE (as it always did) rather
        # than being misrouted into the compound-edge path.
        parsed = _parse_flowchart('graph TD\n    A["Foo & Bar"] --> B["b"]\n')
        assert parsed is not None
        assert parsed[2]["A"] == "Foo & Bar"
        assert [(e.source, e.target) for e in parsed[4]] == [("A", "B")]

    def test_invalid_compound_piece_fails_closed(self):
        # One side has an unquoted label with a bracket char - invalid on
        # its own, so the whole compound edge (and thus the diagram) fails
        # closed rather than silently dropping that piece.
        assert _parse_flowchart('graph TD\n    A[a (b)] & B["b"] --> C["c"]\n') is None

    @pytest.mark.parametrize(
        "code",
        [
            "just plain text, not mermaid at all",
            'graph TD\n    classDef highlight fill:#f00\n    A["a"] --> B["b"]\n',
            'graph TD\n    A["a"] --> B["b"] --> C["c"]\n',  # chained multi-arrow: out of scope
            "graph TD\n    A[a (b)] --> B[c]\n",  # unquoted label with a bracket char: still out of scope
            "graph TD\n",  # header only, no nodes
            'graph TD\n    A["a"] --> B["b"]\n    end\n',  # unmatched `end`
            'graph TD\n    subgraph "X"\n        A["a"] --> B["b"]\n',  # unclosed subgraph
            "sequenceDiagram\n    Alice->>Bob: Hi\n",  # not a supported diagram type at all
        ],
    )
    def test_unsupported_or_malformed_syntax_fails_closed(self, code):
        # The parser is deliberately all-or-nothing (see mermaid_graph.py's
        # module docstring): anything it doesn't fully understand must
        # return None, never a partial/best-effort graph.
        assert _parse_flowchart(code) is None

    def test_oversized_diagram_is_rejected(self):
        nodes = "\n".join(f'    N{i}["Node {i}"] --> N{i + 1}["Node {i + 1}"]' for i in range(_MAX_NODES + 10))
        code = f"flowchart TD\n{nodes}\n"
        assert _parse_flowchart(code) is None


class TestBuildGraphviz:
    def test_produces_valid_dot_source_with_cluster(self):
        parsed = _parse_flowchart(
            "graph TD\n"
            '    subgraph "Service Mesh"\n'
            '        S1["API Gateway"] --> S2["Auth Service"]\n'
            "    end\n"
        )
        rankdir, root, labels, shapes, edges = parsed
        dot = _build_graphviz(rankdir, root, labels, shapes, edges)
        source = dot.source
        assert "cluster_0" in source
        assert 'label="Service Mesh"' in source
        assert "S1 -> S2" in source

    def test_plain_arrow_gets_default_forward_edge(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] --> B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "dir=both" not in dot.source
        assert "dir=none" not in dot.source
        assert "style=" not in dot.source

    def test_bidirectional_arrow_gets_dir_both(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] <--> B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "dir=both" in dot.source

    def test_undirected_arrow_gets_dir_none(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] --- B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "dir=none" in dot.source

    def test_dotted_arrow_gets_dashed_style(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] -.-> B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "style=dashed" in dot.source

    def test_dotted_undirected_arrow_gets_both_attributes(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] -.- B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "dir=none" in dot.source
        assert "style=dashed" in dot.source

    def test_thick_bidirectional_arrow_gets_dir_both_and_penwidth(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] <==> B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "dir=both" in dot.source
        assert "penwidth=2" in dot.source

    def test_edge_label_survives_alongside_arrow_attributes(self):
        parsed = _parse_flowchart('graph TD\n    A["a"] <-->|"sync"| B["b"]\n')
        dot = _build_graphviz(*parsed)
        assert "dir=both" in dot.source
        assert "label=sync" in dot.source or 'label="sync"' in dot.source


class TestRenderFlowchartImage:
    def test_unparseable_code_returns_none_without_touching_graphviz(self):
        assert render_flowchart_image("not a flowchart") is None

    @pytest.mark.skipif(DOT_MISSING, reason="requires the `dot` binary (apk/apt-get install graphviz)")
    def test_valid_flowchart_renders_png_bytes(self):
        png = render_flowchart_image('graph TD\n    A["Start"] --> B["End"]\n')
        assert png is not None
        assert png[:8] == b"\x89PNG\r\n\x1a\n"  # PNG file signature
