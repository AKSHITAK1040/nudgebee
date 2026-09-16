"""Harbor agent that drives NuBi through its client-tool protocol.

ORCA-Bench supplies the instruction and a live terminal environment. NuBi does
the reasoning; every ``orca_shell_execute`` request is executed in that environment
and returned to the same conversation. The official ORCA verifier remains the
sole scorer and reads ``/app/report.md`` after this agent exits.
"""

import asyncio
import base64
import json
import logging
import os
import tempfile
import time
from pathlib import Path
from typing import Any

import httpx
from harbor.agents.base import BaseAgent
from harbor.environments.base import BaseEnvironment
from harbor.models.agent.context import AgentContext

logger = logging.getLogger(__name__)

_ORCA_SHELL_TOOL_SCHEMA = {
    "name": "orca_shell_execute",
    "description": (
        "Execute a non-interactive shell command in the ORCA-Bench environment "
        "and return combined stdout/stderr. Do not use interactive programs."
    ),
    "input": {
        "type": "object",
        "properties": {"command": {"type": "string"}},
        "required": ["command"],
    },
}

_DONE = {"COMPLETED"}
_FAILED = {"FAILED", "TERMINATED", "KILLED"}
_WAITING_FOR_TOOL = "WAITING_FOR_CLIENT_TOOL"
_AGENT_WAITING = "waiting_for_client_tool"
_DEFAULT_MAX_TOOL_OUTPUT_CHARS = 12_000
_DEFAULT_CONVERSATION_VISIBILITY_TIMEOUT = 15.0
_TELEMETRY_HELPER_SOURCE = Path(__file__).with_name("telemetry_helper.py")
_TELEMETRY_HELPER_TARGET = "/tmp/nubi-orca/telemetry.py"
_ENVIRONMENT_DISCOVERY_COMMAND = r"""printf 'os='
uname -srm 2>/dev/null || printf 'unknown'
printf '\nworking_directory='
pwd
for executable in sh bash python3 curl jq; do
  if command -v "$executable" >/dev/null 2>&1; then
    printf '\nexecutable.%s=available' "$executable"
  else
    printf '\nexecutable.%s=missing' "$executable"
  fi
done
if [ -n "${GRAFANA_URL+x}" ]; then
  printf '\nenvironment.GRAFANA_URL=available'
  if command -v python3 >/dev/null 2>&1; then
    printf '\n'
    python3 - <<'PY'
import base64
import json
import os
import urllib.request

try:
    request = urllib.request.Request(f"{os.environ['GRAFANA_URL']}/api/datasources")
    request.add_header(
        "Authorization",
        "Basic " + base64.b64encode(b"admin:admin").decode("ascii"),
    )
    with urllib.request.urlopen(request, timeout=5) as response:
        datasources = json.load(response)
    for datasource in datasources:
        uid = datasource.get('uid', 'unknown')
        datasource_type = datasource.get('type', 'unknown')
        print(
            "datasource."
            f"{uid}="
            f"{datasource_type}:"
            f"{datasource.get('name', 'unknown')}"
        )
        print(f"datasource.{uid}.proxy_path=/api/datasources/proxy/uid/{uid}")
        if datasource_type == "prometheus":
            print(f"datasource.{uid}.query_path=/api/v1/query")
            print(f"datasource.{uid}.query_range_path=/api/v1/query_range")
            print(f"datasource.{uid}.time_unit=unix_seconds")
        elif datasource_type == "jaeger":
            print(f"datasource.{uid}.services_path=/api/services")
            print(f"datasource.{uid}.traces_path=/api/traces")
            print(f"datasource.{uid}.time_unit=unix_microseconds")
        elif "opensearch" in datasource_type:
            print(f"datasource.{uid}.search_path=/<index-pattern>/_search")
            print(f"datasource.{uid}.time_unit=iso8601")
except Exception:
    pass
PY
  fi
else
  printf '\nenvironment.GRAFANA_URL=missing'
fi
"""


def _extract_balanced_json_object(value: str) -> str | None:
    start_index = 0
    while True:
        start = value.find("{", start_index)
        if start < 0:
            return None
        depth = 0
        in_string = False
        escaped = False
        for index in range(start, len(value)):
            char = value[index]
            if escaped:
                escaped = False
                continue
            if char == "\\":
                escaped = True
                continue
            if char == '"':
                in_string = not in_string
                continue
            if in_string:
                continue
            if char == "{":
                depth += 1
            elif char == "}":
                depth -= 1
                if depth == 0:
                    candidate = value[start : index + 1]
                    try:
                        json.loads(candidate)
                        return candidate
                    except json.JSONDecodeError:
                        break
        start_index = start + 1


