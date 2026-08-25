package tools

import (
	"strings"
	"testing"

	"nudgebee/llm/tools/core"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestExtractLeadingShellCommand pins the parser used by ShellTool's
// confirmation-gate classifier. Every branch here maps to a real
// shell-input shape we've seen in prod logs; getting the leading command
// wrong = the gate misfires (false-positive block on benign commands, or
// false-negative pass-through on mutations).
func TestExtractLeadingShellCommand(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		wantLead    string
		wantCleaned string
	}{
		{"bare kubectl", "kubectl get pods", "kubectl", "kubectl get pods"},
		{"leading whitespace", "   kubectl get pods", "kubectl", "kubectl get pods"},
		{"trailing whitespace", "kubectl get pods   ", "kubectl", "kubectl get pods"},
		{"env var prefix", "KUBECONFIG=/x/y kubectl get pods", "kubectl", "kubectl get pods"},
		{"multiple env vars", "AWS_PROFILE=prod KUBECONFIG=/x kubectl get pods", "kubectl", "kubectl get pods"},
		{"env var with underscore + digits", "MY_VAR_2=foo aws s3 ls", "aws", "aws s3 ls"},
		{"quoted env var with spaces (shlex handles this — strings.Fields would misclassify)", `KUBECONFIG="/etc/kube config/config" kubectl get pods`, "kubectl", "kubectl get pods"},
		{"argument with spaces re-quoted for downstream classifier", `kubectl get pods -l "app in (foo, bar)"`, "kubectl", `kubectl get pods -l "app in (foo, bar)"`},
		{"argument with embedded double quote escaped", `kubectl annotate pod x note="hello \"world\""`, "kubectl", `kubectl annotate pod x "note=hello \"world\""`},
		{"single-quoted env var with spaces", `KUBECONFIG='/etc/kube config/config' kubectl get pods`, "kubectl", "kubectl get pods"},
		{"malformed quoting → bailout", `KUBECONFIG="/etc/kube config kubectl get pods`, "", ""},
		{"pipeline — leading command wins", "kubectl get pods | grep foo | jq .", "kubectl", "kubectl get pods | grep foo | jq ."},
		{"redirect — leading command wins", "kubectl get pods > /tmp/out", "kubectl", "kubectl get pods > /tmp/out"},
		{"non-CLI (grep) returns itself", "grep -R foo /var/log", "grep", "grep -R foo /var/log"},
		{"empty string", "", "", ""},
		{"all whitespace", "   \t  ", "", ""},
		{"cd prefix — bail out (safe default)", "cd /tmp && kubectl delete X", "", ""},
		{"eval prefix — bail out", "eval 'kubectl delete X'", "", ""},
		{"command substitution — bail out", "$(kubectl get pods)", "", ""},
		{"backtick command — bail out", "`kubectl get pods`", "", ""},
		{"bare sh — bail out (subshell wrapper)", "sh -c 'kubectl delete X'", "", ""},
		{"bare bash — bail out", "bash -c 'aws s3 rm bucket'", "", ""},
		{"env wrapper — unwrapped, cleaned drops the wrapper", "env FOO=bar kubectl get pods", "kubectl", "kubectl get pods"},
		{"sudo wrapper — unwrapped, cleaned drops the wrapper", "sudo kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo + env chain — both unwrapped, cleaned is bare command", "sudo env AWS_PROFILE=prod aws s3 rm bucket", "aws", "aws s3 rm bucket"},
		{"sudo with -E flag (no value)", "sudo -E kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo with -u flag (value = next token)", "sudo -u admin kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo with combined flags + user", "sudo -E -u admin kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo with --user=admin (embedded value)", "sudo --user=admin kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo with --user admin (long-form with next-token value)", "sudo --user admin kubectl delete X", "kubectl", "kubectl delete X"},
		{"env with -i flag", "env -i kubectl get pods", "kubectl", "kubectl get pods"},
		{"env with -i and env vars", "env -i KUBECONFIG=/x kubectl get pods", "kubectl", "kubectl get pods"},
		{"env with --unset (value = next token)", "env --unset PATH kubectl get pods", "kubectl", "kubectl get pods"},
		{"env with --chdir (value = next token)", "env --chdir /tmp kubectl get pods", "kubectl", "kubectl get pods"},
		{"env with -S (value = next token — env's split-string)", `env -S "AWS_PROFILE=prod" kubectl get pods`, "kubectl", "kubectl get pods"},
		{"sudo with -S (flag-only — sudo's --stdin, NOT a value-taker)", "sudo -S kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo with grouped short flags -Eu (last char takes value)", "sudo -Eu admin kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo with end-of-options --", "sudo -E -- kubectl delete X", "kubectl", "kubectl delete X"},
		{"sudo -- as the first arg after wrapper", "sudo -- kubectl delete X", "kubectl", "kubectl delete X"},
		{"subshell — bail out (safe default)", "(kubectl delete X)", "", ""},
		{"brace group — bail out (safe default)", "{ kubectl delete X; }", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotLead, gotCleaned := extractLeadingShellCommand(tc.in)
			assert.Equal(t, tc.wantLead, gotLead,
				"shell input %q should extract leading command %q", tc.in, tc.wantLead)
			assert.Equal(t, tc.wantCleaned, gotCleaned,
				"shell input %q should extract cleaned command %q (wrappers/env vars stripped)", tc.in, tc.wantCleaned)
		})
	}
}

