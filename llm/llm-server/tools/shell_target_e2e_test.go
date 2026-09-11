//go:build e2e

package tools

import (
	"fmt"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"nudgebee/llm/common"
	"nudgebee/llm/security"
	"nudgebee/llm/tools/core"
)

// TestShellTargetLive exercises the real credential builder, workspace and relay.
// Explicit opt-in prevents a developer's ordinary TEST_ACCOUNT from running it.
func TestShellTargetLive(t *testing.T) {
	if os.Getenv("SHELL_CONFIG_E2E") != "1" {
		t.Skip("set SHELL_CONFIG_E2E=1; see docs/shell-config-resolution.md")
	}
	env := func(key string) string {
		t.Helper()
		value := os.Getenv(key)
		require.NotEmpty(t, value, key)
		return value
	}
	tenant, user := env("TEST_TENANT"), env("TEST_USER")
	k8s, aws := env("SHELL_E2E_K8S_ACCOUNT"), env("SHELL_E2E_AWS_ACCOUNT")
	awsIdentity, clusterUID := env("SHELL_E2E_AWS_IDENTITY"), env("SHELL_E2E_CLUSTER_UID")
	require.NotEqual(t, k8s, aws)
	require.Regexp(t, `^[0-9]{12}$`, awsIdentity)
	_, uidErr := uuid.Parse(clusterUID)
	require.NoError(t, uidErr)
	sc := security.NewRequestContextForTenantAccountAdmin(tenant, user, []string{k8s, aws})
	for _, tc := range []struct{ name, source, target, owner, command, expected string }{
		{"KubernetesToAWS", k8s, aws, ToolExecuteAwsCliCommand, "aws sts get-caller-identity --query Account --output text", awsIdentity},
		{"AWSToKubernetes", aws, k8s, ToolExecuteKubectlCommand, "kubectl get namespace kube-system -o jsonpath='{.metadata.uid}'", clusterUID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shell := ShellTool{AccountId: tc.source}
			configs, err := ShellTargetConfigs(sc, tc.source, mustShellOwner(t, tc.source, tc.owner))
			require.NoError(t, err)
			var selected core.ToolConfig
			for _, c := range configs {
				for _, v := range c.Values {
					if v.Name == "id" && v.Value == tc.target {
						selected = c
					}
				}
			}
			require.NotEmpty(t, selected.Name, "target must be a registered, accessible config")
			conversation := uuid.NewString()
			t.Logf("workspace account=%s conversation=%s target=%s", tc.source, conversation, selected.Name)
			ctx := core.NewNbToolContext(sc, shell, tc.source, user, conversation, uuid.NewString(), uuid.NewString(), "", nil, "", core.NBQueryConfig{ToolConfigs: map[string]string{tc.owner: selected.Name}}, uuid.NewString())
			run := func(command string) core.NBToolResponse {
				t.Helper()
				result, callErr := shell.Call(ctx, core.NBToolCallRequest{Command: command})
				require.NoError(t, callErr)
				require.Equal(t, core.NBToolResponseStatusSuccess, result.Status, "%s", result.Data)
				return result
			}
			marker := uuid.NewString()
			file := "shell-config-" + uuid.NewString() + ".txt"
			run("printf %s " + common.ShellEscape(marker) + " > " + common.ShellEscape(file))
			t.Cleanup(func() {
				_, cleanupErr := shell.Call(ctx, core.NBToolCallRequest{Command: "rm -f -- " + common.ShellEscape(file)})
				if cleanupErr != nil {
					t.Logf("workspace marker cleanup failed: %v", cleanupErr)
				}
			})
			// CLI first keeps this within the existing detector's supported syntax.
			result := run(tc.command + " && cat " + common.ShellEscape(file))
			require.Contains(t, fmt.Sprint(result.Data), tc.expected)
			require.Contains(t, fmt.Sprint(result.Data), marker)
			if tc.owner == ToolExecuteKubectlCommand {
				ctx.ToolConfig = selected
				result, err = (KubectlExecuteTool{}).Call(ctx, core.NBToolCallRequest{Command: tc.command + " && cat " + common.ShellEscape(file)})
				require.NoError(t, err)
				require.Equal(t, core.NBToolResponseStatusSuccess, result.Status, "%s", result.Data)
				require.Contains(t, fmt.Sprint(result.Data), tc.expected)
				require.Contains(t, fmt.Sprint(result.Data), marker)
				require.Contains(t, fmt.Sprint(run("cat "+common.ShellEscape(file)).Data), marker)
			}
			ctx.QueryConfig.ToolConfigs[tc.owner] = "missing-" + uuid.NewString()
			_, err = shell.Call(ctx, core.NBToolCallRequest{Command: tc.command})
			require.ErrorContains(t, err, "unavailable or unauthorized")
			ctx.QueryConfig.ToolConfigs[tc.owner] = selected.Name
		})
	}
}

func mustShellOwner(t *testing.T, account, name string) core.NBTool {
	t.Helper()
	tool, ok := core.GetNBTool(account, name)
	require.True(t, ok)
	return tool
}
