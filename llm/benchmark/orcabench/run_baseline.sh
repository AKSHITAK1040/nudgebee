#!/usr/bin/env bash
# Run the fixed ORCA planner baseline set sequentially.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TASK_ID="$(python3 -c 'import json, sys; print(",".join(task["id"] for task in json.load(open(sys.argv[1]))["tasks"]))' "$SCRIPT_DIR/baseline_tasks.json")"

export TASK_ID
export N_TASKS=all
export N_CONCURRENT_TRIALS=1

echo "[nubi-orca] baseline: orca-planner-v1"
echo "[nubi-orca] baseline tasks: $TASK_ID"
exec "$SCRIPT_DIR/run.sh" "$@"
