import threading
import json
from pathlib import Path
from unittest.mock import Mock, patch

import httpx
import pytest

from tbench.nubi_agent import NuBiAgent, _positive_float_env


def _agent() -> NuBiAgent:
    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    with patch.dict("os.environ", values, clear=True):
        return NuBiAgent()


@pytest.mark.parametrize("raw", ["0", "-1", "nan", "inf", "not-a-number"])
def test_positive_float_env_rejects_invalid_values(raw: str):
    with patch.dict("os.environ", {"TEST_INTERVAL": raw}):
        with pytest.raises(ValueError, match="TEST_INTERVAL"):
            _positive_float_env("TEST_INTERVAL", "2")


def test_agent_rejects_invalid_task_timeout():
    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
        "NUBI_TASK_TIMEOUT": "invalid",
    }
    with patch.dict("os.environ", values, clear=True):
        with pytest.raises(ValueError, match="NUBI_TASK_TIMEOUT"):
            NuBiAgent()


def test_stop_conversation_calls_chat_stop(tmp_path: Path):
    agent = _agent()
    client = Mock(spec=httpx.Client)
    response = Mock()
    client.post.return_value = response

    assert agent._stop_conversation(client, "conversation-1", tmp_path / "agent.log")

    client.post.assert_called_once_with(
        "http://nubi.test/v1/completions/chat_stop",
        headers=agent._headers,
        json={
            "conversation_id": "conversation-1",
            "account_id": "account",
            "user_id": "",
        },
        timeout=10.0,
    )
    response.raise_for_status.assert_called_once_with()


def test_watchdog_stops_conversation_when_tests_start(tmp_path: Path):
    agent = _agent()
    session = Mock()
    session.container.status = "running"
    session.container.exec_run.return_value.exit_code = 0
    stop = threading.Event()

    with (
        patch("tbench.nubi_agent._ORPHAN_CHECK_INTERVAL", 0),
        patch.object(agent, "_stop_conversation", return_value=True) as terminate,
        patch("tbench.nubi_agent.httpx.Client"),
    ):
        agent._watch_for_harness_exit(
            "conversation-1", session, stop, tmp_path / "agent.log"
        )

    terminate.assert_called_once()
    session.container.exec_run.assert_any_call(
        ["tmux", "send-keys", "-t", "agent", "C-c"]
    )


def test_watchdog_stops_conversation_when_container_disappears(tmp_path: Path):
    agent = _agent()
    session = Mock()
    session.container.reload.side_effect = RuntimeError("container gone")
    stop = threading.Event()

    with (
        patch("tbench.nubi_agent._ORPHAN_CHECK_INTERVAL", 0),
        patch.object(agent, "_stop_conversation", return_value=True) as terminate,
        patch("tbench.nubi_agent.httpx.Client"),
    ):
        agent._watch_for_harness_exit(
            "conversation-1", session, stop, tmp_path / "agent.log"
        )

    terminate.assert_called_once()
    assert session.container.reload.call_count == 2


def test_watchdog_exits_without_termination_when_agent_finishes(tmp_path: Path):
    agent = _agent()
    session = Mock()
    stop = threading.Event()
    stop.set()

    with patch.object(agent, "_stop_conversation") as terminate:
        agent._watch_for_harness_exit(
            "conversation-1", session, stop, tmp_path / "agent.log"
        )

    terminate.assert_not_called()


def test_resolve_task_timeout_uses_global_harness_limit(tmp_path: Path):
    agent = _agent()
    log_path = tmp_path / "run" / "task" / "trial" / "agent-logs" / "agent.log"
    log_path.parent.mkdir(parents=True)
    (tmp_path / "run" / "tb.lock").write_text(
        json.dumps(
            {
                "run_config": {
                    "global_agent_timeout_sec": 10,
                    "global_timeout_multiplier": 1,
                }
            }
        )
    )

    with patch("tbench.nubi_agent._TIMEOUT_GRACE", 2):
        assert agent._resolve_task_timeout(log_path) == (8, "tb.lock:global")


