package core

import (
	"github.com/stretchr/testify/require"
	"nudgebee/llm/common"
	"nudgebee/llm/security"
	"nudgebee/llm/tools"
	toolcore "nudgebee/llm/tools/core"
	"testing"
)

func TestShellConfigOwnersDoNotChangeExecutionAction(t *testing.T) {
	shell := tools.ShellTool{}
	seen := map[string]bool{}
	for _, tc := range []struct{ input, owner string }{
		{`{"command":"aws s3 ls"}`, tools.ToolExecuteAwsCliCommand},
		{`{"command":"gcloud projects list"}`, tools.ToolExecuteGcpCliCommand},
		{`{"command":"kubectl get pods"}`, tools.ToolExecuteKubectlCommand},
	} {
		action := NBAgentPlannerToolAction{Tool: "shell_execute", ToolID: "original-call", ToolInput: tc.input}
		owner := configOwnerForAction(shell, "account", action.ToolInput)
		require.Equal(t, tc.owner, owner.Name())
		require.False(t, seen[owner.Name()])
		seen[owner.Name()] = true
		_, unresolved := unresolvedConfigForActionTool(owner, nil, 2)
		require.True(t, unresolved)
		require.Equal(t, "shell_execute", action.Tool)
		require.Equal(t, "original-call", action.ToolID)
	}
	require.Equal(t, "shell_execute", configOwnerForAction(shell, "account", "cat pods.json").Name())
}
func TestShellConfigRejectsStaleSelectionBeforeExecution(t *testing.T) {
	const account = "shell-stale-config-test"
	configs := []toolcore.ToolConfig{{Name: "available", Values: []toolcore.ToolConfigValue{{Name: "id", Value: "target"}}}}
	data, err := common.MarshalJson(configs)
	require.NoError(t, err)
	require.NoError(t, common.CacheSet(toolcore.CacheNamespaceLlmToolConfig, "list_tool_configs:"+account+":aws_execute", data))
	e := plannerExecutor{ctx: security.NewRequestContextForSuperAdmin(), agentRequest: NBAgentRequest{AccountId: account, QueryConfig: toolcore.NBQueryConfig{ToolConfigs: map[string]string{"aws_execute": "removed"}}}}
	owner := configOwnerForAction(tools.ShellTool{}, account, "aws s3 ls")
	_, finish, err := e.followupForMultipleToolConfigs(owner, NBAgentPlannerToolAction{Tool: "shell_execute", ToolID: "call", ToolInput: "aws s3 ls"})
	require.ErrorContains(t, err, "unavailable or unauthorized")
	require.Nil(t, finish)
}
