package agents

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestIsDeterministicCostQuery pins the deterministic cost pre-route. The
// match set must stay precision-first: every "false" case below is a real
// SRE/troubleshooting phrasing that must keep flowing to the LLM router — a
// false positive here silently steals an investigation from the debug agents.
func TestIsDeterministicCostQuery(t *testing.T) {
	cases := []struct {
		query string
		want  bool
	}{
		// The two live misroutes this feature exists for.
		{"Show me the safety band and blast radius for the top rightsizing recommendation. Do not apply anything.", true},
		{"How did our spend change vs the previous 30 days? Break it down by service.", true},

		// Clear cost questions.
		{"What's driving our AWS bill this month?", true},
		{"Why did our cloud bill go up?", true},
		{"How much are we spending on EC2?", true},
		{"Show me cost breakdown by namespace", true},
		{"What is our monthly cost run-rate?", true},
		{"Are we on track against the budget?", true},
		{"Should we buy reserved instances or a savings plan?", true},
		{"What are the top savings opportunities?", true},
		{"How can we save money on this account?", true},
		{"Any cost anomalies this week?", true},
		{"finops summary please", true},
		{"How much have we spent on RDS?", true},
		{"What's our cost per team?", true},
		{"Break down the cost by", true},
		{"Show me cost by  namespace", true},

		// Must NOT match — investigations and ambiguous phrasings stay with the router.
		{"Why is this pod restarting?", false},
		{"This query is expensive, can you optimize it?", false},
		{"What is the cost of downtime for checkout-svc?", false},
		{"Investigate high latency on the payment service", false},
		{"Suspend the cronjob in namespace prod", false},
		{"Show me the top pods by memory", false},
		{"Why did the deployment fail?", false},
		{"Rightsize the JVM heap for this service", false},
		{"Show me recent alerts and events", false},
		{"Is the cost byte-aligned in this struct?", false},
		{"What does the cost permit in this design?", false},
	}

	for _, c := range cases {
		assert.Equal(t, c.want, IsDeterministicCostQuery(c.query), "query: %q", c.query)
	}
}

// TestDeterministicCostRouteYieldsToAgentMention pins the precedence between
// the deterministic cost pre-route and an explicit "@<agent>" mention.
//
// InferAgent runs this decision before anything else and returns FinOps
// directly, so RouterAgent.Execute — the only place that honours a mention —
// never gets consulted. A caller that named its agent was therefore overridden
// by a phrase match on its own payload.
//
// The live case: the auto-optimize apply entrypoint sends "@agent_code_2 {...}"
// whose query reads "Please apply the following Kubernetes resource rightsizing
// recommendations", matching the right-sizing pattern. Every apply on dev
// between 2026-08-20 and 2026-08-21 went to FinOps, which asked a clarifying
// question and left the conversation WAITING forever — no rightsizing pull
// request was raised.
func TestDeterministicCostRouteYieldsToAgentMention(t *testing.T) {
	// Verbatim shape of the apply payload, trimmed to the parts that matter.
	const applyPayload = `@agent_code_2 {"account_id":"a2a30b02","git_repo":` +
		`"https://github.com/nudgebee/nudgebee-infra","mode":"fix","query":` +
		`"Please apply the following Kubernetes resource rightsizing recommendations.` +
		`\n\n**Repository**: nudgebee/nudgebee-infra"}`

	// Precondition: the text really is cost-shaped. If this ever goes false the
	// test below would pass for the wrong reason.
	assert.True(t, IsDeterministicCostQuery(applyPayload),
		"precondition: the payload text is cost-shaped, which is why it was misrouted")

	assert.False(t, shouldDeterministicCostRoute(applyPayload),
		"an explicit @mention must outrank the cost pre-route, or the caller's own "+
			"choice of agent is overridden by a phrase match on its payload")

	cases := []struct {
		name  string
		query string
		want  bool
	}{
		{"mention wins over cost text", "@agent_code_2 apply the rightsizing recommendations", false},
		{"mention wins even for a plain cost question", "@k8s what is our monthly cost?", false},
		{"no mention, cost question still routes", "What are the top rightsizing recommendations by savings?", true},
		{"no mention, not cost, still no route", "Why is this pod restarting?", false},
		{"mid-text @ is not a mention", "email cost report to a@b.com, break down the cost by team", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, shouldDeterministicCostRoute(c.query))
		})
	}
}
