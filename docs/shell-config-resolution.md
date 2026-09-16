# Shell target configuration

Part of #28804. This change restores target-account selection for recognizable
AWS, GCP, Azure and Kubernetes commands issued through `shell_execute`.

The executing action remains `shell_execute`, including its original tool ID,
command, workspace account and conversation ID. Configuration is resolved under
the owning tool (`aws_execute`, `gcloud_execute`, `azure_execute` or
`kubectl_execute`). Existing explicit-name/metadata selection and account-picker
follow-ups are reused. The selected name persists in `QueryConfig.ToolConfigs`.

Shell candidates are filtered for the current caller after reading the shared
configuration cache. An unavailable saved selection is an error, not permission
to substitute another account. Shell writes additionally require write access to
the selected target; selecting an account does not approve an operation.

Kubernetes uses the existing workspace shim. `NB_TOOL_CONFIG_NAME` carries the
cluster choice, and the workspace endpoint resolves it to the relay target
account. A short-lived per-command workspace token authenticates the original workspace account and binds the permitted relay target; ordinary pod tokens cannot select a different cluster.
Direct kubectl requests with verbs its static classifier cannot recognize conservatively require write access to the target.
Direct `kubectl_execute` also keeps its original workspace rather than creating
files in the selected cluster account's workspace.

## Validation before promotion

- From a Kubernetes conversation, request an AWS read with multiple accessible
  AWS accounts and no saved selection. Confirm exactly one account picker, no
  command before selection, and successful resume of the original action.
- Repeat with an explicit account name and with one accessible account.
- From an AWS conversation, select a Kubernetes cluster, run a kubectl read into
  a relative file, and read that file in the next shell call. Verify original
  workspace/conversation IDs and selected relay account independently.
- Verify read-only target access cannot authorize a write even when the original
  workspace account is writable. Account selection and write approval are separate.
- Repeat parallel/delegated selections and both ReAct4 and ReAct3 execution.

## Follow-up PR

The existing shlex-based detection remains intentionally unchanged except for
recognizing kubectl as a configurable target. Its broad token matching can
recognize words in data, misses opaque saved scripts/dynamic executables, and
selects the first detected provider in mixed-provider commands. These remain
limitations; this PR does not promise reliable target discovery for those inputs.
A separate PR will refine executable-command detection and introduce explicit
metadata for opaque scripts, without requiring another model call.

Local unit checks are not deployed end-to-end validation. Do not mark the above
promotion checks complete merely because the unit suite passes.

## Implementation validation (2026-09-10)

- Focused target resolution, workspace routing/token binding, and resume tests:
  passed with the race detector.
- `make lint`: zero issues.
- Full `make validate` using an explicit fail-fast local PostgreSQL endpoint:
  every package except `common` passed, including tools, executor, API and workspace.
  `common` failed in existing RabbitMQ tests: port 5672 accepted TCP but AMQP
  handshake timed out; `TestPublishClose` subsequently dereferenced a nil publisher.
- Fresh-context review: all identified source blockers addressed.
- No deployed validation performed. Full validation remains pending a working local RabbitMQ service. The PR is
  opened with this limitation explicitly recorded.

## Opt-in live regression tests

Run from `llm/llm-server` with your usual development database, LLM, workspace,
and relay configuration. Deploy this branch's llm-server API changes too: the
live workspace calls the configured API, not the in-process test handler.
Use test accounts in one tenant with working AWS and Kubernetes integrations.
The harness grants its request context admin access to the explicit account IDs;
it does not test login or user-role provisioning.

```sh
export SHELL_CONFIG_E2E=1
export TEST_TENANT='<tenant UUID>'
export TEST_USER='<user UUID>'
export SHELL_E2E_K8S_ACCOUNT='<Nudgebee Kubernetes account UUID>'
export SHELL_E2E_AWS_ACCOUNT='<Nudgebee AWS account UUID>'
export SHELL_E2E_OTHER_AWS_ACCOUNT='<second Nudgebee AWS account UUID>'
export SHELL_E2E_AWS_IDENTITY='<expected 12-digit AWS account number>'
export SHELL_E2E_CLUSTER_UID='<expected kube-system namespace UID>'

go test -tags=e2e ./tools -run '^TestShellTargetLive$' -count=1 -v -timeout=10m
go test -tags=e2e ./agents -run '^TestShellConfigPickerResumeLive$' -count=1 -v -timeout=15m
```

Obtain the expected AWS number and namespace UID independently from the intended
targets. Do not derive expected values from the commands under test.

`TestShellTargetLive` checks Kubernetes-to-AWS credential injection, AWS-to-Kubernetes
shell and direct kubectl routing, original workspace file continuity, and stale
selection rejection. It uses actual registered configs and live commands, with
no credential/config mocks. It writes only a unique relative marker file and
attempts to remove it afterward. It does not change cloud or cluster resources.

`TestShellConfigPickerResumeLive` starts the Kubernetes orchestrator, requires an
AWS account picker, resumes through the production conversation entrypoint, and
checks persisted successful shell output for the expected AWS identity. Another
turn checks selection reuse. Two accessible AWS configs are required. LLM config
auto-selection is temporarily disabled in this test process to exercise the
picker; do not run it in parallel with other live conversation tests. LLM planning
can still vary: choosing another tool is a failure to exercise this scenario,
not evidence that credential resolution is broken. Conversations are retained
and their IDs logged for inspection. LLM calls incur normal provider usage.

These tests do not cover opaque script detection (deferred), GCP/Azure live
identities, or browser rendering of the picker. Run the manual checklist above
for those UI aspects. Compilation alone does not validate the deployed flow.
