package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"nudgebee/llm/security"
	toolcore "nudgebee/llm/tools/core"
)

type mockAliasedTool struct {
	mockTool
	aliases []string
}

func (m mockAliasedTool) GetNameAliases() []string { return m.aliases }

type mockAgentWithTools struct {
	mockPlainAgent
	tools []toolcore.NBTool
}

func (a mockAgentWithTools) GetSupportedTools(_ *security.RequestContext) []toolcore.NBTool {
	return a.tools
}

func TestIsAgentToolAuthorizedToProcessRequest_ToolAliases(t *testing.T) {
	// Register a registry alias: mock_canonical_registry -> mock_alias_registry
	toolcore.RegisterNBToolAlias("mock_alias_registry", "mock_canonical_registry")

	registryTool := mockTool{name: "mock_canonical_registry"}
	structAliasedTool := mockAliasedTool{
		mockTool: mockTool{name: "mock_canonical_struct"},
		aliases:  []string{"mock_alias_struct"},
	}

	agent := mockAgentWithTools{
		mockPlainAgent: mockPlainAgent{name: "test_agent"},
		tools:          []toolcore.NBTool{registryTool, structAliasedTool},
	}

	reqCtx := security.NewRequestContextForSuperAdmin()
	request := NBAgentRequest{
		AccountId:      "test-account",
		ConversationId: "test-conv",
	}

	// 1. Calling canonical tool names directly should succeed
	finish, _, err := IsAgentToolAuthorizedToProcessRequest(
		reqCtx, agent, request,
		NBAgentPlannerToolAction{Tool: "mock_canonical_registry"},
	)
	require.NoError(t, err)
	require.Nil(t, finish)

	finish, _, err = IsAgentToolAuthorizedToProcessRequest(
		reqCtx, agent, request,
		NBAgentPlannerToolAction{Tool: "mock_canonical_struct"},
	)
	require.NoError(t, err)
	require.Nil(t, finish)

	// 2. Calling via registered alias should succeed
	finish, _, err = IsAgentToolAuthorizedToProcessRequest(
		reqCtx, agent, request,
		NBAgentPlannerToolAction{Tool: "mock_alias_registry"},
	)
	require.NoError(t, err)
	require.Nil(t, finish)

	// 3. Calling via struct GetNameAliases() should succeed
	finish, _, err = IsAgentToolAuthorizedToProcessRequest(
		reqCtx, agent, request,
		NBAgentPlannerToolAction{Tool: "mock_alias_struct"},
	)
	require.NoError(t, err)
	require.Nil(t, finish)

	// 4. Calling an unauthorized or non-existent tool should fail
	_, _, err = IsAgentToolAuthorizedToProcessRequest(
		reqCtx, agent, request,
		NBAgentPlannerToolAction{Tool: "completely_unknown_tool"},
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "auth: tool not found")
}

func TestGetNameToTool_ResolvesAliases(t *testing.T) {
	toolcore.RegisterNBToolAlias("mock_tool_alias_gnt", "mock_tool_canonical_gnt")

	t1 := mockAliasedTool{
		mockTool: mockTool{name: "mock_tool_canonical_struct_gnt"},
		aliases:  []string{"mock_alias_struct_gnt"},
	}
	t2 := mockTool{name: "mock_tool_canonical_gnt"}

	toolsList := []toolcore.NBTool{t1, t2}
	toolsMap := getNameToTool(toolsList)

	require.NotNil(t, toolsMap[strings.ToUpper("mock_tool_canonical_struct_gnt")])
	require.NotNil(t, toolsMap[strings.ToUpper("mock_alias_struct_gnt")])
	require.Equal(t, toolsMap[strings.ToUpper("mock_tool_canonical_struct_gnt")], toolsMap[strings.ToUpper("mock_alias_struct_gnt")])

	require.NotNil(t, toolsMap[strings.ToUpper("mock_tool_canonical_gnt")])
	require.NotNil(t, toolsMap[strings.ToUpper("mock_tool_alias_gnt")])
	require.Equal(t, toolsMap[strings.ToUpper("mock_tool_canonical_gnt")], toolsMap[strings.ToUpper("mock_tool_alias_gnt")])
}
