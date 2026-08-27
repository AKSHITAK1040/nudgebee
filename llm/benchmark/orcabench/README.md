# NuBi × ORCA-Bench

A standalone Harbor adapter that runs NuBi against the official
[ORCA-Bench](https://github.com/ORCA-bench/ORCA-bench) dataset. It does not use
or modify the benchmark FastAPI server, database, fixtures, or dashboard.

## Requirements

- Linux host with Docker Engine and the Docker Compose plugin
- Python 3.13 and `uv`
- 8 CPUs, 16 GB RAM, and 100 GB free SSD space for a one-task smoke test
- NuBi reachable from the host running Harbor
- An OpenAI-compatible key for ORCA's official verifier

ORCA launches the 19-service OpenTelemetry Astronomy Shop plus Prometheus,
OpenSearch, Jaeger, and Grafana for each trial. Kubernetes is not required.
Start with one concurrent trial; parallel trials multiply the Docker, memory,
and disk requirements.

## NuBi setup

Create or select a NuBi agent named `orca` and instruct it to use only
`orca_shell_execute` for commands in the benchmark environment. The adapter
publishes that benchmark-specific client tool, supplies the official ORCA
instruction unchanged, and executes its requests inside the Harbor trial
environment. The distinct name prevents accidental use of NuBi's server-side
`shell_execute`, which runs in a different workspace.

The tested custom-agent definition is included in
[`orca-agent.yaml`](./orca-agent.yaml). Its persisted `tools` list is empty by
design: the adapter publishes `orca_shell_execute` as a request-scoped client
tool for each benchmark conversation.

Required environment variables:

```bash
export NUBI_URL=http://host.docker.internal:8005
export NUBI_TOKEN=...
export NUBI_ACCOUNT_ID=...
export NUBI_TENANT_ID=...
export OPENAI_API_KEY=...
export OPENAI_BASE_URL=https://your-openai-compatible-endpoint/v1
```

ORCA's official verifier requests the fixed judge model name
`openai-gpt-5.4`. The configured endpoint must support that exact name or map
it to a compatible model alias. `NUBI_LLM_PROVIDER` and `NUBI_LLM_MODEL`
control NuBi's investigation model only; they do not change the verifier.

On Linux, `NUBI_URL` must be an address reachable from the host process. The
adapter itself runs in the Harbor process, while requested shell commands run
inside the trial container.

## Run

From `llm/benchmark`:

```bash
# First published task (default smoke test)
./orcabench/run.sh

# First five tasks, sequentially
N_TASKS=5 ./orcabench/run.sh

# First twenty tasks with two trials running concurrently
# Use only when the host has enough memory and Docker capacity.
N_TASKS=20 N_CONCURRENT_TRIALS=2 ./orcabench/run.sh

# Entire published dataset; intentionally expensive
N_TASKS=all ./orcabench/run.sh

# One or more exact Harbor task package names
TASK_ID=<task-name> ./orcabench/run.sh
TASK_ID=<task-one>,<task-two> ./orcabench/run.sh

# Additional Harbor arguments can be appended
N_TASKS=1 ./orcabench/run.sh --jobs-dir ./orca-runs
```

Use a gradual validation ladder on a new setup:

```bash
N_TASKS=1 ./orcabench/run.sh
N_TASKS=5 ./orcabench/run.sh
N_TASKS=20 ./orcabench/run.sh
```

Run the pinned five-task planner comparison set sequentially:

```bash
./orcabench/run_baseline.sh
```

The exact task IDs, expected incident mechanisms, and selection classes live in
[`baseline_tasks.json`](./baseline_tasks.json). Keep that file unchanged within
an experiment series; changing the task set creates a new baseline version.

Keep `N_CONCURRENT_TRIALS=1` on Apple Silicon until several sequential tasks
complete reliably. Every concurrent trial launches its own telemetry stack.
After a run, inspect the most recent result with `harbor view jobs`, or inspect
the files under `.orca-bench-upstream/jobs/<job-id>/`. A healthy run reports
one or more trials, zero exceptions, and a verifier reward. On no-incident
tasks, `reward: 1.0` with `rca_accuracy: 0` is expected: there is no incident
mechanism or timestamp to match.

The first run clones ORCA-Bench at a pinned commit, installs its frozen
environment, pulls the snapshot image, and stages `/app` into
`~/.cache/orca-bench`. This requires tens of gigabytes and can take a while.
Later runs reuse the cache. Override locations with `ORCA_BENCH_DIR` and
`ORCA_SNAPSHOT_CACHE`.

Optional variables:

| Variable | Default | Purpose |
| --- | --- | --- |
| `NUBI_AGENT_NAME` | `orca` | NuBi agent selected by the `@agent` prefix |
| `NUBI_POLL_INTERVAL` | `2` | Conversation polling interval in seconds |
| `NUBI_CONVERSATION_VISIBILITY_TIMEOUT` | `15` | Maximum seconds to retry an initial transient `chat_get` 404 while the accepted conversation is being persisted |
| `NUBI_CMD_TIMEOUT` | `600` | Per-command timeout |
| `NUBI_TASK_TIMEOUT` | `3600` | NuBi conversation timeout |
| `NUBI_MAX_TOOL_OUTPUT_CHARS` | `12000` | Maximum shell observation size; larger output preserves its head/tail and is saved under `/tmp/nubi-orca/` |
| `NUBI_LLM_PROVIDER` | unset | Per-request NuBi provider override; requires `NUBI_LLM_MODEL` |
| `NUBI_LLM_MODEL` | unset | Per-request NuBi model override; requires `NUBI_LLM_PROVIDER` |
| `N_CONCURRENT_TRIALS` | `1` | Harbor trial concurrency |
| `N_TASKS` | `1` | Maximum tasks; set `all` only for an intentional full run |
| `SNAPSHOT_IMAGE` | official published image | Snapshot image override |
| `SNAPSHOT_PLATFORM` | `linux/amd64` | Docker platform; required by the official AMD64-only snapshot |
| `ORCA_BENCH_COMMIT` | pinned tested commit | Upstream source revision |

The official snapshot currently publishes only `linux/amd64`. Docker Desktop
on Apple Silicon can run it through emulation; expect slower startup and trial
execution. An x86_64 Linux host remains preferable for full runs.

The wrapper passes secret references such as `${NUBI_TOKEN}` to Harbor and lets
Harbor resolve them from the host environment. This keeps literal credentials
out of the process command line and persisted job configuration.

Results remain in Harbor's normal `jobs/` directory. The official verifier
reads `/app/report.md`; the adapter records the NuBi conversation id in Harbor's
agent metadata and `nubi_agent.log`.

## Baseline and comparisons

See [`baseline_report.md`](./baseline_report.md) for the provisional baseline
captured while this adapter was developed, the known sources of invalid runs,
and the fixed protocol to use for future comparisons. Record both completion
quality and runtime: a faster run with an exception, or a successful run against
a different task/model/configuration, is not a comparable improvement.

Each new trial saves the latest NuBi `chat_get` response as
`agent/nubi_conversation.json`. It is intentionally a local benchmark artifact
and may contain commands and telemetry returned during the investigation. To
extract planner metrics, pass that snapshot—or a richer conversation JSON
exported from the UI—together with the trial result:

```bash
python orcabench/analyze_planner.py \
  .orca-bench-upstream/jobs/<job>/<trial>/agent/nubi_conversation.json \
  --trial-result .orca-bench-upstream/jobs/<job>/<trial>/result.json \
  --output .orca-bench-upstream/jobs/<job>/<trial>/planner_metrics.json
```

The richer UI export includes persisted tool-call batch metadata and therefore
supports exact parallel-batch counts. The `chat_get` snapshot remains useful for
tool count, errors, repeated commands, status, and model configuration; metrics
that are not present are reported as unbatched rather than guessed.

Before paying for another full benchmark, apply explicit gates to one downloaded
conversation export:

```bash
uv run python analyze_planner.py \
  ~/Downloads/conversation-<session-id>.json \
  --max-tool-calls 6 \
  --max-wall-seconds 480 \
  --require-parallel \
  --max-first-parallel-call 3 \
  --max-datasource-discovery-calls 1 \
  --forbid-empty-shell-calls
```

The command exits non-zero when a gate fails. Keep the limits explicit: the
no-incident control should be small and fast, while incident tasks may
legitimately need a larger call budget. `--max-first-parallel-call` distinguishes
early evidence fan-out from incidental parallelism late in a long sequential
investigation; it counts the one-based position of the first call in a recorded
parallel batch.

## Important behavior

- The official ORCA dataset, environment, instruction, and verifier are not
  translated into Nudgebee fixtures.
- NuBi retries are not performed; each Harbor trial remains pass@1.
- NuBi should write the answer to `/app/report.md` with `orca_shell_execute`.
  The adapter compares ORCA's pre-created placeholder before and after the run.
  If NuBi never modifies it, the adapter writes NuBi's final conversational
  response there as a fallback.
- For a control/no-incident task, NuBi must create an empty `/app/report.md` as
  required by ORCA's instruction.
