"""Tests for scoping a Confluence scrape to page trees instead of a space.

A page tree is walked from its root through ``get_child_pages``. Unlike the
space walk, nothing else seeds the queue, so the walk itself must find every
descendant — including those beneath a container page with no body.
"""

from types import SimpleNamespace

import rag.core.documents.scraper as scraper
from rag.core.documents.scraper import (
    collect_confluence_tree_documents,
    configured_page_trees,
)


def fake_confluence(pages, children):
    """pages: id -> body html (None = unreadable); children: id -> [ids]."""

    def get_page_by_id(page_id, expand=None):
        if page_id not in pages or pages[page_id] is None:
            raise RuntimeError(f"HTTP 404 for {page_id}")
        return {
            "id": page_id,
            "body": {"storage": {"value": pages[page_id]}},
            "_links": {"base": "https://wiki.example.com", "webui": f"/pages/{page_id}"},
        }

    def get_child_pages(page_id):
        return [{"id": child} for child in children.get(page_id, [])]

    return SimpleNamespace(get_page_by_id=get_page_by_id, get_child_pages=get_child_pages)


def test_configured_page_trees_splits_and_trims():
    assert configured_page_trees({"page_trees": " 100, 200 ,,300"}) == ["100", "200", "300"]
    assert configured_page_trees({"page_trees": ""}) == []
    assert configured_page_trees({}) == []


def test_tree_walk_descends_through_empty_container_pages():
    # A container-page layout: the root and a section page have no body of
    # their own, and the runbooks live several levels beneath them.
    confluence = fake_confluence(
        pages={"root": "", "section": "", "runbook": "<p>restart the pods</p>", "deep": "<p>rotate keys</p>"},
        children={"root": ["section"], "section": ["runbook"], "runbook": ["deep"]},
    )
    stats = {"failed_pages": 0, "empty_spaces": 0, "failed_roots": 0}

    documents = collect_confluence_tree_documents(confluence, "root", stats)

    assert sorted(d.page_content for d in documents) == ["restart the pods", "rotate keys"]
    assert all(d.metadata["tree_root"] == "root" for d in documents)
    assert stats == {"failed_pages": 0, "empty_spaces": 0, "failed_roots": 0}


def test_tree_walk_stays_inside_the_tree_and_visits_each_page_once():
    confluence = fake_confluence(
        pages={"a": "<p>a</p>", "b": "<p>b</p>", "c": "<p>c</p>", "outside": "<p>x</p>"},
        # b is linked from two parents; nothing points at "outside".
        children={"a": ["b", "c"], "c": ["b"]},
    )
    stats = {"failed_pages": 0, "empty_spaces": 0, "failed_roots": 0}

    documents = collect_confluence_tree_documents(confluence, "a", stats)

    assert sorted(d.metadata["page_id"] for d in documents) == ["a", "b", "c"]


def test_unreadable_root_is_reported_not_treated_as_empty():
    confluence = fake_confluence(pages={"root": None}, children={})
    stats = {"failed_pages": 0, "empty_spaces": 0, "failed_roots": 0}

    assert collect_confluence_tree_documents(confluence, "root", stats) == []
    assert stats["failed_roots"] == 1


def test_integration_with_page_trees_never_walks_spaces(monkeypatch):
    confluence = fake_confluence(
        pages={"100": "<p>Runbooks</p>", "101": "<p>child</p>"},
        children={"100": ["101"]},
    )
    monkeypatch.setattr(scraper, "build_confluence_client", lambda config: confluence)
    monkeypatch.setattr(scraper, "fetch_all_spaces", lambda c: (_ for _ in ()).throw(AssertionError("spaces walked")))
    monkeypatch.setattr(scraper, "get_all_pages_from_space", lambda *a, **k: [], raising=False)
    embedded = {}

    def process_documents(documents, embeddings, **kwargs):
        embedded["ids"] = [d.metadata["page_id"] for d in documents]
        return kwargs["collection_name"], embedded["ids"]

    results = []
    monkeypatch.setattr(scraper, "process_documents", process_documents)
    monkeypatch.setattr(scraper, "update_integration_kb_load_result", lambda *a, **k: results.append((a, k)))

    integration = {"integration_id": "int-1", "config": {"namespace": "SRE", "page_trees": "100"}}
    ids = scraper._process_integration(integration, "tenant-1", embeddings=None)

    assert sorted(ids) == ["100", "101"]
    assert results[-1][0][:2] == ("int-1", "active")


def test_integration_with_unreadable_tree_ends_in_error(monkeypatch):
    confluence = fake_confluence(pages={"100": "<p>ok</p>", "200": None}, children={})
    monkeypatch.setattr(scraper, "build_confluence_client", lambda config: confluence)
    results = []
    monkeypatch.setattr(scraper, "update_integration_kb_load_result", lambda *a, **k: results.append((a, k)))
    monkeypatch.setattr(scraper, "process_documents", lambda *a, **k: (_ for _ in ()).throw(AssertionError("embedded")))

    integration = {"integration_id": "int-1", "config": {"page_trees": "100,200"}}
    assert scraper._process_integration(integration, "tenant-1", embeddings=None) == []

    args, kwargs = results[-1]
    assert args[:2] == ("int-1", "error")
    assert "1 of 2 configured Confluence page trees could not be read" in kwargs["error_message"]
