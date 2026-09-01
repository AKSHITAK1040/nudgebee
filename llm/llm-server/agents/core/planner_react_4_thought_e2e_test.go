//go:build e2e

package core

import (
	"context"
	"os"
	"testing"

	"nudgebee/llm/llms/googleai"

	"github.com/stretchr/testify/require"
	"github.com/tmc/langchaingo/llms"
)

// TestReAct4PlannerMetadataNativeRoundTrip is a narrow paid provider probe for
// the planner-only _thought and _memory_refs arguments. It deliberately stops
// short of executing a real tool: the contract under test is model schema
// adherence, executable-input stripping, and exact signed replay into the next
// Gemini turn.
func TestReAct4PlannerMetadataNativeRoundTrip(t *testing.T) {
	if os.Getenv("RUN_LIVE_PLANNER_PROBES") != "1" {
		t.Skip("set RUN_LIVE_PLANNER_PROBES=1 to make the paid provider calls")
	}
	apiKey := os.Getenv("LLM_PROVIDER_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_PROVIDER_API_KEY required")
	}
	modelName := os.Getenv("LLM_MODEL_NAME")
	if modelName == "" {
		modelName = "gemini-3.7-flash"
	}

	model, err := googleai.New(
		context.Background(),
		googleai.WithAPIKey(apiKey),
		googleai.WithDefaultModel(modelName),
	)
	require.NoError(t, err)

	tools := withReact4MemoryAttributionSchemas(withReact4ThoughtSchemas([]llms.Tool{{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        "inspect_resources",
			Description: "Inspect named Kubernetes resources without changing them.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"namespace": map[string]any{"type": "string"},
					"resources": map[string]any{
						"type":  "array",
						"items": map[string]any{"type": "string"},
					},
				},
				"required": []string{"namespace", "resources"},
			},
		},
	}}))
	messages := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem,
			"Use native function calls. Every function call must include the required _thought field as one short user-displayable intent sentence and required _memory_refs attribution. Never use XML.\n\n<user_memory>default_namespace: payments</user_memory>\n<memory_index>\n[m1] preferences: default_namespace=payments\n</memory_index>"),
		llms.TextParts(llms.ChatMessageTypeHuman,
			"Call inspect_resources once for pods and services. Apply the default namespace from memory and attribute it."),
	}

	response, err := model.GenerateContent(context.Background(), messages, llms.WithTools(tools))
	require.NoError(t, err)
	require.NotEmpty(t, response.Choices)
	choice := response.Choices[0]
	require.Len(t, choice.ToolCalls, 1)
	require.NotNil(t, choice.ToolCalls[0].FunctionCall)

	planner := &NBReActPlanner4{}
	actions, finish, err := planner.parseCompletion(choice)
	require.NoError(t, err)
	require.Nil(t, finish)
	require.Len(t, actions, 1)
	action := actions[0]
	require.NotEmpty(t, action.Log, "live model omitted required _thought; native schema contract is not reliable")
	require.NotContains(t, action.ToolInput, react4ThoughtArgument)
	require.JSONEq(t, `{"namespace":"payments","resources":["pods","services"]}`, action.ToolInput)
	require.Len(t, action.MemoryRefs, 1)
	require.Equal(t, 1, action.MemoryRefs[0].Position)
	require.NotEmpty(t, action.MemoryRefs[0].Note)
	require.NotEmpty(t, action.ThoughtSignature, "live Gemini response did not exercise signed replay")
	t.Logf("native arguments=%s", action.NativeToolInput)
	t.Logf("execution arguments=%s display thought=%q", action.ToolInput, action.Log)

	// Replay the exact original call, not the stripped execution input. Gemini
	// thinking models also require the opaque signature from the first response.
	steps := []NBAgentPlannerToolActionStep{{
		Action:      action,
		Observation: `{"pods":2,"services":1}`,
		Status:      ToolStatusSuccess,
	}}
	messages = append(messages, planner.renderStepsToMessages(steps)...)
	opts := []llms.CallOption{llms.WithTools(tools)}
	signatureOpt := thoughtSignatureOption(steps)
	require.NotNil(t, signatureOpt)
	opts = append(opts, signatureOpt)
	replayed, err := model.GenerateContent(context.Background(), messages, opts...)
	require.NoError(t, err, "provider rejected exact native replay")
	require.NotEmpty(t, replayed.Choices)
}
