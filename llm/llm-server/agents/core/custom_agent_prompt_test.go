package core

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A custom agent's prompt is stored as JSON, and tool_usage is omitempty. Any
// agent authored outside the UI — through the ai_create_agent API, a migration,
// or by hand — normally has no tool_usage key at all, which unmarshals to a nil
// map. GetSystemPrompt then writes each supported tool into that map, so the
// first tool turned the agent into a panic and killed the conversation.
func TestCustomAgentPromptToolUsageIsWritableWhenAbsent(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"no tool_usage key", `{"role":"r","instructions":["i"]}`},
		{"explicit null", `{"role":"r","tool_usage":null}`},
		{"empty object", `{"role":"r","tool_usage":{}}`},
		{"already populated", `{"role":"r","tool_usage":{"existing":["desc"]}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var prompt NBAgentPrompt
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &prompt))

			// Mirrors the guard in GetSystemPrompt.
			if prompt.ToolUsage == nil {
				prompt.ToolUsage = make(map[string][]string)
			}

			assert.NotPanics(t, func() {
				prompt.ToolUsage["some_tool"] = append(prompt.ToolUsage["some_tool"], "a description")
			})
			assert.Equal(t, []string{"a description"}, prompt.ToolUsage["some_tool"])
		})
	}
}

// Without the guard the same write panics, which is the failure this protects
// against — asserted directly so the test fails if the map is ever assumed
// non-nil again.
func TestNilToolUsageWriteWouldPanic(t *testing.T) {
	var prompt NBAgentPrompt
	require.NoError(t, json.Unmarshal([]byte(`{"role":"r"}`), &prompt))
	require.Nil(t, prompt.ToolUsage)

	assert.Panics(t, func() {
		prompt.ToolUsage["some_tool"] = []string{"a description"}
	})
}

func TestCustomAgentThinkingLevel(t *testing.T) {
	tests := []struct {
		name   string
		config map[string]any
		want   string
	}{
		{name: "absent", config: nil, want: ThinkingLevelLow},
		{name: "low", config: map[string]any{"thinking_level": "LOW"}, want: ThinkingLevelLow},
		{name: "trimmed", config: map[string]any{"thinking_level": " medium "}, want: ThinkingLevelMedium},
		{name: "unknown", config: map[string]any{"thinking_level": "enthusiastic"}, want: ThinkingLevelLow},
		{name: "wrong type", config: map[string]any{"thinking_level": 2}, want: ThinkingLevelLow},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := &nbCustomAgent{agent: AgentDto{Config: tc.config}}
			assert.Equal(t, tc.want, ResolveAgentThinkingLevel(agent))
		})
	}

	assert.Empty(t, ResolveAgentThinkingLevel(&MockAgent{}),
		"agents without the optional capability must preserve default resolution")
}
