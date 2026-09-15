package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tmc/langchaingo/llms"
)

// Exercise the same GenerateContent -> createChat path used by native-tool agents.
func TestGPT56FunctionToolsChatCompletions(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		name := "non-streaming"
		if streaming {
			name = "streaming"
		}
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/chat/completions", r.URL.Path)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				assert.Equal(t, "gpt-5.6-sol", body["model"])
				assert.NotEmpty(t, body["tools"])
				assert.NotContains(t, body, "temperature")
				if body["reasoning_effort"] != "none" {
					w.WriteHeader(400)
					_, _ = w.Write([]byte(`{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-5.6-sol in /v1/chat/completions"}}`))
					return
				}
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
				}
			}))
			defer server.Close()
			client, err := New(WithToken("test-key"), WithBaseURL(server.URL), WithModel("gpt-5.6-sol"))
			require.NoError(t, err)
			opts := []llms.CallOption{llms.WithTools([]llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "lookup", Parameters: map[string]any{"type": "object"}}}})}
			if streaming {
				opts = append(opts, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
			}
			response, err := client.GenerateContent(context.Background(), []llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "Look it up")}, opts...)
			require.NoError(t, err)
			require.Len(t, response.Choices, 1)
			require.Len(t, response.Choices[0].ToolCalls, 1)
			assert.Equal(t, "lookup", response.Choices[0].ToolCalls[0].FunctionCall.Name)
		})
	}
}