def _parse_tool_calls(step_response: str | list | dict | None) -> list[dict]:
    if step_response is None:
        return []
    if isinstance(step_response, list):
        return [item for item in step_response if isinstance(item, dict)]
    if isinstance(step_response, dict):
        return [step_response]
    try:
        parsed = json.loads(step_response)
    except (json.JSONDecodeError, TypeError):
        return []
    if isinstance(parsed, list):
        return [item for item in parsed if isinstance(item, dict)]
    return [parsed] if isinstance(parsed, dict) else []


def _extract_command(tool_input: str | dict | None) -> str:
    if not tool_input:
        return ""
    if isinstance(tool_input, dict):
        return str(tool_input.get("command", ""))
    try:
        parsed = json.loads(tool_input)
    except (json.JSONDecodeError, TypeError):
        parsed = None
    if isinstance(parsed, dict):
        return str(parsed.get("command", ""))
    candidate = _extract_balanced_json_object(tool_input)
    if candidate:
        try:
            parsed = json.loads(candidate)
            if isinstance(parsed, dict):
                return str(parsed.get("command", ""))
        except json.JSONDecodeError:
            pass
    return tool_input


def _truncate_output(value: str, limit: int) -> str:
    if len(value) <= limit:
        return value
    marker = f"\n[output truncated: {len(value)} -> {limit} characters]\n"
    available = max(0, limit - len(marker))
    head = (available + 1) // 2
    tail = available // 2
    return f"{value[:head]}{marker}{value[-tail:] if tail else ''}"


def _conversation_data(response: dict) -> dict:
    data = response.get("data", response)
    return data if isinstance(data, dict) else {}


def _final_response(response: dict) -> str:
    data = _conversation_data(response)
    direct = data.get("response")
    if isinstance(direct, list):
        return "\n".join(str(item) for item in direct if item)
    if direct:
        return str(direct)
    for message in reversed(data.get("llm_conversation_messages") or []):
        if message.get("response"):
            return str(message["response"])
    return ""


def _is_explicit_no_incident_response(value: str) -> bool:
    normalized = " ".join(value.lower().split())
    no_incident = any(
        phrase in normalized
        for phrase in (
            "no incident occurred",
            "no incident detected",
            "no active incident",
        )
    )
    empty_artifact = "/app/report.md" in normalized and any(
        phrase in normalized
        for phrase in ("empty file", "empty report", "zero-byte", "0-byte")
    )
    return no_incident and empty_artifact