// TestShellCommandPrefixRegistry_LookupIsCaseInsensitive pins that the
// prefix registry lookup ignores case — `Kubectl get pods` matches the
// `kubectl` registration.
func TestShellCommandPrefixRegistry_LookupIsCaseInsensitive(t *testing.T) {
	// Prime the registry (populates lazily on first call).
	first := core.LookupShellWrappable("kubectl")
	require.NotEmpty(t, first, "kubectl must be registered — otherwise the CLI tools are missing ShellCommandPrefixes")
	assert.Equal(t, first, core.LookupShellWrappable("KUBECTL"))
	assert.Equal(t, first, core.LookupShellWrappable("Kubectl"))
}

// TestShellCommandPrefixRegistry_UnknownReturnsEmpty pins the safe-default
// behavior: an unknown prefix returns "" (no gate). Otherwise every unknown
// shell command would incorrectly resolve to a random tool.
func TestShellCommandPrefixRegistry_UnknownReturnsEmpty(t *testing.T) {
	assert.Equal(t, "", core.LookupShellWrappable("some_binary_that_isnt_registered_ever"))
	assert.Equal(t, "", core.LookupShellWrappable(""))
}

// TestShellCommandPrefixRegistry_CoversExpectedCLIs pins the reviewed built-in
// CLI set. It verifies that every listed executable is discoverable and owned
// by the intended tool.
//
// Adds a new CLI? Add its prefix to the expected list here AND implement
// core.ShellWrappable on the tool. If the CLI is intentionally NOT
// gate-eligible (rare — most executables aren't), document why in a code
// comment on the tool struct.
func TestShellCommandPrefixRegistry_CoversExpectedCLIs(t *testing.T) {
	// The prefixes below are the executable names as typed at a real shell,
	// paired with the tool that owns them. Grep the codebase for
	// `func .*ShellCommandPrefixes` to see every current registration.
	expected := map[string]string{
		"kubectl":           "kubectl_execute",
		"aws":               "aws_execute",
		"gcloud":            "gcloud_execute",
		"gsutil":            "gcloud_execute",
		"bq":                "gcloud_execute",
		"az":                "azure_execute",
		"gh":                "github_execute",
		"glab":              "gitlab_execute",
		"helm":              "helm_execute",
		"argocd":            "argocd_execute",
		"clickhouse-client": "clickhouse_query_execute",
		"clickhouse":        "clickhouse_query_execute",
		"sqlcmd":            "mssql_query_execute",
		"mysql":             "mysql_query_execute",
		"sqlplus":           "oracle_query_execute",
		"psql":              "postgres_query_execute",
		"rabbitmqadmin":     "rabbit_execute",
		"rabbitmq-api":      "rabbit_execute",
		"rabbitmqctl":       "rabbit_execute",
		"redis-cli":         "redis_command_executer",
		"ssh":               "server_command_executor",
	}
	for prefix, expectedOwner := range expected {
		t.Run(prefix, func(t *testing.T) {
			got := core.LookupShellWrappable(prefix)
			assert.Equal(t, expectedOwner, got,
				"shell prefix %q must be owned by %q — either the tool's ShellCommandPrefixes was removed, or a new tool wrongly claimed this prefix",
				prefix, expectedOwner)
		})
	}
}

