package tools

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestSavingsExceedSpend pins the impossible-savings predicate. It replaces a
// prose prompt constraint that was observed being ignored on its first live
// trigger: the agent reported $3,517/mo of open savings against $863.61/mo of
// spend without remark. Emitting the contradiction as data is what makes the
// model act on it.
func TestSavingsExceedSpend(t *testing.T) {
	assert.True(t, savingsExceedSpend(3517.21, 863.61), "4x savings-to-spend must flag")
	assert.True(t, savingsExceedSpend(410.09, 26.32), "the workflow-server case must flag")
	assert.False(t, savingsExceedSpend(200, 863.61), "plausible savings must not flag")
	assert.False(t, savingsExceedSpend(863.61, 863.61), "equal is not greater")
	assert.False(t, savingsExceedSpend(0, 863.61), "no savings must not flag")

	// Trivial rows must not trip the warning: a near-zero resource whose
	// estimate marginally exceeds its spend is noise, not a data defect.
	assert.False(t, savingsExceedSpend(0.03, 0.02), "sub-$1 spend is below the comparison floor")
	assert.False(t, savingsExceedSpend(5, 0), "zero spend carries no signal")
}

// TestRecommendationViewExposesDedupe pins the deduplication contract on the
// view the FinOps agent queries. Alternative purchase options for one
// commitment share a dedupe_group; summing them all inflated a live AWS
// account's savings from $1,184 to $2,806. The window must mirror
// recommendation_groupings_v2 (the query-engine view the Optimise UI reads) so
// both surfaces produce the same total.
func TestRecommendationViewExposesDedupe(t *testing.T) {
	assert.Contains(t, recommendationView, "is_primary_recommendation",
		"the view must expose the dedupe flag the agent filters savings totals on")
	assert.Contains(t, recommendationView, "r.dedupe_group",
		"dedupe_group must be selectable so alternatives can be reported as one opportunity")

	// Same partition key and ordering as recommendation_groupings_v2: highest
	// savings wins within (dedupe_group | resource_id | id, category).
	window := recommendationView[strings.Index(recommendationView, "ROW_NUMBER() OVER"):]
	assert.Contains(t, window, "PARTITION BY")
	assert.Contains(t, window, "r.dedupe_group")
	assert.Contains(t, window, "r.category")
	assert.Contains(t, window, "ORDER BY r.estimated_savings DESC, r.updated_at DESC, r.id")

	assert.Contains(t, RecommendationExecuteTool{}.Description(), "is_primary_recommendation",
		"the tool description must tell callers totals require the dedupe filter")
}
