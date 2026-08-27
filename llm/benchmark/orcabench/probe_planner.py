#!/usr/bin/env python3
"""Run low-cost, canned probes of custom-agent planner fan-out behavior."""

import argparse
import json
import os
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from pathlib import Path
from typing import Any


TOOL_SCHEMA = {
    "name": "orca_shell_execute",
    "description": (
        "Synthetic read-only telemetry tool. It supports datasource discovery "
        "and independent bounded metrics, logs, and traces queries."
    ),
    "input": {
        "type": "object",
        "properties": {"command": {"type": "string"}},
        "required": ["command"],
    },
}


@dataclass(frozen=True)
class ProbeCase:
    prompt: str
    expect_discovery: bool


def _prompt_fixture(name: str) -> str:
    return (Path(__file__).parent / "probe_prompts" / name).read_text()


CASES = {
    "known-target": ProbeCase(
        prompt=(
            "@orca Synthetic read-only investigation: checkout is slow. The target "
            "is checkout. Collect the initial metrics, logs, and traces evidence. "
            "This is not a real incident and requires no report file.\n\n"
            "<verified_environment_capabilities>\n"
            "working_directory=/app\n"
            "executable.synthetic-metrics=available\n"
            "executable.synthetic-logs=available\n"
            "executable.synthetic-traces=available\n"
            "datasource.metrics=synthetic-metrics\n"
            "datasource.logs=synthetic-logs\n"
            "datasource.traces=synthetic-traces\n"
            "</verified_environment_capabilities>\n"
            "The adapter verified this capability map. Reuse it and do not perform "
            "datasource, executable, environment, or schema discovery."
        ),
        expect_discovery=False,
    ),
    "natural-discovery": ProbeCase(
        prompt=(
            "@orca Synthetic read-only investigation: checkout is slow. Determine "
            "the available telemetry datasources, then collect independent initial "
            "evidence from metrics, logs, and traces. Keep discovery bounded. This "
            "is not a real incident and requires no report file."
        ),
        expect_discovery=True,
    ),
    "orca-first-batch": ProbeCase(
        prompt=_prompt_fixture("orca_first_batch.md"),
        expect_discovery=False,
    ),
}


def _data(response: dict[str, Any]) -> dict[str, Any]:
    value = response.get("data", response)
    return value if isinstance(value, dict) else {}


def extract_waiting_calls(response: dict[str, Any]) -> tuple[str, str, list[dict]]:
    for message in _data(response).get("llm_conversation_messages") or []:
        for agent in message.get("llm_conversation_agents") or []:
            if agent.get("status") != "waiting_for_client_tool":
                continue
            raw = agent.get("agent_step_response")
            try:
                calls = json.loads(raw) if isinstance(raw, str) else raw
            except json.JSONDecodeError:
                calls = []
            if isinstance(calls, dict):
                calls = [calls]
            if not isinstance(calls, list):
                calls = []
            return (
                str(agent.get("message_id") or message.get("id") or ""),
                str(agent.get("id") or ""),
                [call for call in calls if isinstance(call, dict)],
            )
    return "", "", []


def command_for(call: dict) -> str:
    value = call.get("tool_input")
    try:
        parsed = json.loads(value) if isinstance(value, str) else value
    except json.JSONDecodeError:
        parsed = None
    if isinstance(parsed, dict):
        return str(parsed.get("command", ""))
    return str(value or "")