class NuBiHarborAgent(BaseAgent):
    """Harbor custom agent backed by NuBi."""

    @staticmethod
    def name() -> str:
        return "nubi-orca"

    def version(self) -> str:
        return "0.1.0"

    def __init__(self, logs_dir: Path, model_name: str | None = None, **kwargs):
        super().__init__(logs_dir=logs_dir, model_name=model_name, **kwargs)
        values = {**os.environ, **(self.extra_env or {})}
        self._url = values["NUBI_URL"].rstrip("/")
        self._token_header = values.get("NUBI_TOKEN_HEADER", "X-ACTION-TOKEN")
        self._token = values["NUBI_TOKEN"]
        self._account_id = values["NUBI_ACCOUNT_ID"]
        self._tenant_id = values["NUBI_TENANT_ID"]
        self._user_id = values.get("NUBI_USER_ID", "")
        self._agent_name = values.get("NUBI_AGENT_NAME", "orca")
        self._poll_interval = float(values.get("NUBI_POLL_INTERVAL", "2"))
        self._conversation_visibility_timeout = float(
            values.get(
                "NUBI_CONVERSATION_VISIBILITY_TIMEOUT",
                str(_DEFAULT_CONVERSATION_VISIBILITY_TIMEOUT),
            )
        )
        self._task_timeout = int(values.get("NUBI_TASK_TIMEOUT", "3600"))
        self._command_timeout = int(values.get("NUBI_CMD_TIMEOUT", "600"))
        self._max_tool_output_chars = max(
            1000,
            int(
                values.get(
                    "NUBI_MAX_TOOL_OUTPUT_CHARS",
                    str(_DEFAULT_MAX_TOOL_OUTPUT_CHARS),
                )
            ),
        )
        self._llm_provider = values.get("NUBI_LLM_PROVIDER", "").strip()
        self._llm_model = values.get("NUBI_LLM_MODEL", "").strip()
        self._report_invalidated = False
        self._submitted_tool_ids: set[str] = set()
        self._pending_tool_results: dict[str, dict[str, str]] = {}

    @property
    def _headers(self) -> dict[str, str]:
        return {
            self._token_header: self._token,
            "x-tenant-id": self._tenant_id,
            "Content-Type": "application/json",
        }

    async def setup(self, environment: BaseEnvironment) -> None:
        del environment
        self.logs_dir.mkdir(parents=True, exist_ok=True)

    async def run(
        self,
        instruction: str,
        environment: BaseEnvironment,
        context: AgentContext,
    ) -> None:
        log_path = self.logs_dir / "nubi_agent.log"
        # Harbor may reuse one agent instance across trials. A failed report
        # write in one trial must not invalidate the next trial's artifact.
        self._report_invalidated = False
        self._submitted_tool_ids.clear()
        self._pending_tool_results.clear()
        initial_report_state = await self._report_state(environment)
        instruction = await self._instruction_with_environment_hint(
            instruction, environment
        )
        async with httpx.AsyncClient(timeout=30.0) as client:
            conversation_id = await self._start_conversation(client, instruction)
            self._log(log_path, f"[start] conversation_id={conversation_id}")
            context.metadata = {"nubi_conversation_id": conversation_id}

            deadline = time.monotonic() + self._task_timeout
            visibility_deadline = min(
                deadline,
                time.monotonic() + self._conversation_visibility_timeout,
            )
            final = {}
            conversation_visible = False
            while time.monotonic() < deadline:
                if conversation_visible:
                    final = await self._poll(client, conversation_id)
                else:
                    final = await self._poll_until_visible(
                        client,
                        conversation_id,
                        visibility_deadline,
                        log_path,
                    )
                    conversation_visible = True
                status = str(_conversation_data(final).get("status", "")).upper()
                self._log(log_path, f"[poll] status={status}")
                if status == _WAITING_FOR_TOOL:
                    self._write_conversation_snapshot(final)
                    await self._execute_client_tools(
                        client, final, conversation_id, environment, log_path
                    )
                elif status in _DONE:
                    self._write_conversation_snapshot(final)
                    await self._ensure_report(
                        environment, _final_response(final), initial_report_state
                    )
                    return
                elif status in _FAILED:
                    self._write_conversation_snapshot(final)
                    raise RuntimeError(f"NuBi conversation ended with status {status}")
                await asyncio.sleep(self._poll_interval)
            self._write_conversation_snapshot(final)
        raise TimeoutError(f"NuBi conversation exceeded {self._task_timeout}s")

    async def _instruction_with_environment_hint(
        self,
        instruction: str,
        environment: BaseEnvironment,
    ) -> str:
        helper_available = await self._stage_telemetry_helper(environment)
        result = await environment.exec(
            command=_ENVIRONMENT_DISCOVERY_COMMAND,
            timeout_sec=10,
        )
        if result.return_code != 0:
            return instruction
        capabilities = result.stdout.strip()
        if not capabilities:
            return instruction
        if helper_available:
            capabilities += (
                f"\nhelper.telemetry={_TELEMETRY_HELPER_TARGET}"
                "\nhelper.metrics=python3 /tmp/nubi-orca/telemetry.py --uid <uid> "
                "metrics --query <promql> [--start <iso-or-epoch> "
                "--end <iso-or-epoch> --step <duration>]"
                "\nhelper.logs=python3 /tmp/nubi-orca/telemetry.py --uid <uid> "
                "logs --index <pattern> (--body <json> | --body-file <path>)"
                "\nhelper.services=python3 /tmp/nubi-orca/telemetry.py --uid <uid> "
                "services"
                "\nhelper.traces=python3 /tmp/nubi-orca/telemetry.py --uid <uid> "
                "traces --service <service> --start <iso-or-epoch> "
                "--end <iso-or-epoch> [--limit <count> --tags <json>]"
            )
        return (
            f"{instruction}\n\n"
            "<verified_environment_capabilities>\n"
            f"{capabilities}\n"
            "</verified_environment_capabilities>\n"
            "The ORCA adapter verified this allowlisted capability map before "
            "starting the investigation. Reuse it; do not spend a tool call "
            "rediscovering the OS, working directory, listed executables, or "
            "whether GRAFANA_URL exists. Reuse any datasource UID/type entries "
            "as authoritative for this run; do not call /api/datasources when "
            "those entries are present. Only rediscover a datasource if a "
            "provided UID fails. When helper.telemetry is present, use it for "
            "Grafana proxy authentication, request encoding, and timestamp "
            "conversion instead of rebuilding those mechanics. Refer to "
            "GRAFANA_URL by variable name; its value "
            "is intentionally not included. This map does not replace "
            "source-specific schema discovery or evidence collection."
        )

    async def _stage_telemetry_helper(
        self, environment: BaseEnvironment
    ) -> bool:
        try:
            encoded = base64.b64encode(_TELEMETRY_HELPER_SOURCE.read_bytes()).decode(
                "ascii"
            )
            command = (
                "mkdir -p /tmp/nubi-orca && python3 -c \"import base64; "
                f"open('{_TELEMETRY_HELPER_TARGET}', 'wb').write("
                f"base64.b64decode('{encoded}'))\""
            )
            result = await environment.exec(command=command, timeout_sec=10)
            return result.return_code == 0
        except (OSError, AttributeError):
            return False

    async def _poll_until_visible(
        self,
        client: httpx.AsyncClient,
        conversation_id: str,
        deadline: float,
        log_path: Path,
    ) -> dict:
        while True:
            try:
                return await self._poll(client, conversation_id)
            except httpx.HTTPStatusError as error:
                if error.response.status_code != 404 or time.monotonic() >= deadline:
                    raise
                self._log(
                    log_path,
                    "[poll] conversation not visible yet; retrying transient 404",
                )
                remaining = deadline - time.monotonic()
                await asyncio.sleep(min(self._poll_interval, max(0, remaining)))

    async def _start_conversation(
        self, client: httpx.AsyncClient, instruction: str
    ) -> str:
        payload = {
            "query": f"@{self._agent_name} {instruction}",
            "account_id": self._account_id,
            "user_id": self._user_id,
            "tenant_id": self._tenant_id,
            "async": True,
            "client_tools": [_ORCA_SHELL_TOOL_SCHEMA],
        }
        if self._llm_provider and self._llm_model:
            payload["config"] = {
                "llm_provider": self._llm_provider,
                "llm_model_name": self._llm_model,
            }
        response = await client.post(
            f"{self._url}/v1/completions/chat",
            headers=self._headers,
            json=payload,
        )
        response.raise_for_status()
        conversation_id = (_conversation_data(response.json())).get("conversation_id")
        if not conversation_id:
            raise RuntimeError("NuBi returned no conversation_id")
        return str(conversation_id)

    async def _poll(self, client: httpx.AsyncClient, conversation_id: str) -> dict:
        response = await client.post(
            f"{self._url}/v1/completions/chat_get",
            headers=self._headers,
            json={
                "conversation_id": conversation_id,
                "account_id": self._account_id,
            },
        )
        response.raise_for_status()
        return response.json()

    async def _execute_client_tools(
        self,
        client: httpx.AsyncClient,
        response: dict,
        conversation_id: str,
        environment: BaseEnvironment,
        log_path: Path,
    ) -> None:
        data = _conversation_data(response)
        for message in data.get("llm_conversation_messages") or []:
            for agent in message.get("llm_conversation_agents") or []:
                if agent.get("status") != _AGENT_WAITING:
                    continue
                calls = _parse_tool_calls(agent.get("agent_step_response"))
                pending_calls = [
                    call
                    for call in calls
                    if not (str(call.get("tool_id") or "") in self._submitted_tool_ids)
                ]
                if not pending_calls:
                    continue
                uncached_calls = [
                    call
                    for call in pending_calls
                    if not str(call.get("tool_id") or "")
                    or str(call.get("tool_id") or "") not in self._pending_tool_results
                ]
                executed_results = await asyncio.gather(
                    *(
                        self._execute_tool_call(call, environment, log_path)
                        for call in uncached_calls
                    )
                )
                self._pending_tool_results.update(
                    {
                        result["tool_id"]: result
                        for result in executed_results
                        if result["tool_id"]
                    }
                )
                executed_without_ids = iter(
                    result for result in executed_results if not result["tool_id"]
                )
                results = [
                    (
                        self._pending_tool_results[str(call.get("tool_id") or "")]
                        if str(call.get("tool_id") or "")
                        else next(executed_without_ids)
                    )
                    for call in pending_calls
                ]
                submitted = await self._submit_tool_results(
                    client,
                    conversation_id,
                    str(agent.get("message_id") or message.get("id") or ""),
                    str(agent.get("id") or ""),
                    results,
                )
                if submitted:
                    self._submitted_tool_ids.update(
                        result["tool_id"] for result in results if result["tool_id"]
                    )
                    for result in results:
                        self._pending_tool_results.pop(result["tool_id"], None)
                else:
                    self._log(
                        log_path,
                        "[submit] conversation resumed before tool result submission; retrying after poll",
                    )
                return

    async def _execute_tool_call(
        self,
        call: dict,
        environment: BaseEnvironment,
        log_path: Path,
    ) -> dict[str, str]:
        command = _extract_command(call.get("tool_input"))
        self._log(log_path, f"[exec] {command!r}")
        result = await environment.exec(
            command=command or "true",
            timeout_sec=self._command_timeout,
        )
        output = "\n".join(part for part in (result.stdout, result.stderr) if part)
        status = "SUCCESS"
        if result.return_code != 0:
            status = "ERROR"
            if "/app/report.md" in command:
                self._report_invalidated = True
            output = f"[exit code {result.return_code}]\n{output}"
        elif not output:
            output = "[command completed successfully with no output]"
        if len(output) > self._max_tool_output_chars:
            tool_id = str(call.get("tool_id") or "")
            safe_tool_id = (
                "".join(
                    char if char.isalnum() or char in "-_" else "_" for char in tool_id
                )
                or "unknown"
            )
            artifact_path = f"/tmp/nubi-orca/tool-output-{safe_tool_id}.txt"
            try:
                mkdir_result = await environment.exec(
                    command="mkdir -p /tmp/nubi-orca",
                    timeout_sec=10,
                )
                if mkdir_result.return_code != 0:
                    raise RuntimeError("could not create /tmp/nubi-orca")
                with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8") as file:
                    file.write(output)
                    file.flush()
                    await environment.upload_file(file.name, artifact_path)
                artifact_note = f"\n[full output saved to {artifact_path}]"
            except Exception as error:
                self._log(log_path, f"[artifact] could not save full output: {error}")
                artifact_note = "\n[full output could not be saved]"
            output = (
                _truncate_output(
                    output,
                    self._max_tool_output_chars - len(artifact_note),
                )
                + artifact_note
            )
        return {
            "tool_id": str(call.get("tool_id") or ""),
            "result": output,
            "status": status,
        }

    async def _submit_tool_results(
        self,
        client: httpx.AsyncClient,
        conversation_id: str,
        message_id: str,
        agent_id: str,
        results: list[dict[str, Any]],
    ) -> bool:
        response = await client.post(
            f"{self._url}/v1/completions/client-tool-result",
            headers=self._headers,
            json={
                "conversation_id": conversation_id,
                "message_id": message_id,
                "agent_id": agent_id,
                "account_id": self._account_id,
                "async": True,
                "results": results,
            },
        )
        if getattr(response, "status_code", 200) == 409:
            detail = response.text.lower()
            if "already submitted" in detail:
                return True
            if "currently in progress" in detail:
                return False
        response.raise_for_status()
        return True

    async def _ensure_report(
        self,
        environment: BaseEnvironment,
        final_response: str,
        initial_report_state: str = "",
    ) -> None:
        # The completed conversation is authoritative when it explicitly selects
        # ORCA's no-incident contract. A failed compound command may have written
        # a stale candidate report before a later sub-command failed; always
        # replace that partial artifact with the required zero-byte report.
        if _is_explicit_no_incident_response(final_response):
            result = await environment.exec(": > /app/report.md", timeout_sec=30)
            if result.return_code != 0:
                raise RuntimeError("Could not create ORCA's empty no-incident report")
            return

        # A zero-byte report is ORCA's required output when no incident occurred.
        # Compare the root-owned entrypoint placeholder's state before and after
        # the run; a changed mtime or size proves the agent intentionally wrote it.
        if (
            not self._report_invalidated
            and await self._report_state(environment) != initial_report_state
        ):
            return
        raise RuntimeError(
            "NuBi completed without intentionally creating /app/report.md"
        )

    async def _report_state(self, environment: BaseEnvironment) -> str:
        result = await environment.exec(
            "stat -c '%y:%s' /app/report.md", timeout_sec=10
        )
        if result.return_code != 0:
            return ""
        return result.stdout.strip()

    @staticmethod
    def _log(path: Path, message: str) -> None:
        logger.info(message)
        with path.open("a", encoding="utf-8") as handle:
            handle.write(f"{time.strftime('%H:%M:%S')} {message}\n")

    def _write_conversation_snapshot(self, response: dict) -> None:
        if not response:
            return
        path = self.logs_dir / "nubi_conversation.json"
        path.write_text(
            json.dumps(response, indent=2, sort_keys=True),
            encoding="utf-8",
        )
