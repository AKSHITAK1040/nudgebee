#!/usr/bin/env python3
"""Extract comparable planner metrics from an ORCA trial and NuBi export."""

import argparse
import json
from collections import Counter
from datetime import datetime
from pathlib import Path
from typing import Any


def _mapping(value: Any) -> dict:
    if isinstance(value, dict):
        return value
    if isinstance(value, str):
        try:
            parsed = json.loads(value)
        except json.JSONDecodeError:
            return {}
        return parsed if isinstance(parsed, dict) else {}
    return {}


def _payload(document: dict) -> dict:
    data = document.get("data", document)
    if not isinstance(data, dict):
        return {}
    conversations = data.get("llm_conversations")
    if isinstance(conversations, list) and conversations:
        return conversations[0] if isinstance(conversations[0], dict) else {}
    graphql = data.get("ai_get_conversation_v3")
    if isinstance(graphql, dict):
        return graphql
    return data


def _parse_calls(value: Any) -> list[dict]:
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError:
            return []
    if isinstance(value, dict):
        return [value]
    if isinstance(value, list):
        return [item for item in value if isinstance(item, dict)]
    return []


def _tool_calls(payload: dict) -> list[dict]:
    direct = payload.get("tool_calls")
    if isinstance(direct, list):
        return [call for call in direct if isinstance(call, dict)]

    calls = []
    messages = payload.get("llm_conversation_messages") or payload.get("messages") or []
    for message in messages:
        agents = message.get("llm_conversation_agents") or []
        for agent in agents:
            persisted = agent.get("llm_conversation_tool_calls") or []
            calls.extend(
                call
                for call in persisted
                if isinstance(call, dict)
                and (call.get("tool_name") or call.get("tool_id"))
            )
            for call in _parse_calls(agent.get("agent_step_response")):
                calls.append(
                    {
                        **call,
                        "agent_id": agent.get("id"),
                        "created_at": agent.get("created_at"),
                        "updated_at": agent.get("updated_at"),
                    }
                )
    return calls


def _timestamp(value: Any) -> datetime | None:
    if not isinstance(value, str) or not value:
        return None
    try:
        return datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        return None


def _duration_seconds(start: Any, finish: Any) -> float | None:
    started = _timestamp(start)
    finished = _timestamp(finish)
    if not started or not finished:
        return None
    return round((finished - started).total_seconds(), 3)


def _command(call: dict) -> str:
    parameters = _mapping(call.get("parameters") or call.get("tool_input"))
    command = parameters.get("command", "")
    return " ".join(str(command).split())


def _is_datasource_discovery(command: str) -> bool:
    lowered = command.lower()
    return "/api/datasources" in lowered and "/api/datasources/proxy/" not in lowered


def _message_config(payload: dict) -> dict:
    messages = payload.get("messages") or payload.get("llm_conversation_messages") or []
    for message in messages:
        config = _mapping(message.get("message_config"))
        if config:
            return config
    return {}


def analyze(conversation: dict, trial: dict | None = None) -> dict:
    payload = _payload(conversation)
    calls = _tool_calls(payload)
    metadata = [_mapping(call.get("metadata")) for call in calls]
    batch_counts = Counter(
        str(item["execution_batch_id"])
        for item in metadata
        if item.get("execution_batch_id")
    )
    parallel_batches = [size for size in batch_counts.values() if size > 1]
    iterations = sorted(
        {
            int(item["planner_iteration"])
            for item in metadata
            if str(item.get("planner_iteration", "")).isdigit()
        }
    )
    commands = [command for call in calls if (command := _command(call))]
    # Preserve call positions so the baseline can distinguish useful early
    # fan-out from parallelism that appears only after a long sequential search.
    parallel_batch_ids = {
        batch_id for batch_id, size in batch_counts.items() if size > 1
    }
    first_parallel_call = next(
        (
            index
            for index, item in enumerate(metadata, start=1)
            if str(item.get("execution_batch_id")) in parallel_batch_ids
        ),
        None,
    )
    repeated_commands = sum(
        count - 1 for count in Counter(commands).values() if count > 1
    )
    tool_durations = [
        duration
        for call in calls
        if (
            duration := _duration_seconds(
                call.get("created_at"), call.get("updated_at")
            )
        )
        is not None
    ]
    config = _message_config(payload)
    conversation_data = payload.get("conversation") or payload
    conversation_seconds = _duration_seconds(
        conversation_data.get("created_at"), conversation_data.get("updated_at")
    )
    call_timestamps = [
        timestamp
        for call in calls
        if (timestamp := _timestamp(call.get("created_at"))) is not None
    ]
    first_call_at = min(call_timestamps, default=None)
    conversation_started_at = _timestamp(conversation_data.get("created_at"))
    first_tool_delay = None
    if first_call_at and conversation_started_at:
        first_tool_delay = round(
            (first_call_at - conversation_started_at).total_seconds(), 3
        )

    result = {
        "conversation_id": conversation_data.get("id"),
        "conversation_status": conversation_data.get("status") or payload.get("status"),
        "model": {
            "provider": config.get("llm_provider"),
            "name": config.get("llm_model_name"),
        },
        "planner": {
            "iterations_seen": iterations,
            "iteration_count": len(iterations),
            "tool_calls": len(calls),
            "tool_errors": sum(
                str(call.get("status", "")).lower() in {"error", "fail", "failed"}
                for call in calls
            ),
            "tool_names": dict(
                sorted(
                    Counter(
                        call.get("tool_name") or "unknown" for call in calls
                    ).items()
                )
            ),
            "explicit_execution_batches": len(batch_counts),
            "parallel_batches": len(parallel_batches),
            "parallel_tool_calls": sum(parallel_batches),
            "max_parallel_width": max(parallel_batches, default=1 if calls else 0),
            "unbatched_tool_calls": sum(
                not item.get("execution_batch_id") for item in metadata
            ),
            "repeated_exact_commands": repeated_commands,
            "empty_shell_calls": sum(
                (call.get("tool_name") or "").lower().endswith("shell_execute")
                and not _command(call)
                for call in calls
            ),
            "datasource_discovery_calls": sum(
                _is_datasource_discovery(command) for command in commands
            ),
            "first_parallel_call": first_parallel_call,
            "tool_execution_seconds": round(sum(tool_durations), 3),
            "first_tool_delay_seconds": first_tool_delay,
        },
        "conversation_seconds": conversation_seconds,
    }

    if trial:
        execution = trial.get("agent_execution") or {}
        result["trial"] = {
            "task_name": trial.get("task_name"),
            "trial_name": trial.get("trial_name"),
            "started_at": trial.get("started_at"),
            "finished_at": trial.get("finished_at"),
            "wall_seconds": _duration_seconds(
                trial.get("started_at"), trial.get("finished_at")
            ),
            "agent_seconds": _duration_seconds(
                execution.get("started_at"), execution.get("finished_at")
            ),
            "rewards": (trial.get("verifier_result") or {}).get("rewards") or {},
            "exception": trial.get("exception_info"),
        }
    return result