class Client:
    def __init__(self) -> None:
        self.base = os.environ["NUBI_URL"].rstrip("/")
        self.account_id = os.environ["NUBI_ACCOUNT_ID"]
        self.tenant_id = os.environ["NUBI_TENANT_ID"]
        self.headers = {
            os.getenv("NUBI_TOKEN_HEADER", "X-ACTION-TOKEN"): os.environ[
                "NUBI_TOKEN"
            ],
            "x-tenant-id": self.tenant_id,
            "Content-Type": "application/json",
        }

    def post(self, path: str, payload: dict) -> dict:
        request = urllib.request.Request(
            self.base + path,
            data=json.dumps(payload).encode(),
            headers=self.headers,
        )
        with urllib.request.urlopen(request, timeout=30) as response:
            return json.load(response)

    def start(self, prompt: str) -> str:
        payload = {
            "query": prompt,
            "account_id": self.account_id,
            "user_id": "",
            "tenant_id": self.tenant_id,
            "async": True,
            "client_tools": [TOOL_SCHEMA],
        }
        provider = os.getenv("NUBI_LLM_PROVIDER", "").strip()
        model = os.getenv("NUBI_LLM_MODEL", "").strip()
        if provider and model:
            payload["config"] = {
                "llm_provider": provider,
                "llm_model_name": model,
            }
        return str(_data(self.post("/v1/completions/chat", payload))["conversation_id"])

    def poll_for_calls(
        self,
        conversation_id: str,
        timeout: float = 180,
        exclude_tool_ids: set[str] | None = None,
    ) -> list[dict]:
        excluded = exclude_tool_ids or set()
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            time.sleep(2)
            try:
                response = self.post(
                    "/v1/completions/chat_get",
                    {"conversation_id": conversation_id, "account_id": self.account_id},
                )
            except urllib.error.HTTPError as error:
                if error.code == 404:
                    continue
                raise
            _, _, calls = extract_waiting_calls(response)
            calls = [
                call
                for call in calls
                if str(call.get("tool_id") or "") not in excluded
            ]
            if calls:
                return calls
            status = str(_data(response).get("status", "")).upper()
            if status in {"COMPLETED", "FAILED", "TERMINATED", "KILLED"}:
                raise RuntimeError(f"conversation ended before tool calls: {status}")
        raise TimeoutError("planner probe timed out waiting for tool calls")

    def submit_discovery(self, conversation_id: str) -> None:
        response = self.post(
            "/v1/completions/chat_get",
            {"conversation_id": conversation_id, "account_id": self.account_id},
        )
        message_id, agent_id, calls = extract_waiting_calls(response)
        self.post(
            "/v1/completions/client-tool-result",
            {
                "conversation_id": conversation_id,
                "message_id": message_id,
                "agent_id": agent_id,
                "account_id": self.account_id,
                "async": True,
                "results": [
                    {
                        "tool_id": str(call.get("tool_id") or ""),
                        "status": "SUCCESS",
                        "result": (
                            "datasource.metrics=synthetic-metrics\n"
                            "datasource.logs=synthetic-logs\n"
                            "datasource.traces=synthetic-traces"
                        ),
                    }
                    for call in calls
                ],
            },
        )

    def stop(self, conversation_id: str) -> None:
        self.post(
            "/v1/completions/chat_stop",
            {
                "conversation_id": conversation_id,
                "account_id": self.account_id,
                "user_id": "",
            },
        )


def run(case_name: str) -> dict[str, Any]:
    case = CASES[case_name]
    client = Client()
    conversation_id = client.start(case.prompt)
    try:
        first = client.poll_for_calls(conversation_id)
        first_commands = [command_for(call) for call in first]
        if case.expect_discovery:
            if len(first) != 1:
                raise AssertionError(f"expected one discovery call, got {first_commands}")
            client.submit_discovery(conversation_id)
            fanout = client.poll_for_calls(
                conversation_id,
                exclude_tool_ids={str(call.get("tool_id") or "") for call in first},
            )
        else:
            fanout = first
        fanout_commands = [command_for(call) for call in fanout]
        if len(fanout) < 3:
            raise AssertionError(f"expected parallel fan-out, got {fanout_commands}")
        return {
            "case": case_name,
            "conversation_id": conversation_id,
            "first_commands": first_commands,
            "fanout_width": len(fanout),
            "fanout_commands": fanout_commands,
            "passed": True,
        }
    except Exception as error:
        raise RuntimeError(
            f"case={case_name} conversation_id={conversation_id}: {error}"
        ) from error
    finally:
        client.stop(conversation_id)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("case", choices=sorted(CASES))
    args = parser.parse_args()
    print(json.dumps(run(args.case), indent=2))


if __name__ == "__main__":
    main()
