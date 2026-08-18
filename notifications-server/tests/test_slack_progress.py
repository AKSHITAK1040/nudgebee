import threading
from unittest.mock import MagicMock, patch

import pytest

from notifications_server.configs import settings
from notifications_server.services import slack_progress


@pytest.fixture(autouse=True)
def reset_poller_counter():
    slack_progress._active_pollers = 0
    yield
    slack_progress._active_pollers = 0


def _entry(**overrides):
    entry = {
        "team_id": "T111",
        "channel_id": "C222",
        "slack_user_id": "U333",
        "session_id": "C222-1000.1",
        "account_id": "acc-1",
        "tenant_id": "tenant-1",
        "user_id": "user-1",
    }
    entry.update(overrides)
    return entry


def _tool_row(row_id, status, tool_name="get_pod_logs", thought="checking logs", updated_at="2026-08-14T10:00:00Z"):
    return {"id": row_id, "status": status, "tool_name": tool_name, "thought": thought, "updated_at": updated_at}


class TestBuildChunks:
    def test_new_in_progress_tool_emits_bare_task(self):
        sent = {}
        chunks = slack_progress._build_chunks([_tool_row("t1", "IN_PROGRESS")], sent)
        assert chunks == [
            {
                "type": "task_update",
                "id": "t1",
                "title": "Get pod logs",
                "status": "in_progress",
            }
        ]
        assert sent == {"t1": "in_progress"}

    def test_status_transitions_and_dedupe(self):
        sent = {}
        slack_progress._build_chunks([_tool_row("t1", "IN_PROGRESS")], sent)
        # Same status again: nothing new to send.
        assert slack_progress._build_chunks([_tool_row("t1", "IN_PROGRESS")], sent) == []
        # Terminal transition goes out once, then never repeats or downgrades.
        chunks = slack_progress._build_chunks([_tool_row("t1", "SUCCESS")], sent)
        assert [c["status"] for c in chunks] == ["complete"]
        assert slack_progress._build_chunks([_tool_row("t1", "SUCCESS")], sent) == []
        assert slack_progress._build_chunks([_tool_row("t1", "IN_PROGRESS")], sent) == []

    def test_status_mapping(self):
        sent = {}
        rows = [
            _tool_row("a", "SUCCESS"),
            _tool_row("b", "EMPTY_RESULT"),
            _tool_row("c", "ERROR"),
            _tool_row("d", "FAILURE"),
            _tool_row("e", "TERMINATED"),
            _tool_row("f", "WAITING"),
            _tool_row("g", "something_unknown"),
        ]
        statuses = {c["id"]: c["status"] for c in slack_progress._build_chunks(rows, sent)}
        assert statuses == {
            "a": "complete",
            "b": "complete",
            "c": "error",
            "d": "error",
            "e": "error",
            "f": "in_progress",
            "g": "in_progress",
        }

    def test_rows_sorted_by_updated_at_and_title_truncated(self):
        sent = {}
        rows = [
            _tool_row("later", "IN_PROGRESS", updated_at="2026-08-14T10:00:02Z"),
            _tool_row("earlier", "IN_PROGRESS", tool_name="x" * 500, updated_at="2026-08-14T10:00:01Z"),
        ]
        chunks = slack_progress._build_chunks(rows, sent)
        assert [c["id"] for c in chunks] == ["earlier", "later"]
        assert len(chunks[0]["title"]) == slack_progress._TASK_FIELD_LIMIT

    def test_rows_without_id_are_skipped_and_thought_never_rendered(self):
        sent = {}
        chunks = slack_progress._build_chunks(
            [{"status": "IN_PROGRESS"}, _tool_row("t1", "IN_PROGRESS", thought="internal reasoning")], sent
        )
        assert len(chunks) == 1
        assert "details" not in chunks[0]

    def test_none_tool_calls(self):
        assert slack_progress._build_chunks(None, {}) == []


class TestTaskTitle:
    def test_humanizes_snake_case(self):
        assert slack_progress._task_title("get_pod_logs") == "Get pod logs"

    def test_empty_name_falls_back(self):
        assert slack_progress._task_title(None) == "Working"
        assert slack_progress._task_title("___") == "Working"


