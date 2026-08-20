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
