import json
import os
import asyncio
import time
from pathlib import Path
from unittest.mock import patch

import httpx
import pytest

from orcabench.nubi_agent import (
    _ENVIRONMENT_DISCOVERY_COMMAND,
    _ORCA_SHELL_TOOL_SCHEMA,
    _extract_balanced_json_object,
    _extract_command,
    _final_response,
    _is_explicit_no_incident_response,
    _parse_tool_calls,
    _truncate_output,
    NuBiHarborAgent,
)


def test_orca_prompt_separates_discovery_collection_and_finalization():
    prompt = Path(__file__).with_name("orca-agent.yaml").read_text()

    assert "Phase 1 — Capability discovery" in prompt
    assert "Do not decide whether an incident occurred during this phase" in prompt
    assert "Phase 2 — Evidence collection" in prompt
    assert "Capability discovery is not a completed investigation" in prompt
    assert "immediately continue to Phase 2" in prompt
    assert "tool calls together so they can execute in parallel" in prompt
    assert "Phase 3 — Confirmation" in prompt
    assert "Use at most one additional evidence round" in prompt
    assert "Phase 4 — Finalization" in prompt
    assert "must only write `/app/report.md`" in prompt
    assert "uname -a &&" not in prompt


def test_client_tool_has_benchmark_specific_name():
    assert _ORCA_SHELL_TOOL_SCHEMA["name"] == "orca_shell_execute"


def test_extract_command_from_dict_and_json():
    assert _extract_command({"command": "pwd"}) == "pwd"
    assert _extract_command(json.dumps({"command": "ls -la"})) == "ls -la"


def test_extract_command_tolerates_trailing_planner_artifact():
    value = '{"command":"printf \\"}\\""}]]>'
    assert _extract_balanced_json_object(value) == '{"command":"printf \\"}\\""}'
    assert _extract_command(value) == 'printf "}"'


def test_extract_command_skips_non_json_braces_before_tool_input():
    value = 'I will use the {tool} to run {"command": "ls"}'

    assert _extract_balanced_json_object(value) == '{"command": "ls"}'
    assert _extract_command(value) == "ls"


def test_parse_tool_calls_accepts_list_object_and_json():
    call = {"tool_id": "1", "tool_input": {"command": "pwd"}}
    assert _parse_tool_calls(call) == [call]
    assert _parse_tool_calls([call, "bad"]) == [call]
    assert _parse_tool_calls(json.dumps([call])) == [call]


def test_truncate_output_preserves_head_tail_and_limit():
    value = "a" * 100 + "z" * 100

    truncated = _truncate_output(value, 100)

    assert len(truncated) == 100
    assert truncated.startswith("a")
    assert truncated.endswith("z")
    assert "[output truncated: 200 -> 100 characters]" in truncated


def test_final_response_uses_latest_conversation_message():
    response = {
        "data": {
            "llm_conversation_messages": [
                {"response": "old"},
                {"response": "final report"},
            ]
        }
    }
    assert _final_response(response) == "final report"


def test_explicit_no_incident_response_requires_empty_report_contract():
    assert _is_explicit_no_incident_response(
        "No incident occurred. An empty file was written to /app/report.md."
    )
    assert not _is_explicit_no_incident_response(
        "No incident occurred before the current outage. See /app/report.md."
    )


def test_agent_reads_harbor_extra_env(tmp_path: Path):
    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    with patch.dict(os.environ, {}, clear=True):
        agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    assert agent._url == "http://nubi.test"
    assert agent._agent_name == "orca"


def test_agent_accepts_missing_harbor_extra_env(tmp_path: Path):
    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    with patch.dict(os.environ, values, clear=True):
        agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=None)

    assert agent._url == "http://nubi.test"


def test_agent_writes_latest_conversation_snapshot(tmp_path: Path):
    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)

    agent._write_conversation_snapshot({"data": {"status": "COMPLETED"}})

    assert json.loads((tmp_path / "nubi_conversation.json").read_text()) == {
        "data": {"status": "COMPLETED"}
    }


def test_ensure_report_preserves_intentionally_empty_report(tmp_path: Path):
    class Result:
        return_code = 0
        stdout = "new-state\n"

    class Environment:
        def __init__(self):
            self.commands = []

        async def exec(self, command, timeout_sec):
            self.commands.append((command, timeout_sec))
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()

    asyncio.run(agent._ensure_report(environment, "final report", "old-state"))

    assert len(environment.commands) == 1
    assert "stat -c '%y:%s' /app/report.md" in environment.commands[0][0]


