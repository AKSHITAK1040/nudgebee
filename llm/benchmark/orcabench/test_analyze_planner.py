import json

from orcabench.analyze_planner import analyze


def test_analyze_ui_export_with_explicit_parallel_batch():
    conversation = {
        "data": {
            "ai_get_conversation_v3": {
                "conversation": {"id": "conversation-1", "status": "COMPLETED"},
                "messages": [
                    {
                        "message_config": json.dumps(
                            {
                                "llm_provider": "googleai",
                                "llm_model_name": "flash",
                            }
                        )
                    }
                ],
                "tool_calls": [
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":"query metrics"}',
                        "status": "success",
                        "metadata": '{"planner_iteration":2,"execution_batch_id":"batch-1"}',
                        "created_at": "2026-08-26T10:00:00Z",
                        "updated_at": "2026-08-26T10:00:03Z",
                    },
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":"query logs"}',
                        "status": "error",
                        "metadata": '{"planner_iteration":2,"execution_batch_id":"batch-1"}',
                        "created_at": "2026-08-26T10:00:00Z",
                        "updated_at": "2026-08-26T10:00:05Z",
                    },
                ],
            }
        }
    }
    trial = {
        "task_name": "orca-bench/task-1",
        "trial_name": "task-1__abc",
        "started_at": "2026-08-26T09:59:00Z",
        "finished_at": "2026-08-26T10:01:00Z",
        "agent_execution": {
            "started_at": "2026-08-26T09:59:10Z",
            "finished_at": "2026-08-26T10:00:50Z",
        },
        "verifier_result": {"rewards": {"reward": 1.0}},
        "exception_info": None,
    }

    result = analyze(conversation, trial)

    assert result["model"] == {"provider": "googleai", "name": "flash"}
    assert result["planner"] == {
        "iterations_seen": [2],
        "iteration_count": 1,
        "tool_calls": 2,
        "tool_errors": 1,
        "tool_names": {"orca_shell_execute": 2},
        "explicit_execution_batches": 1,
        "parallel_batches": 1,
        "parallel_tool_calls": 2,
        "max_parallel_width": 2,
        "unbatched_tool_calls": 0,
        "repeated_exact_commands": 0,
        "empty_shell_calls": 0,
        "datasource_discovery_calls": 0,
        "first_parallel_call": 1,
        "tool_execution_seconds": 8.0,
        "first_tool_delay_seconds": None,
    }
    assert result["conversation_seconds"] is None
    assert result["trial"]["wall_seconds"] == 120.0
    assert result["trial"]["agent_seconds"] == 100.0
    assert result["trial"]["rewards"] == {"reward": 1.0}


def test_analyze_chat_get_snapshot_and_repeated_commands():
    conversation = {
        "data": {
            "status": "WAITING_FOR_CLIENT_TOOL",
            "llm_conversation_messages": [
                {
                    "llm_conversation_agents": [
                        {
                            "id": "agent-1",
                            "created_at": "2026-08-26T10:00:00Z",
                            "updated_at": "2026-08-26T10:00:02Z",
                            "agent_step_response": [
                                {
                                    "tool_name": "orca_shell_execute",
                                    "tool_input": {"command": "pwd"},
                                },
                                {
                                    "tool_name": "orca_shell_execute",
                                    "tool_input": {"command": "pwd"},
                                },
                            ],
                        }
                    ]
                }
            ],
        }
    }

    result = analyze(conversation)

    assert result["conversation_status"] == "WAITING_FOR_CLIENT_TOOL"
    assert result["planner"]["tool_calls"] == 2
    assert result["planner"]["unbatched_tool_calls"] == 2
    assert result["planner"]["repeated_exact_commands"] == 1


def test_analyze_downloaded_database_export():
    conversation = {
        "data": {
            "llm_conversations": [
                {
                    "id": "conversation-db",
                    "status": "completed",
                    "created_at": "2026-08-26T10:00:00Z",
                    "updated_at": "2026-08-26T10:05:00Z",
                    "llm_conversation_messages": [
                        {
                            "message_config": json.dumps(
                                {"llm_provider": "googleai", "llm_model_name": "flash"}
                            ),
                            "llm_conversation_agents": [
                                {
                                    "llm_conversation_tool_calls": [
                                        {
                                            "tool_name": "orca_shell_execute",
                                            "parameters": '{"command":"pwd"}',
                                            "status": "success",
                                            "metadata": '{"planner_iteration":1}',
                                            "created_at": "2026-08-26T10:02:00Z",
                                            "updated_at": "2026-08-26T10:02:03Z",
                                        }
                                    ]
                                }
                            ],
                        }
                    ],
                }
            ]
        }
    }

    result = analyze(conversation)

    assert result["conversation_id"] == "conversation-db"
    assert result["conversation_seconds"] == 300.0
    assert result["planner"]["first_tool_delay_seconds"] == 120.0
    assert result["planner"]["tool_calls"] == 1
    assert result["planner"]["iterations_seen"] == [1]


def test_analyze_flags_late_fanout_repeated_discovery_and_empty_shell():
    conversation = {
        "data": {
            "ai_get_conversation_v3": {
                "conversation": {"id": "runaway"},
                "tool_calls": [
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":"curl http://grafana/api/datasources"}',
                        "metadata": '{"planner_iteration":1}',
                    },
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":"curl http://grafana/api/datasources"}',
                        "metadata": '{"planner_iteration":2}',
                    },
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":""}',
                        "metadata": '{"planner_iteration":3}',
                    },
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":"query metrics"}',
                        "metadata": (
                            '{"planner_iteration":4,'
                            '"execution_batch_id":"late"}'
                        ),
                    },
                    {
                        "tool_name": "orca_shell_execute",
                        "parameters": '{"command":"query logs"}',
                        "metadata": (
                            '{"planner_iteration":4,'
                            '"execution_batch_id":"late"}'
                        ),
                    },
                ],
            }
        }
    }

    result = analyze(conversation)

    assert result["planner"]["datasource_discovery_calls"] == 2
    assert result["planner"]["empty_shell_calls"] == 1
    assert result["planner"]["first_parallel_call"] == 4
