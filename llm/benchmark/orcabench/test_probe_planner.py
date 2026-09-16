import json

from orcabench.probe_planner import CASES, command_for, extract_waiting_calls


def test_extract_waiting_calls_and_commands():
    response = {
        "data": {
            "llm_conversation_messages": [
                {
                    "id": "message-1",
                    "llm_conversation_agents": [
                        {
                            "id": "agent-1",
                            "status": "waiting_for_client_tool",
                            "agent_step_response": json.dumps(
                                [
                                    {
                                        "tool_id": "call-1",
                                        "tool_input": '{"command":"query-metrics"}',
                                    },
                                    {
                                        "tool_id": "call-2",
                                        "tool_input": "query-logs",
                                    },
                                ]
                            ),
                        }
                    ],
                }
            ]
        }
    }

    message_id, agent_id, calls = extract_waiting_calls(response)

    assert message_id == "message-1"
    assert agent_id == "agent-1"
    assert [command_for(call) for call in calls] == ["query-metrics", "query-logs"]


def test_orca_first_batch_probe_uses_verified_realistic_capabilities():
    case = CASES["orca-first-batch"]

    assert case.expect_discovery is False
    assert "<verified_environment_capabilities>" in case.prompt
    assert "executable.python3=available" in case.prompt
    assert "datasource.webstore-metrics=prometheus:Prometheus" in case.prompt