def test_ensure_report_rejects_final_response_when_report_missing(tmp_path: Path):
    class Result:
        def __init__(self, return_code):
            self.return_code = return_code
            self.stdout = "unchanged-state\n"

    class Environment:
        def __init__(self):
            self.commands = []

        async def exec(self, command, timeout_sec):
            self.commands.append((command, timeout_sec))
            return Result(0)

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()

    with pytest.raises(
        RuntimeError,
        match="completed without intentionally creating /app/report.md",
    ):
        asyncio.run(
            agent._ensure_report(environment, "final report", "unchanged-state")
        )

    assert "stat -c '%y:%s' /app/report.md" in environment.commands[0][0]
    assert len(environment.commands) == 1


def test_ensure_report_overrides_stale_artifact_for_explicit_no_incident(
    tmp_path: Path,
):
    class Result:
        return_code = 0
        stdout = "changed-state\n"

    class Environment:
        def __init__(self):
            self.commands = []

        async def exec(self, command, timeout_sec):
            self.commands.append((command, timeout_sec))
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    agent._report_invalidated = True
    environment = Environment()

    asyncio.run(
        agent._ensure_report(
            environment,
            "No incident detected. An empty report was written to /app/report.md.",
            "old-state",
        )
    )

    assert environment.commands == [(": > /app/report.md", 30)]


def test_report_state_returns_empty_when_placeholder_is_missing(tmp_path: Path):
    class Result:
        return_code = 1
        stdout = ""

    class Environment:
        def __init__(self):
            self.commands = []

        async def exec(self, command, timeout_sec):
            self.commands.append((command, timeout_sec))
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()

    state = asyncio.run(agent._report_state(environment))

    assert state == ""
    assert environment.commands == [("stat -c '%y:%s' /app/report.md", 10)]


def test_start_conversation_adds_model_config_only_when_complete(tmp_path: Path):
    class Response:
        def raise_for_status(self):
            pass

        def json(self):
            return {"data": {"conversation_id": "conversation-id"}}

    class Client:
        def __init__(self):
            self.payload = None

        async def post(self, url, headers, json):
            self.payload = json
            return Response()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
        "NUBI_LLM_PROVIDER": "googleai",
        "NUBI_LLM_MODEL": "fast-model",
    }
    client = Client()
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    asyncio.run(agent._start_conversation(client, "task"))
    assert client.payload["config"] == {
        "llm_provider": "googleai",
        "llm_model_name": "fast-model",
    }

    client = Client()
    values.pop("NUBI_LLM_MODEL")
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    asyncio.run(agent._start_conversation(client, "task"))
    assert "config" not in client.payload


def test_environment_hint_is_allowlisted_and_reused(tmp_path: Path):
    class Result:
        return_code = 0
        stdout = """os=Linux 6.0 x86_64
working_directory=/app
executable.python3=available
executable.jq=missing
environment.GRAFANA_URL=available
datasource.webstore-metrics=prometheus:Prometheus
datasource.webstore-logs=grafana-opensearch-datasource:OpenSearch
"""

    class Environment:
        def __init__(self):
            self.commands = []

        async def exec(self, command, timeout_sec):
            self.commands.append((command, timeout_sec))
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()

    instruction = asyncio.run(
        agent._instruction_with_environment_hint("investigate", environment)
    )

    assert environment.commands == [(_ENVIRONMENT_DISCOVERY_COMMAND, 10)]
    assert "printf '\\n'\n    python3 - <<'PY'" in _ENVIRONMENT_DISCOVERY_COMMAND
    assert instruction.startswith("investigate\n\n<verified_environment_capabilities>")
    assert "executable.jq=missing" in instruction
    assert "datasource.webstore-metrics=prometheus:Prometheus" in instruction
    assert "do not spend a tool call rediscovering" in instruction
    assert "authoritative for this run" in instruction
    assert "do not call /api/datasources" in instruction
    assert "Only rediscover a datasource if a provided UID fails" in instruction
    assert "Refer to GRAFANA_URL by variable name" in instruction


def test_environment_hint_does_not_block_run_when_discovery_fails(tmp_path: Path):
    class Result:
        return_code = 127
        stdout = ""

    class Environment:
        async def exec(self, command, timeout_sec):
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)

    instruction = asyncio.run(
        agent._instruction_with_environment_hint("investigate", Environment())
    )

    assert instruction == "investigate"