def _load(path: Path) -> dict:
    document = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict):
        raise ValueError(f"Expected a JSON object in {path}")
    return document


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("conversation", type=Path, help="NuBi conversation JSON")
    parser.add_argument("--trial-result", type=Path, help="Harbor trial result.json")
    parser.add_argument("--output", type=Path, help="Write metrics JSON to this path")
    parser.add_argument("--max-tool-calls", type=int, help="Fail if calls exceed this value")
    parser.add_argument("--max-wall-seconds", type=float, help="Fail if runtime exceeds this value")
    parser.add_argument(
        "--require-parallel",
        action="store_true",
        help="Fail if no parallel batch was recorded",
    )
    parser.add_argument(
        "--max-first-parallel-call",
        type=int,
        help="Fail if fan-out starts after this tool-call position",
    )
    parser.add_argument(
        "--max-datasource-discovery-calls",
        type=int,
        help="Fail if datasource discovery exceeds this value",
    )
    parser.add_argument(
        "--forbid-empty-shell-calls",
        action="store_true",
        help="Fail if an empty shell action was recorded",
    )
    args = parser.parse_args()

    trial = _load(args.trial_result) if args.trial_result else None
    metrics = analyze(_load(args.conversation), trial)
    rendered = json.dumps(metrics, indent=2, sort_keys=True) + "\n"
    if args.output:
        args.output.write_text(rendered, encoding="utf-8")
    else:
        print(rendered, end="")

    failures = []
    planner = metrics["planner"]
    if args.max_tool_calls is not None and planner["tool_calls"] > args.max_tool_calls:
        failures.append(f"tool_calls={planner['tool_calls']} exceeds {args.max_tool_calls}")
    wall_seconds = (metrics.get("trial") or {}).get("wall_seconds") or metrics.get(
        "conversation_seconds"
    )
    if (
        args.max_wall_seconds is not None
        and wall_seconds is not None
        and wall_seconds > args.max_wall_seconds
    ):
        failures.append(f"wall_seconds={wall_seconds} exceeds {args.max_wall_seconds}")
    if args.require_parallel and planner["parallel_batches"] == 0:
        failures.append("no parallel execution batch recorded")
    if args.max_first_parallel_call is not None and (
        planner["first_parallel_call"] is None
        or planner["first_parallel_call"] > args.max_first_parallel_call
    ):
        failures.append(
            f"first_parallel_call={planner['first_parallel_call']} "
            f"exceeds {args.max_first_parallel_call}"
        )
    if (
        args.max_datasource_discovery_calls is not None
        and planner["datasource_discovery_calls"]
        > args.max_datasource_discovery_calls
    ):
        failures.append(
            "datasource_discovery_calls="
            f"{planner['datasource_discovery_calls']} "
            f"exceeds {args.max_datasource_discovery_calls}"
        )
    if args.forbid_empty_shell_calls and planner["empty_shell_calls"]:
        failures.append(f"empty_shell_calls={planner['empty_shell_calls']}")
    if failures:
        parser.error("baseline gate failed: " + "; ".join(failures))


if __name__ == "__main__":
    main()
