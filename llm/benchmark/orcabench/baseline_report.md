# ORCA-Bench baseline

**Date:** 2026-08-26  
**Status:** Provisional engineering baseline; not a publishable model score

This baseline captures what was observed while the NuBi ORCA adapter and agent
instructions were being stabilized. It is intended to answer “did the next run
improve?” without mixing model quality, harness reliability, and infrastructure
failures into one number.

## Fixed environment

| Component | Baseline value |
| --- | --- |
| ORCA-Bench commit | `b8ab93ae5d2716ff8e7190196151acd4a15bf3d7` |
| Snapshot | `orcabench/sre-otel-snapshot:data-0418-harbor-template` |
| Snapshot platform | `linux/amd64` |
| Host used during development | Apple Silicon macOS through Docker Desktop AMD64 emulation |
| Trial concurrency | `1` |
| NuBi agent | `orca`, using only `orca_shell_execute` |
| Planner | `react_3` |
| Investigation model | User-configured Gemini Flash alias; exact provider/model alias must be recorded for a comparable rerun |
| Official judge | Endpoint alias for ORCA's fixed `openai-gpt-5.4` judge name |

## Observed runs

These runs are retained as engineering evidence. They are not one homogeneous
sample: prompts, adapter behavior, model aliases, and infrastructure health
changed during development.

| Sample | Trials | Exceptions | Mean reward | RCA accuracy | Wall time | Interpretation |
| --- | ---: | ---: | ---: | ---: | ---: | --- |
| Single control | 1 | 0 | 1.000 | 0.000 | 7m 11s | Correct no-incident outcome; `rca_accuracy: 0` is expected for this control |
| Five-task run | 3 | 2 | 0.269 | 0.000 | 1h 55m 56s | Invalid as a quality baseline; two HTTP failures followed a lost DB connection |
| Five-task rerun | 3 | 2 | 0.453 | 0.200 | 44m 42s | Reliability still invalidated by one HTTP and one runtime exception |
| Single control | 1 | 0 | 1.000 | 0.000 | 10m 15s | Successful but slower than the first control |
| Three-task run | 3 | 0 | 0.333 | 0.000 | 25m 32s | First clean multi-task reliability sample; one hallucination classification |
| Single control | 1 | 0 | 1.000 | 0.000 | 8m 18s | Latest clean run before final adapter hardening |
| Single control | 1 | 0 | 1.000 | 0.000 | 9m 07s | Clean end-to-end run after adapter retry work |

The clean single-task controls establish a current runtime band of roughly
**7–10 minutes** on the emulated development host. They do not establish RCA
quality because the control task has no incident to identify. The clean
three-task run establishes only a small-sample pass@1 reward of **0.333** and
must not be generalized to the full dataset.

## React4 observed runs (2026-08-30)

These runs exercise the fixed task IDs in `baseline_tasks.json` with the
React4 custom-agent path and the ORCA prompt/adapter changes in this branch.
They are recorded separately from the historical React3 results above because
the planner, prompt, adapter capability map, and telemetry helper changed.

| Task | Class | Reward | RCA accuracy | Hallucination | Wall time | Planner shape | Interpretation |
| --- | --- | ---: | ---: | ---: | ---: | --- | --- |
| `5b71925cf2820c86` | No-incident control | 1.000 | 0.000 | — | 7m 53s | 10 iterations | Correct zero-byte report; valid per-task no-incident baseline |
| `bb4a194e8fa38186` | Focused application incident, before final grounding fix | 0.000 | 0.000 | 0.000 | 14m 16s | — | Incorrect no-incident conclusion; retained as the pre-fix comparison |
| `bb4a194e8fa38186` | Focused application incident, current candidate | 1.000 | 1.000 | 0.000 | 16m 44s | 24 iterations; 37 shell calls; 6 parallel batches | Correct root cause, but well outside the runtime and round-trip targets |

The current React4 result improves correctness on the focused incident from reward
`0.000` to `1.000`, and demonstrates real sibling-call fan-out. It does **not**
establish an efficiency improvement: the successful incident run took 16m 44s
and 24 iterations. These are valid per-task observed baselines. A stable React4
full-suite baseline still requires two sequential executions of all five pinned
tasks with the same server commit, agent revision, and model alias.

## Comparison protocol

A future result is comparable only when all of the following are unchanged and
recorded with the result:

1. ORCA commit, snapshot image, host architecture, and trial concurrency.
2. Exact ordered `TASK_ID` list. Do not compare two `N_TASKS=n` runs unless the
   resolved task package names are known to be identical.
3. NuBi server commit, agent YAML revision, planner, exact LLM provider/model
   alias, and judge endpoint/model mapping.
4. No NuBi DB outage, provider HTTP failure, Docker failure, or benchmark
   exception. Report these separately as harness reliability failures.
5. Pass@1 only; do not silently retry failed model trials.

Use one stable smoke task for iteration, then a fixed five-task set for broader
comparison:

```bash
TASK_ID=<stable-control-task> N_CONCURRENT_TRIALS=1 ./orcabench/run.sh
TASK_ID=<task-1>,<task-2>,<task-3>,<task-4>,<task-5> \
  N_CONCURRENT_TRIALS=1 ./orcabench/run.sh
```

For every comparison, capture:

| Dimension | Metrics |
| --- | --- |
| Reliability | requested tasks, completed trials, exceptions by type |
| Quality | mean reward, RCA accuracy, hallucination rate, per-task reward |
| Efficiency | total wall time, per-task wall time, planner turns, shell tool calls |
| Investigation shape | number of execution batches, calls per batch, maximum parallel calls, repeated/broad discovery rounds |

## Initial targets

These are directional engineering targets, not ORCA-defined pass criteria:

- **Reliability:** 100% completed trials and zero harness exceptions on the
  fixed five-task set.
- **Quality:** no regression in per-task reward; report aggregate reward only
  after reliability is clean.
- **Runtime:** first reduce the clean single-task median below 7 minutes on the
  same emulated host. The earlier 4-minute aspiration is not yet supported by
  the observed baseline.
- **Execution:** capability discovery once, followed by one metrics/logs/traces
  evidence fan-out and at most one targeted confirmation round.

## Next baseline run

Before treating this as a stable baseline, select and commit the exact five
task package names, record the exact NuBi model alias and server commit, and run
the set at least twice sequentially. Keep both result directories; variance
between identical runs is part of the baseline rather than noise to discard.
