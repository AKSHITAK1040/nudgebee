//go:build e2e

package agents

import (
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"nudgebee/llm/agents/core"
	"nudgebee/llm/common"
	"nudgebee/llm/config"
	"nudgebee/llm/security"
	"nudgebee/llm/tools"
	toolcore "nudgebee/llm/tools/core"
	"os"
	"testing"
)

// TestShellConfigPickerResumeLive asserts persisted tool output, not LLM prose.
func TestShellConfigPickerResumeLive(t *testing.T) {
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
	source, target, other := env("SHELL_E2E_K8S_ACCOUNT"), env("SHELL_E2E_AWS_ACCOUNT"), env("SHELL_E2E_OTHER_AWS_ACCOUNT")
	expected := env("SHELL_E2E_AWS_IDENTITY")
	require.NotEqual(t, target, other)
	require.Regexp(t, `^[0-9]{12}$`, expected)
	sc := security.NewRequestContextForTenantAccountAdmin(tenant, user, []string{source, target, other})
	owner, ok := toolcore.GetNBTool(source, tools.ToolExecuteAwsCliCommand)
	require.True(t, ok)
	configs, err := tools.ShellTargetConfigs(sc, source, owner)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(configs), 2, "register two accessible AWS targets")
	var selected string
	for _, c := range configs {
		for _, v := range c.Values {
			if v.Name == "id" && v.Value == target {
				selected = c.Name
			}
		}
	}
	require.NotEmpty(t, selected)
	old := config.Config.LlmConfigAutoSelectionEnabled
	config.Config.LlmConfigAutoSelectionEnabled = false
	t.Cleanup(func() { config.Config.LlmConfigAutoSelectionEnabled = old })
	session := "shell-picker-" + uuid.NewString()
	agent := newK8sOrchestratorAgent(source)
	query := "Use shell_execute directly to run exactly: aws sts get-caller-identity --query Account --output text. Ask me to choose the AWS account if needed. Do not delegate or use aws_execute."
	first, err := core.HandleConversationSessionRequest(sc, agent, user, source, session, query)
	require.NoError(t, err)
	t.Logf("session=%s conversation=%s message=%s (retained for inspection)", session, first.ConversationId, first.MessageId)
	require.Equal(t, core.ConversationStatusWaiting, first.Status)
	require.Equal(t, core.FollowupTypeToolConfig, first.FollowupRequest.FollowupType)
	require.Equal(t, tools.ToolExecuteAwsCliCommand, first.FollowupRequest.ToolName)
	require.Contains(t, first.FollowupRequest.FollowupOptions, selected)
	require.NotEmpty(t, first.FollowupRequest.ToolId)
	require.NotEqual(t, uuid.Nil, first.FollowupRequest.AgentId)
	dbm, err := common.GetDatabaseManager(common.Metastore)
	require.NoError(t, err)
	countSuccess := func(message string) int {
		t.Helper()
		var count int
		err := dbm.Db.Get(&count, `SELECT count(*) FROM llm_conversation_tool_calls WHERE conversation_id=$1 AND message_id=$2 AND tool_name='shell_execute' AND parameters LIKE '%get-caller-identity%' AND status='success' AND response LIKE $3`, first.ConversationId, message, "%"+expected+"%")
		require.NoError(t, err)
		return count
	}
	var before int
	err = dbm.Db.Get(&before, `SELECT count(*) FROM llm_conversation_tool_calls WHERE conversation_id=$1 AND tool_name='shell_execute' AND parameters LIKE '%get-caller-identity%' AND status IN ('success','error','failed')`, first.ConversationId)
	require.NoError(t, err)
	require.Zero(t, before, "execution must wait for selection")
	resumedAgent, _, _, err := core.ResolveAgentByConversationAgentId(sc, first.FollowupRequest.AgentId, source)
	require.NoError(t, err)
	require.NotNil(t, resumedAgent)
	resumed, err := core.HandleConversationSessionRequest(sc, resumedAgent, user, source, session, selected,
		core.ConversationSessionRequestWithConversationId(uuid.NullUUID{UUID: uuid.MustParse(first.ConversationId), Valid: true}),
		core.ConversationSessionRequestWithMessageId(uuid.NullUUID{UUID: uuid.MustParse(first.MessageId), Valid: true}),
		core.ConversationSessionRequestWithAgentId(uuid.NullUUID{UUID: first.FollowupRequest.AgentId, Valid: true}),
		core.ConversationSessionRequestWithIsResume(true))
	require.NoError(t, err)
	require.Equal(t, core.ConversationStatusCompleted, resumed.Status)
	require.Equal(t, 1, countSuccess(first.MessageId), "resume must execute once against selected account")
	var original int
	err = dbm.Db.Get(&original, `SELECT count(*) FROM llm_conversation_tool_calls WHERE conversation_id=$1 AND tool_id=$2 AND tool_name='shell_execute' AND parameters LIKE '%get-caller-identity%' AND status='success' AND response LIKE $3`, first.ConversationId, first.FollowupRequest.ToolId, "%"+expected+"%")
	require.NoError(t, err)
	require.Equal(t, 1, original, "the original waiting shell action must resume")
	next, err := core.HandleConversationSessionRequest(sc, agent, user, source, session, "Use shell_execute again to run exactly: aws sts get-caller-identity --query Account --output text. Reuse my selected account. Do not delegate.")
	require.NoError(t, err)
	require.Equal(t, core.ConversationStatusCompleted, next.Status)
	require.Positive(t, countSuccess(next.MessageId), "next turn must reuse persisted selection")
}
