package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestResolveOrchestratorThinkingLevelConfig(t *testing.T) {
	assert.Equal(t, "high", resolveOrchestratorThinkingLevelConfig("high", true, "low", true), "canonical setting wins")
	assert.Equal(t, "", resolveOrchestratorThinkingLevelConfig("", true, "high", true), "explicit canonical empty disables")
	assert.Equal(t, "high", resolveOrchestratorThinkingLevelConfig("", false, "high", true), "legacy setting remains compatible")
	assert.Equal(t, "", resolveOrchestratorThinkingLevelConfig("", false, "", true), "explicit legacy empty remains compatible")
	assert.Equal(t, "medium", resolveOrchestratorThinkingLevelConfig("", false, "", false), "compiled default remains medium")
}