// TestShellTool_InferToolRequestType_UnknownCommandIsUnclassified pins that
// non-CLI commands (grep, jq, awk, cat, tail) return "" — auth_agent treats
// unclassified as read (no gate), which is the correct behavior for these
// read-only utility commands. Regressing this would slap a spurious
// confirmation dialog on every `grep` invocation.
func TestShellTool_InferToolRequestType_UnknownCommandIsUnclassified(t *testing.T) {
	tool := ShellTool{AccountId: "test-account"}
	cases := []string{
		"grep -R foo /var/log",
		"jq '.items[].metadata.name' out.json",
		"awk '{print $1}' /tmp/x",
		"cat /etc/hostname",
		"tail -f /var/log/app.log",
		"find / -name '*.conf'",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got, err := tool.InferToolRequestType(nil, "shell_execute", input)
			require.NoError(t, err)
			assert.Empty(t, string(got),
				"non-CLI command %q must be unclassified (empty), NOT flagged as create/update/delete", input)
		})
	}
}

// TestShellTool_InferToolRequestType_BailoutShapesFailClosed pins that
// shapes the leading-command parser cannot reduce (sh -c, cd &&, $(...),
// backtick) still fail closed when they contain a registered CLI.
func TestShellTool_InferToolRequestType_BailoutShapesFailClosed(t *testing.T) {
	tool := ShellTool{AccountId: "test-account"}
	cases := []string{
		"sh -c 'kubectl delete deploy prod'",
		"bash -c 'aws s3 rm s3://bucket/critical'",
		"cd /tmp && kubectl delete deploy prod",
		"$(kubectl delete deploy prod)",
		"`kubectl delete deploy prod`",
		"eval 'kubectl delete deploy prod'",
	}
	for _, input := range cases {
		t.Run(strings.SplitN(input, " ", 2)[0], func(t *testing.T) {
			got, err := tool.InferToolRequestType(nil, "shell_execute", input)
			require.NoError(t, err)
			assert.Equal(t, core.ToolRequestTypeUpdate, got,
				"ambiguous shell shape %q containing a registered CLI must fail closed through confirmation", input)
		})
	}
}

func TestShellTool_InferToolRequestType_ProductionJSONInput(t *testing.T) {
	tool := ShellTool{AccountId: "test-account"}
	input := `{"command":"kubectl delete deployment prod"}`
	got, err := tool.InferToolRequestType(nil, "shell_execute", input)
	require.NoError(t, err)
	if got == "" {
		prompt, promptErr := tool.InferToolRequestTypePrompt(nil, "shell_execute", input)
		require.NoError(t, promptErr)
		assert.NotEmpty(t, prompt, "JSON-wrapped production input must dispatch to kubectl's prompt classifier")
		return
	}
	assert.Equal(t, core.ToolRequestTypeDelete, got,
		"JSON-wrapped production input must dispatch to kubectl's static classifier")
}

func TestShellTool_InferToolRequestType_AmbiguousCLIShapesFailClosed(t *testing.T) {
	tool := ShellTool{AccountId: "test-account"}
	cases := []string{
		"grep ready status.txt && kubectl delete deployment prod",
		"aws s3 ls s3://bucket && aws s3 rm s3://bucket/critical",
		"k=kubectl; $k delete deployment prod",
		"command kubectl delete deployment prod",
	}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			got, err := tool.InferToolRequestType(nil, "shell_execute", input)
			require.NoError(t, err)
			assert.Equal(t, core.ToolRequestTypeUpdate, got)
		})
	}
}

func TestShellTool_InferToolRequestType_AbsolutePathUsesCLIClassifier(t *testing.T) {
	tool := ShellTool{AccountId: "test-account"}
	input := "/usr/local/bin/kubectl get pods"

	got, err := tool.InferToolRequestType(nil, "shell_execute", input)
	require.NoError(t, err)
	assert.NotEqual(t, core.ToolRequestTypeUpdate, got,
		"an absolute path to a read-only CLI command must not be conservatively gated as an update")
	if got == "" {
		prompt, promptErr := tool.InferToolRequestTypePrompt(nil, "shell_execute", input)
		require.NoError(t, promptErr)
		assert.NotEmpty(t, prompt, "absolute executable path must dispatch to the CLI's prompt classifier")
	}
}