def test_initial_poll_retries_transient_not_found(tmp_path: Path):
    class Client:
        def __init__(self):
            self.calls = 0

        async def post(self, url, headers, json):
            self.calls += 1
            request = httpx.Request("POST", url)
            if self.calls == 1:
                return httpx.Response(404, request=request, json={"error": "not found"})
            return httpx.Response(
                200,
                request=request,
                json={"data": {"status": "RUNNING"}},
            )

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
        "NUBI_POLL_INTERVAL": "0",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    client = Client()

    response = asyncio.run(
        agent._poll_until_visible(
            client,
            "conversation-id",
            time.monotonic() + 1,
            tmp_path / "agent.log",
        )
    )

    assert client.calls == 2
    assert response == {"data": {"status": "RUNNING"}}
    assert "retrying transient 404" in (tmp_path / "agent.log").read_text()


def test_initial_poll_does_not_retry_other_http_errors(tmp_path: Path):
    class Client:
        async def post(self, url, headers, json):
            request = httpx.Request("POST", url)
            return httpx.Response(401, request=request, json={"error": "unauthorized"})

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
        "NUBI_POLL_INTERVAL": "0",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)

    with pytest.raises(httpx.HTTPStatusError):
        asyncio.run(
            agent._poll_until_visible(
                Client(),
                "conversation-id",
                time.monotonic() + 1,
                tmp_path / "agent.log",
            )
        )


def test_execute_client_tools_runs_parallel_calls_concurrently(tmp_path: Path):
    class Result:
        return_code = 0
        stderr = ""

        def __init__(self, stdout):
            self.stdout = stdout

    class Environment:
        def __init__(self):
            self.running = 0
            self.max_running = 0
            self.commands = []

        async def exec(self, command, timeout_sec):
            self.commands.append(command)
            self.running += 1
            self.max_running = max(self.max_running, self.running)
            await asyncio.sleep(0.01)
            self.running -= 1
            return Result(command)

    class Client:
        def __init__(self):
            self.payload = None

        async def post(self, url, headers, json):
            self.payload = json

            class Response:
                def raise_for_status(self):
                    pass

            return Response()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()
    client = Client()
    response = {
        "data": {
            "llm_conversation_messages": [
                {
                    "id": "message-id",
                    "llm_conversation_agents": [
                        {
                            "id": "agent-id",
                            "status": "waiting_for_client_tool",
                            "agent_step_response": [
                                {
                                    "tool_id": "first",
                                    "tool_input": {"command": "first-command"},
                                },
                                {
                                    "tool_id": "second",
                                    "tool_input": {"command": "second-command"},
                                },
                            ],
                        }
                    ],
                }
            ]
        }
    }

    asyncio.run(
        agent._execute_client_tools(
            client, response, "conversation-id", environment, tmp_path / "agent.log"
        )
    )

    assert environment.max_running == 2
    assert client.payload["results"] == [
        {"tool_id": "first", "result": "first-command", "status": "SUCCESS"},
        {"tool_id": "second", "result": "second-command", "status": "SUCCESS"},
    ]

    asyncio.run(
        agent._execute_client_tools(
            client, response, "conversation-id", environment, tmp_path / "agent.log"
        )
    )
    assert environment.max_running == 2
    assert environment.commands == ["first-command", "second-command"]
    assert agent._submitted_tool_ids == {"first", "second"}


def test_submit_tool_results_accepts_only_idempotent_or_transient_conflicts(
    tmp_path: Path,
):
    class Client:
        def __init__(self, detail):
            self.detail = detail

        async def post(self, url, headers, json):
            return httpx.Response(
                409,
                request=httpx.Request("POST", url),
                json={"errors": [{"message": self.detail}]},
            )

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    arguments = ("conversation-id", "message-id", "agent-id", [])

    assert asyncio.run(
        agent._submit_tool_results(
            Client("api: tool response for tool-1 already submitted"), *arguments
        )
    )
    assert not asyncio.run(
        agent._submit_tool_results(
            Client("api: conversation is currently in progress, please wait"),
            *arguments,
        )
    )
    with pytest.raises(httpx.HTTPStatusError):
        asyncio.run(
            agent._submit_tool_results(Client("some other conflict"), *arguments)
        )