class TestStartProgressPoller:
    def test_missing_required_field_spawns_nothing(self):
        with patch.object(threading, "Thread") as thread_cls:
            slack_progress.start_progress_poller(MagicMock(), _entry(slack_user_id=None), "1000.1", "C222-1000.1")
        thread_cls.assert_not_called()

    def test_spawns_daemon_thread_with_payload_session_id(self):
        with patch.object(threading, "Thread") as thread_cls:
            slack_progress.start_progress_poller(MagicMock(), _entry(session_id="stale"), "1000.1", "event-abc")
        assert thread_cls.call_args.kwargs["daemon"] is True
        passed_entry = thread_cls.call_args.kwargs["args"][1]
        assert passed_entry["session_id"] == "event-abc"

    def test_poller_cap_blocks_new_pollers(self):
        with patch.object(settings.slack, "thinking_steps_max_pollers", 0):
            with patch.object(threading, "Thread") as thread_cls:
                slack_progress.start_progress_poller(MagicMock(), _entry(), "1000.1", "C222-1000.1")
        thread_cls.assert_not_called()

    def test_never_raises(self):
        with patch.object(threading, "Thread", side_effect=RuntimeError("boom")):
            slack_progress.start_progress_poller(MagicMock(), _entry(), "1000.1", "C222-1000.1")


class TestStopProgressStream:
    def test_no_stream_key_is_a_noop(self):
        common = MagicMock()
        with patch.object(slack_progress, "Cache") as cache_cls:
            cache_cls.return_value.get_event_entry.return_value = {"channel_id": "C222"}
            slack_progress.stop_progress_stream(common, None, "C222", "T111", "1000.1")
        common.get_slack_installation.assert_not_called()

    def test_clears_key_before_stopping(self):
        common = MagicMock()
        common.get_slack_installation.return_value.token = "xoxb-test"
        calls = []
        with patch.object(slack_progress, "Cache") as cache_cls:
            cache = cache_cls.return_value
            cache.get_event_entry.return_value = {"stream_ts": "2000.2"}
            cache.remove_event_keys.side_effect = lambda *a: calls.append("clear")
            common.slack_app.client.stop_stream.side_effect = lambda **kw: calls.append("stop")
            slack_progress.stop_progress_stream(common, None, "C222", "T111", "1000.1")
        assert calls == ["clear", "stop"]
        common.slack_app.client.stop_stream.assert_called_once_with(token="xoxb-test", channel_id="C222", ts="2000.2")
        # No progress_since on the entry: nothing to catch up from, so no flush.
        common.slack_app.client.append_stream.assert_not_called()

    def test_flushes_final_delta_before_stopping(self):
        common = MagicMock()
        common.get_slack_installation.return_value.token = "xoxb-test"
        calls = []
        delta = {"tool_calls": [_tool_row("t1", "SUCCESS")]}
        with patch.object(slack_progress, "Cache") as cache_cls:
            cache = cache_cls.return_value
            cache.get_event_entry.return_value = {"stream_ts": "2000.2", "progress_since": "2026-08-14T10:00:00Z"}
            with patch.object(slack_progress, "_fetch_delta", return_value=delta):
                common.slack_app.client.append_stream.side_effect = lambda **kw: calls.append(("append", kw))
                common.slack_app.client.stop_stream.side_effect = lambda **kw: calls.append(("stop", kw))
                slack_progress.stop_progress_stream(common, None, "C222", "T111", "1000.1")
        assert [c[0] for c in calls] == ["append", "stop"]
        appended_chunks = calls[0][1]["chunks"]
        assert appended_chunks == [
            {"type": "task_update", "id": "t1", "title": "Get pod logs", "status": "complete"},
            {
                "type": "task_update",
                "id": slack_progress._INITIAL_TASK_ID,
                "title": slack_progress._INITIAL_TASK_TITLE,
                "status": "complete",
            },
        ]
        assert calls[0][1]["ts"] == "2000.2"

    def test_flush_failure_still_stops_the_stream(self):
        common = MagicMock()
        common.get_slack_installation.return_value.token = "xoxb-test"
        with patch.object(slack_progress, "Cache") as cache_cls:
            cache = cache_cls.return_value
            cache.get_event_entry.return_value = {"stream_ts": "2000.2", "progress_since": "2026-08-14T10:00:00Z"}
            with patch.object(slack_progress, "_fetch_delta", side_effect=RuntimeError("boom")):
                slack_progress.stop_progress_stream(common, None, "C222", "T111", "1000.1")
        common.slack_app.client.stop_stream.assert_called_once_with(token="xoxb-test", channel_id="C222", ts="2000.2")

    def test_falls_back_to_passed_entry_and_never_raises(self):
        common = MagicMock()
        common.slack_app.client.stop_stream.side_effect = RuntimeError("already stopped")
        with patch.object(slack_progress, "Cache") as cache_cls:
            cache_cls.return_value.get_event_entry.return_value = None
            slack_progress.stop_progress_stream(common, {"stream_ts": "2000.2"}, "C222", "T111", "1000.1")
        common.slack_app.client.stop_stream.assert_called_once()

    def test_pending_claim_clears_key_without_slack_call(self):
        common = MagicMock()
        with patch.object(slack_progress, "Cache") as cache_cls:
            cache = cache_cls.return_value
            cache.get_event_entry.return_value = {"stream_ts": "pending-fixed"}
            slack_progress.stop_progress_stream(common, None, "C222", "T111", "1000.1")
        cache.remove_event_keys.assert_called_once_with("1000.1", ["stream_ts", "progress_since"])
        common.slack_app.client.stop_stream.assert_not_called()