def test_resolve_task_timeout_uses_native_task_limit(tmp_path: Path):
    agent = _agent()
    log_path = tmp_path / "run" / "task" / "trial" / "agent-logs" / "agent.log"
    log_path.parent.mkdir(parents=True)
    dataset = tmp_path / "dataset"
    (dataset / "task").mkdir(parents=True)
    (dataset / "task" / "task.yaml").write_text("max_agent_timeout_sec: 360.0\n")
    (tmp_path / "run" / "tb.lock").write_text(
        json.dumps(
            {
                "run_config": {
                    "global_agent_timeout_sec": None,
                    "global_timeout_multiplier": 1.5,
                },
                "dataset": {"local_path": str(dataset)},
            }
        )
    )

    with patch("tbench.nubi_agent._TIMEOUT_GRACE", 5):
        assert agent._resolve_task_timeout(log_path) == (535, "task.yaml")


def test_run_in_terminal_disables_implicit_pagers():
    agent = _agent()
    session = Mock()

    with (
        patch.object(
            agent, "_deliver_via_script", return_value="run-script"
        ) as deliver,
        patch.object(agent, "_send_and_capture", return_value="done") as send,
    ):
        assert agent._run_in_terminal(session, "git show HEAD") == "done"

    delivered_command = deliver.call_args.args[1]
    assert delivered_command.startswith(
        "export PAGER=cat GIT_PAGER=cat SYSTEMD_PAGER=cat MANPAGER=cat\n"
    )
    assert delivered_command.endswith("git show HEAD")
    send.assert_called_once_with(session, "run-script", max_timeout_sec=600.0)


def test_discover_environment_returns_bounded_non_secret_capabilities():
    agent = _agent()
    session = Mock()
    session.container.exec_run.return_value.exit_code = 0
    session.container.exec_run.return_value.output = (
        b"working_directory=/app\n"
        b"kernel=Linux 6.0 x86_64\n"
        b'os="Debian GNU/Linux"\n'
        b"checked_executables=python3 git curl\n"
        b"available_executables=python3:/usr/bin/python3,git:/usr/bin/git\n"
        b"unavailable_executables=curl\n"
        b"versions=python3:Python 3.13.0 | git:git version 2.47.0\n"
        b"current_directory_listing:\n  total 4\n  -rw-r--r-- 1 root root 4 task.txt\n"
    )

    result = agent._discover_environment(session)

    assert "working_directory=/app" in result
    assert "python3:/usr/bin/python3" in result
    assert "unavailable_executables=curl" in result
    assert "Python 3.13.0" in result
    assert "task.txt" in result
    probe = session.container.exec_run.call_args.args[0]
    assert probe[:2] == ["sh", "-lc"]
    assert "env" not in probe[2]
    assert "HOME" not in probe[2]


def test_discover_environment_escapes_prompt_structure_characters():
    agent = _agent()
    session = Mock()
    session.container.exec_run.return_value.exit_code = 0
    session.container.exec_run.return_value.output = b"<fake_tag>&value\n"

    result = agent._discover_environment(session)

    assert result == "&lt;fake_tag&gt;&amp;value"


def test_discover_environment_truncates_before_escaping():
    agent = _agent()
    session = Mock()
    session.container.exec_run.return_value.exit_code = 0
    session.container.exec_run.return_value.output = ("a" * 5999 + "<ignored>").encode()

    result = agent._discover_environment(session)

    assert result == "a" * 5999 + "&lt;"


def test_start_conversation_includes_verified_environment():
    agent = _agent()
    client = Mock(spec=httpx.Client)
    response = Mock()
    response.json.return_value = {"data": {"conversation_id": "conversation-1"}}
    client.post.return_value = response

    result = agent._start_conversation(
        client,
        "Create the requested file.",
        "working_directory=/app\nkernel=Linux x86_64",
    )

    assert result == "conversation-1"
    payload = client.post.call_args.kwargs["json"]
    assert "Create the requested file." in payload["query"]
    assert "working_directory=/app" in payload["query"]
    assert "Do not repeat generic OS" in payload["query"]