// TestSystemToolFactories_ShellWrappableContract asserts that every registered
// system-tool factory that IMPLEMENTS ShellWrappable satisfies the interface
// contract when probed with `core.SystemAccountIDForProbe` — the exact
// sentinel the runtime shell-wrappable registry uses. Specifically:
//   - factory returns a non-nil tool without panicking, and
//   - ShellCommandPrefixes returns at least one prefix.
//
// This test lives in the `tools` package (not `tools/core`) because
// nbSystemTools is populated by each CLI package's init() function. Running
// this in `tools/core` would see an empty map (no factories registered there
// — circular dep prevents core from importing tools) and silently pass with
// 0 subtests. Placing it here forces the tools package's init chain to run
// first, populating the registry.
//
// Factories that fail to instantiate (returning an error or nil) are SKIPPED
// rather than failed — this mirrors prod behavior in
// populateShellWrappableRegistry which logs slog.Error and continues. Real
// prod causes of factory error are environmental (DB unreachable, credentials
// missing) and can't be reproduced in a unit-test context.
//
// Coverage of the specific expected CLI prefixes is enforced separately by
// TestShellCommandPrefixRegistry_CoversExpectedCLIs — this test is the
// symmetric interface-contract check for any new ShellWrappable additions.
func TestSystemToolFactories_ShellWrappableContract(t *testing.T) {
	visited := 0
	wrappableCount := 0
	core.VisitSystemToolFactories(func(name string, factory func(accountId string) (core.NBTool, error)) {
		visited++
		t.Run(name, func(t *testing.T) {
			var tool core.NBTool
			var err error
			assert.NotPanics(t, func() {
				tool, err = factory(core.SystemAccountIDForProbe)
			}, "factory for %q must not panic on SystemAccountIDForProbe", name)
			if err != nil || tool == nil {
				// Mirrors populateShellWrappableRegistry's slog.Error path —
				// environmental factory failures (DB, credentials) drop the
				// tool's prefixes in prod too, and the coverage test flags
				// any missing expected prefix. Skip here.
				t.Skipf("factory error=%v (tool nil? %v) — likely environmental (DB/creds); skipping ShellWrappable check", err, tool == nil)
				return
			}
			wrappable, ok := tool.(core.ShellWrappable)
			if !ok {
				return
			}
			wrappableCount++
			assert.NotPanics(t, func() {
				prefixes := wrappable.ShellCommandPrefixes()
				assert.NotEmpty(t, prefixes, "%q implements ShellWrappable but returned zero prefixes — either drop the interface or declare at least one prefix", name)
			}, "ShellCommandPrefixes for %q must not panic", name)
		})
	})
	// Sanity checks: silent 0-visit / 0-wrappable would defeat the purpose of
	// moving this test out of tools/core.
	assert.Greater(t, visited, 0, "no system-tool factories visited — registry is empty; is this test in the wrong package?")
	assert.Greater(t, wrappableCount, 0, "no ShellWrappable tools found — the registry has factories but none implement ShellWrappable; the whole gate is inert")
}

// TestIsEnvAssignment is a light coverage check for the identifier-name rule
// used by extractLeadingShellCommand. Reuses the shared implementation in
// shell_suspicious.go — same rule as the suspicious-command detector.
func TestIsEnvAssignment(t *testing.T) {
	cases := []struct {
		tok  string
		want bool
	}{
		{"FOO=bar", true},
		{"F=", true},
		{"F_OO=bar", true},
		{"F00=bar", true},
		{"a1=x", true},
		// Not env-var assignments:
		{"kubectl", false},      // no `=`
		{"=value", false},       // empty name
		{"1FOO=bar", false},     // starts with digit
		{"FOO", false},          // no `=`
		{"/path/=x", false},     // path prefix (has `/`)
		{"--flag=value", false}, // starts with `-`
		{"FOO.bar=baz", false},  // `.` isn't identifier
		{"", false},             // empty
	}
	for _, tc := range cases {
		t.Run(tc.tok, func(t *testing.T) {
			assert.Equal(t, tc.want, isEnvAssignment(tc.tok))
		})
	}
}