class TestPollLifecycle:
    def _common(self):
        common = MagicMock()
        common.get_slack_installation.return_value.token = "xoxb-test"
        common.slack_app.client.start_stream.return_value = {"ts": "3000.3"}
        return common

    def _run(self, common, cache, deltas=()):
        """Run _poll with instant sleeps, a fixed claim token, and canned deltas."""
        fetches = iter(deltas)
        with patch.object(slack_progress, "Cache", return_value=cache):
            with patch.object(slack_progress.time, "sleep"):
                with patch.object(slack_progress.uuid, "uuid4", return_value=MagicMock(hex="fixed")):
                    with patch.object(slack_progress, "_fetch_delta", side_effect=lambda *a: next(fetches, None)):
                        slack_progress._poll(common, _entry(), "1000.1")

    # _poll reads the cache entry: before starting (leftover check), after
    # startStream (claim re-check), once per loop iteration, and in finally.

    def test_superseded_stream_is_stopped_before_starting_a_new_one(self):
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{"stream_ts": "old.1"}]
        cache.update_event_entry.return_value = False
        self._run(common, cache)
        stopped = [c.kwargs["ts"] for c in common.slack_app.client.stop_stream.call_args_list]
        assert stopped == ["old.1"]
        common.slack_app.client.start_stream.assert_not_called()

    def test_exits_without_stopping_when_settle_took_the_key(self):
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [
            {},
            {"stream_ts": "pending-fixed"},
            {"stream_ts": "different"},
            {"stream_ts": "different"},
        ]
        cache.update_event_entry.return_value = True
        self._run(common, cache)
        common.slack_app.client.stop_stream.assert_not_called()

    def test_settle_racing_startstream_still_stops_the_stream(self):
        common = self._common()
        cache = MagicMock()
        # The pending claim is gone by the post-start re-check: the settle
        # handler cleared it while chat.startStream was in flight.
        cache.get_event_entry.side_effect = [{}, None]
        cache.update_event_entry.return_value = True
        self._run(common, cache)
        common.slack_app.client.stop_stream.assert_called_once()
        assert common.slack_app.client.stop_stream.call_args.kwargs["ts"] == "3000.3"

    def test_expired_entry_mid_run_still_stops_the_stream(self):
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{}, {"stream_ts": "pending-fixed"}, None, None]
        cache.update_event_entry.return_value = True
        self._run(common, cache)
        common.slack_app.client.stop_stream.assert_called_once()
        assert common.slack_app.client.stop_stream.call_args.kwargs["ts"] == "3000.3"

    def test_terminal_conversation_stops_own_stream_when_settle_never_came(self):
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [
            {},
            {"stream_ts": "pending-fixed"},
            {"stream_ts": "3000.3"},
            {"stream_ts": "3000.3"},
        ]
        cache.update_event_entry.return_value = True
        # A zero-tool-call turn (e.g. "hi") still has to show up in `messages`
        # for the terminal status to be trusted — an empty delta alongside
        # COMPLETED would now be treated as a stale read from a prior turn.
        delta = {
            "conversation": {"status": "COMPLETED"},
            "messages": [{"id": "m1"}],
            "tool_calls": [],
            "cursor": "c1",
        }
        # Same delta twice: the main loop's poll, then the finally block's
        # own re-fetch from turn start once it decides to close the panel.
        self._run(common, cache, deltas=[delta, delta])
        common.slack_app.client.stop_stream.assert_called_once()
        cache.remove_event_keys.assert_called_with("1000.1", ["stream_ts", "progress_since"])
        start_chunks = common.slack_app.client.start_stream.call_args.kwargs["chunks"]
        assert start_chunks == [
            {"type": "plan_update", "title": "Thinking"},
            {
                "type": "task_update",
                "id": slack_progress._INITIAL_TASK_ID,
                "title": slack_progress._INITIAL_TASK_TITLE,
                "status": "in_progress",
            },
        ]
        # No tool calls, so the main loop's own poll has nothing to append —
        # the synthetic task stays in_progress instead of completing early.
        # Only the finally block's flush, right before the panel closes,
        # resolves it to complete.
        appended = common.slack_app.client.append_stream.call_args_list
        assert len(appended) == 1
        assert appended[0].kwargs["chunks"] == [
            {
                "type": "task_update",
                "id": slack_progress._INITIAL_TASK_ID,
                "title": slack_progress._INITIAL_TASK_TITLE,
                "status": "complete",
            }
        ]

    def test_appends_tool_chunks_from_delta(self):
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{}, {"stream_ts": "pending-fixed"}] + [{"stream_ts": "3000.3"}] * 3
        cache.update_event_entry.return_value = True
        deltas = [
            {"conversation": {"status": "IN_PROGRESS"}, "tool_calls": [_tool_row("t1", "IN_PROGRESS")], "cursor": "c1"},
            {"conversation": {"status": "COMPLETED"}, "tool_calls": [_tool_row("t1", "SUCCESS")], "cursor": "c2"},
        ]
        self._run(common, cache, deltas=deltas)
        appended = common.slack_app.client.append_stream.call_args_list
        assert [c.kwargs["chunks"][0]["status"] for c in appended] == ["in_progress", "complete"]
        assert all(c.kwargs["ts"] == "3000.3" for c in appended)

    def test_synthetic_task_reopens_in_gaps_between_real_tools(self):
        """Between t1 finishing and t2 starting, nothing real is in_progress —
        the synthetic task must reopen (relabeled, since real activity has
        started) rather than leaving the panel with everything checked off."""
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{}, {"stream_ts": "pending-fixed"}] + [{"stream_ts": "3000.3"}] * 5
        cache.update_event_entry.return_value = True
        t1_running = {
            "conversation": {"status": "IN_PROGRESS"},
            "tool_calls": [_tool_row("t1", "IN_PROGRESS")],
            "cursor": "c1",
        }
        t1_done = {
            "conversation": {"status": "IN_PROGRESS"},
            "tool_calls": [_tool_row("t1", "SUCCESS")],
            "cursor": "c2",
        }
        t2_running = {
            "conversation": {"status": "IN_PROGRESS"},
            "tool_calls": [_tool_row("t2", "IN_PROGRESS")],
            "cursor": "c3",
        }
        t2_done = {"conversation": {"status": "COMPLETED"}, "tool_calls": [_tool_row("t2", "SUCCESS")], "cursor": "c4"}
        # t2_done twice: once for the main loop, once for the finally block's
        # own re-fetch once it decides the terminal status closes the panel.
        self._run(common, cache, deltas=[t1_running, t1_done, t2_running, t2_done, t2_done])
        appended = common.slack_app.client.append_stream.call_args_list
        starting = slack_progress._INITIAL_TASK_ID

        def placeholder_chunk(call):
            return next(c for c in call.kwargs["chunks"] if c["id"] == starting)

        # t1 starts: synthetic task settles under its original title.
        assert placeholder_chunk(appended[0]) == {
            "type": "task_update",
            "id": starting,
            "title": slack_progress._INITIAL_TASK_TITLE,
            "status": "complete",
        }
        # t1 finishes, nothing else active: reopens, relabeled.
        assert placeholder_chunk(appended[1]) == {
            "type": "task_update",
            "id": starting,
            "title": slack_progress._CONTINUING_TASK_TITLE,
            "status": "in_progress",
        }
        # t2 starts: settles again, keeping the relabeled title (no flapping
        # back to "Understanding your query").
        assert placeholder_chunk(appended[2]) == {
            "type": "task_update",
            "id": starting,
            "title": slack_progress._CONTINUING_TASK_TITLE,
            "status": "complete",
        }
        # t2 finishes and the turn is over, but this poll iteration still
        # sees nothing active — reopens once more before the loop exits.
        assert placeholder_chunk(appended[3]) == {
            "type": "task_update",
            "id": starting,
            "title": slack_progress._CONTINUING_TASK_TITLE,
            "status": "in_progress",
        }
        # The finally block's flush is what actually closes the panel out.
        assert placeholder_chunk(appended[4]) == {
            "type": "task_update",
            "id": starting,
            "title": slack_progress._INITIAL_TASK_TITLE,
            "status": "complete",
        }
        common.slack_app.client.stop_stream.assert_called_once()

    def test_waiting_for_client_tool_does_not_stop_the_panel_mid_run(self):
        """A delegate/sub-agent step can leave the conversation reading WAITING
        or WAITING_FOR_CLIENT_TOOL without a real end-user follow-up pending —
        neither is terminal in llm-server's own model, so the poller must keep
        going past it instead of closing the panel mid-investigation."""
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{}, {"stream_ts": "pending-fixed"}] + [{"stream_ts": "3000.3"}] * 4
        cache.update_event_entry.return_value = True
        deltas = [
            {
                "conversation": {"status": "WAITING_FOR_CLIENT_TOOL"},
                "tool_calls": [_tool_row("t1", "IN_PROGRESS")],
                "cursor": "c1",
            },
            {"conversation": {"status": "WAITING"}, "tool_calls": [_tool_row("t1", "SUCCESS")], "cursor": "c2"},
            {"conversation": {"status": "COMPLETED"}, "tool_calls": [_tool_row("t2", "SUCCESS")], "cursor": "c3"},
        ]
        self._run(common, cache, deltas=deltas)
        appended = common.slack_app.client.append_stream.call_args_list
        # All three deltas were processed — the two non-terminal-for-llm-server
        # statuses didn't cut the loop short — and the panel only closes once,
        # on the truly terminal COMPLETED delta.
        assert len(appended) == 3
        common.slack_app.client.stop_stream.assert_called_once()

    def test_stale_completed_status_before_turn_activity_is_ignored(self):
        """On a reused Slack thread, the very first poll can still read
        COMPLETED left over from the previous turn (llm-server's async
        endpoint returns 202 before a worker dequeues the request and flips
        the row's status). That stale read must not close the panel — only a
        COMPLETED seen after real activity (a message or tool call) for this
        turn should."""
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{}, {"stream_ts": "pending-fixed"}] + [{"stream_ts": "3000.3"}] * 4
        cache.update_event_entry.return_value = True
        deltas = [
            {"conversation": {"status": "COMPLETED"}, "tool_calls": [], "cursor": "c1"},
            {"conversation": {"status": "IN_PROGRESS"}, "tool_calls": [_tool_row("t1", "IN_PROGRESS")], "cursor": "c2"},
            {"conversation": {"status": "COMPLETED"}, "tool_calls": [_tool_row("t1", "SUCCESS")], "cursor": "c3"},
        ]
        self._run(common, cache, deltas=deltas)
        appended = common.slack_app.client.append_stream.call_args_list
        # All three deltas were processed — the stale COMPLETED on the first
        # poll didn't cut the loop short — and the panel only closes once,
        # on the COMPLETED that arrives after real activity was observed. The
        # first (empty) delta has nothing to show, so it appends nothing —
        # the synthetic task stays in_progress rather than completing early.
        assert len(appended) == 2
        common.slack_app.client.stop_stream.assert_called_once()

    def test_poller_self_close_still_flushes_a_tool_call_that_missed_the_last_poll(self):
        """A tool call can finish in the same window the poller decides the
        turn is done (a live incident: the conversation read COMPLETED before
        the turn's own final write), so when the poller closes the panel
        itself — not via the external settle handler — it must still take one
        more look before actually stopping, exactly like stop_progress_stream
        already does."""
        common = self._common()
        cache = MagicMock()
        cache.get_event_entry.side_effect = [{}, {"stream_ts": "pending-fixed"}] + [{"stream_ts": "3000.3"}] * 3
        cache.update_event_entry.return_value = True
        deltas = [
            {"conversation": {"status": "IN_PROGRESS"}, "tool_calls": [_tool_row("t1", "IN_PROGRESS")], "cursor": "c1"},
            {"conversation": {"status": "COMPLETED"}, "tool_calls": [], "cursor": "c2"},
            # Only reached by the finally-block flush, after the main loop
            # already decided to close on the (trusted, but incomplete) delta
            # above.
            {"tool_calls": [_tool_row("t2", "SUCCESS")]},
        ]
        self._run(common, cache, deltas=deltas)
        appended = common.slack_app.client.append_stream.call_args_list
        assert len(appended) == 2
        assert appended[-1].kwargs["chunks"][0] == {
            "type": "task_update",
            "id": "t2",
            "title": "Get pod logs",
            "status": "complete",
        }
        common.slack_app.client.stop_stream.assert_called_once()