def test_execute_client_tools_does_not_rerun_command_after_transient_conflict(
    tmp_path: Path,
):
    class Result:
        return_code = 0
        stdout = "evidence"
        stderr = ""

    class Environment:
        def __init__(self):
            self.calls = 0

        async def exec(self, command, timeout_sec):
            self.calls += 1
            return Result()

    class Client:
        def __init__(self):
            self.calls = 0

        async def post(self, url, headers, json):
            self.calls += 1
            if self.calls == 1:
                return httpx.Response(
                    409,
                    request=httpx.Request("POST", url),
                    json={
                        "errors": [
                            {
                                "message": "api: conversation is currently in progress, please wait"
                            }
                        ]
                    },
                )
            return httpx.Response(200, request=httpx.Request("POST", url), json={})

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()
    client = Client()
    response = {
        "data": {
            "llm_conversation_messages": [
                {
                    "id": "message-id",
                    "llm_conversation_agents": [
                        {
                            "id": "agent-id",
                            "status": "waiting_for_client_tool",
                            "agent_step_response": [
                                {
                                    "tool_id": "tool-1",
                                    "tool_input": {"command": "query"},
                                }
                            ],
                        }
                    ],
                }
            ]
        }
    }

    for _ in range(2):
        asyncio.run(
            agent._execute_client_tools(
                client,
                response,
                "conversation-id",
                environment,
                tmp_path / "agent.log",
            )
        )

    assert environment.calls == 1
    assert client.calls == 2
    assert agent._submitted_tool_ids == {"tool-1"}
    assert agent._pending_tool_results == {}


def test_execute_tool_call_returns_nonempty_result_for_silent_command(tmp_path: Path):
    class Result:
        return_code = 0
        stdout = ""
        stderr = ""

    class Environment:
        async def exec(self, command, timeout_sec):
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)

    result = asyncio.run(
        agent._execute_tool_call(
            {"tool_id": "silent", "tool_input": {"command": "true"}},
            Environment(),
            tmp_path / "agent.log",
        )
    )

    assert result == {
        "tool_id": "silent",
        "result": "[command completed successfully with no output]",
        "status": "SUCCESS",
    }


def test_execute_tool_call_normalizes_null_tool_id(tmp_path: Path):
    class Result:
        return_code = 0
        stdout = "ok"
        stderr = ""

    class Environment:
        async def exec(self, command, timeout_sec):
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)

    result = asyncio.run(
        agent._execute_tool_call(
            {"tool_id": None, "tool_input": {"command": "true"}},
            Environment(),
            tmp_path / "agent.log",
        )
    )

    assert result["tool_id"] == ""


def test_execute_tool_call_reports_nonzero_exit_as_error(tmp_path: Path):
    class Result:
        return_code = 2
        stdout = "partial output"
        stderr = "bad command"

    class Environment:
        async def exec(self, command, timeout_sec):
            return Result()

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)

    result = asyncio.run(
        agent._execute_tool_call(
            {
                "tool_id": "failed",
                "tool_input": {"command": "false > /app/report.md"},
            },
            Environment(),
            tmp_path / "agent.log",
        )
    )

    assert result == {
        "tool_id": "failed",
        "result": "[exit code 2]\npartial output\nbad command",
        "status": "ERROR",
    }
    assert agent._report_invalidated is True


def test_execute_tool_call_persists_full_oversized_output(tmp_path: Path):
    full_output = "a" * 1000 + "z" * 1000

    class Result:
        return_code = 0
        stdout = full_output
        stderr = ""

    class Environment:
        def __init__(self):
            self.uploaded_content = ""
            self.uploaded_target = ""

        async def exec(self, command, timeout_sec):
            return Result()

        async def upload_file(self, source_path, target_path):
            self.uploaded_content = Path(source_path).read_text()
            self.uploaded_target = target_path

    values = {
        "NUBI_URL": "http://nubi.test",
        "NUBI_TOKEN": "token",
        "NUBI_ACCOUNT_ID": "account",
        "NUBI_TENANT_ID": "tenant",
        "NUBI_MAX_TOOL_OUTPUT_CHARS": "1000",
    }
    agent = NuBiHarborAgent(logs_dir=tmp_path, extra_env=values)
    environment = Environment()

    result = asyncio.run(
        agent._execute_tool_call(
            {"tool_id": "tool/id", "tool_input": {"command": "large-output"}},
            environment,
            tmp_path / "agent.log",
        )
    )

    assert environment.uploaded_content == full_output
    assert environment.uploaded_target == "/tmp/nubi-orca/tool-output-tool_id.txt"
    assert len(result["result"]) == 1000
    assert result["result"].startswith("a")
    assert "[output truncated: 2000 ->" in result["result"]
    assert result["result"].endswith(
        "[full output saved to /tmp/nubi-orca/tool-output-tool_id.txt]"
    )
