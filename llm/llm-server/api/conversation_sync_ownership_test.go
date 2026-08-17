package api

import (
	"testing"
	"time"

	"nudgebee/llm/agents/core"
	"nudgebee/llm/config"

	"github.com/stretchr/testify/assert"
)

func msg(workerName string, messageType string, updatedAt *time.Time) core.ConversationMessage {
	return core.ConversationMessage{
		MessageType: messageType,
		WorkerName:  &workerName,
		UpdatedAt:   updatedAt,
	}
}

func ago(d time.Duration) *time.Time {
	t := time.Now().Add(-d)
	return &t
}

// TestDeadWorkerMessagesSkipLocallyOwnedConversations is the split this change exists
// for: cluster replicas recover conversations abandoned by other cluster replicas, and
// leave developer machines' conversations alone. Restarting a laptop's session on a
// pod runs it against different credentials and different code.
func TestDeadWorkerMessagesSkipLocallyOwnedConversations(t *testing.T) {
	assert.True(t, isDeadWorkerMessageRecoverable(msg("llm-server-86d6595fbb-z2spc", "user", ago(time.Minute))),
		"a dead pod's conversation is the cluster's to recover")

	assert.False(t, isDeadWorkerMessageRecoverable(msg(config.LocalWorkerNamePrefix+"nandeshboyz", "user", ago(time.Minute))),
		"a laptop's conversation must be left to that laptop")

	assert.False(t, isDeadWorkerMessageRecoverable(msg("Hemasundar-MB.local", "user", ago(time.Minute))),
		"rows written before the prefix existed must still be recognised as local")

	assert.False(t, isDeadWorkerMessageRecoverable(msg("llm-server-86d6595fbb-z2spc", "followup", ago(time.Minute))),
		"followups are generated, not resumed")
}

// TestOwnOrphanRecovery covers the boot-time sweep a non-leader-eligible process runs
// over its own abandoned work. It deliberately has no liveness heuristic: at boot,
// anything in-progress under our own name predates this process.
func TestOwnOrphanRecovery(t *testing.T) {
	own := config.LocalWorkerNamePrefix + "Hemasundar-MB.local"
	// Stands in for the database-side boot cutoff bootCutoff() computes at runtime.
	cut := time.Now().Add(-time.Minute)

	assert.True(t, isOwnOrphanRecoverable(msg(own, "user", ago(2*time.Hour)), cut),
		"our own recently-abandoned message should be resumed")

	assert.False(t, isOwnOrphanRecoverable(msg(own, "user", ago(72*time.Hour)), cut),
		"past the 48h horizon nobody is still waiting for the answer")

	assert.False(t, isOwnOrphanRecoverable(msg(own, "followup", ago(time.Hour)), cut),
		"followups are generated, not resumed")

	assert.False(t, isOwnOrphanRecoverable(msg(own, "user", nil), cut),
		"a message with no updated_at cannot be aged and must not be resumed blindly")

	// The listener accepts traffic a few seconds before the one-time sweep fires, so a
	// conversation this process just started is in-progress under our own name and
	// would otherwise match. Restarting it would duplicate work already running.
	justStarted := time.Now()
	assert.False(t, isOwnOrphanRecoverable(msg(own, "user", &justStarted), cut),
		"work begun after this process started is live, not an orphan of a previous run")
}

// TestOrphanRecoveryHorizon pins the shared horizon. Both recovery paths read this
// one constant — the boot sweep via time.Since, the dead-worker query via
// make_interval — so the value itself is the contract between them.
func TestOrphanRecoveryHorizon(t *testing.T) {
	assert.Equal(t, 48*time.Hour, config.OrphanRecoveryHorizon)
}
