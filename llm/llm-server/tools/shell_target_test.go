package tools

import (
	"fmt"
	"github.com/stretchr/testify/require"
	"nudgebee/llm/common"
	"nudgebee/llm/security"
	"nudgebee/llm/tools/core"
	"nudgebee/llm/workspace"
	"testing"
)

func cacheShellTargets(t *testing.T, account, owner string, configs []core.ToolConfig) {
	t.Helper()
	data, err := common.MarshalJson(configs)
	require.NoError(t, err)
	require.NoError(t, common.CacheSet(core.CacheNamespaceLlmToolConfig, fmt.Sprintf("list_tool_configs:%s:%s", account, owner), data))
}
func TestShellTargetSelection(t *testing.T) {
	const account = "shell-target-selection"
	configs := []core.ToolConfig{
		{Name: "allowed", Values: []core.ToolConfigValue{{Name: "id", Value: "target-a"}}},
		{Name: "other", Values: []core.ToolConfigValue{{Name: "id", Value: "target-b"}}},
	}
	cacheShellTargets(t, account, ToolExecuteAwsCliCommand, configs)
	tool := ShellTool{AccountId: account}
	ctx := core.NewNbToolContext(security.NewRequestContextForTenantAccountAdmin("tenant", "user", []string{account, "target-a"}), tool, account, "user", "conversation", "message", "agent", "", nil, "", core.NBQueryConfig{}, "call")
	selected, err := ResolveShellTarget(ctx, ToolExecuteAwsCliCommand)
	require.NoError(t, err)
	require.Equal(t, "allowed", selected.Name)
	ctx.QueryConfig.ToolConfigs = map[string]string{ToolExecuteAwsCliCommand: "other"}
	_, err = ResolveShellTarget(ctx, ToolExecuteAwsCliCommand)
	require.ErrorContains(t, err, "unavailable or unauthorized")
	ctx.Ctx = security.NewRequestContextForSuperAdmin()
	ctx.QueryConfig.ToolConfigs = nil
	_, err = ResolveShellTarget(ctx, ToolExecuteAwsCliCommand)
	require.ErrorContains(t, err, "selection")
	ctx.QueryConfig.ToolConfigs = map[string]string{ToolExecuteAwsCliCommand: "allowed"}
	selected, err = ResolveShellTarget(ctx, ToolExecuteAwsCliCommand)
	require.NoError(t, err)
	require.Equal(t, "allowed", selected.Name)
}

type targetWorkspaceCapture struct {
	workspace.WorkspaceManager
	account, conversation, command string
	env                            map[string]string
}

func (m *targetWorkspaceCapture) ExecuteOrLazyCreate(_ *security.RequestContext, account, conversation, command string, env map[string]string) (string, error) {
	m.account, m.conversation, m.command, m.env = account, conversation, command, env
	return "pod-a", nil
}
func TestShellKubernetesTargetKeepsConversationWorkspace(t *testing.T) {
	const account = "original-aws-workspace"
	cacheShellTargets(t, account, ToolExecuteKubectlCommand, []core.ToolConfig{{Name: "target-cluster", Values: []core.ToolConfigValue{{Name: "id", Value: "different-k8s-account"}}}})
	capture := &targetWorkspaceCapture{}
	tool := ShellTool{AccountId: account, workspaceManager: capture}
	ctx := core.NewNbToolContext(security.NewRequestContextForSuperAdmin(), tool, account, "user", "conversation", "message", "agent", "", nil, "", core.NBQueryConfig{ToolConfigs: map[string]string{ToolExecuteKubectlCommand: "target-cluster"}}, "call")
	_, err := tool.Call(ctx, core.NBToolCallRequest{Command: "kubectl get pods > pods.txt"})
	require.NoError(t, err)
	require.Equal(t, account, capture.account)
	require.Equal(t, "conversation", capture.conversation)
	require.Equal(t, "target-cluster", capture.env[workspace.ENV_NB_TOOL_CONFIG_NAME])
	require.Equal(t, "kubectl get pods > pods.txt", capture.command)
}
