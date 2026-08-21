package tools

import (
	"fmt"
	"nudgebee/llm/tools/core"
)

func init() {
	core.RegisterNBToolFactory(ToolRecommendationExecuteSql, func(accountId string) (core.NBTool, error) {
		return RecommendationExecuteTool{}, nil
	})
}

// PrimaryRecommendationRank is the window expression that ranks the alternative
// recommendations describing ONE opportunity, so callers can keep just the
// best of them. alias is the table alias of `recommendation` in the caller's
// query.
//
// Rows sharing a dedupe_group are alternative ways to act on the same thing —
// AWS Cost Explorer returns a commitment purchase as 1yr/3yr × All/No-Upfront
// per plan type, and a second producer writes its own row for the same
// opportunity. Only one is purchasable, so summing them overstates savings: on
// a live account 11 EC2 rows summed to $1,257.94 where the best single
// purchase saves $174.89.
//
// This MUST stay identical to recommendation_groupings_v2's window in
// api-server (services/query/metadata.go) — same partition key, same
// highest-savings-wins ordering. That is what keeps the AI's savings total and
// the Optimise page's Total Savings card equal by construction rather than by
// coincidence. TestPrimaryRecommendationRankShape pins the shape here; changing
// one side without the other reopens the 2.2x cross-surface disagreement in
// #36673.
//
// Multi-cloud note: the providers are not treated symmetrically, and that is a
// wart, not a design. AWS producers set dedupe_group; Azure does not, so it
// needs the provider-specific branch below (now duplicated in both services);
// GCP currently ships no commitment recommendations, so nothing groups there
// yet. When GCP CUDs arrive they must NOT get a third special case — the
// durable fix is producer-side dedupe_group for every provider, after which
// this CASE collapses to the dedupe_group branch alone.
// accountAlias is the alias of the joined `cloud_accounts` row, needed for the
// Azure fallback below.
func PrimaryRecommendationRank(alias, accountAlias string) string {
	return fmt.Sprintf(`ROW_NUMBER() OVER (
			PARTITION BY
				CASE
					WHEN %[1]s.dedupe_group IS NOT NULL AND %[1]s.dedupe_group <> '' THEN %[1]s.dedupe_group
					WHEN %[1]s.resource_id IS NOT NULL THEN %[1]s.resource_id::text
					-- Azure ingestion sets neither a dedupe_group nor a resource_id on
					-- a large share of its recommendations (681 of 1823 open Azure rows
					-- on dev), so without this branch they each become their own
					-- partition here while the Optimise page groups them — the exact
					-- cross-surface disagreement this is meant to end, just moved to
					-- Azure. Gated on the provider first so non-Azure rows never
					-- detoast the recommendation jsonb.
					WHEN LOWER(%[2]s.cloud_provider) = 'azure' AND %[1]s.recommendation->>'recommendation_type_id' IS NOT NULL
						THEN %[1]s.cloud_account_id::text || ':'
							|| COALESCE(%[1]s.recommendation->>'recommendation_type_id', '') || ':'
							|| COALESCE(%[1]s.recommendation->>'ext_subid', %[1]s.recommendation->>'subscription_id', '') || ':'
							|| COALESCE(%[1]s.recommendation->>'ext_sku', '')
					ELSE %[1]s.id::text
				END,
				%[1]s.category
			ORDER BY %[1]s.estimated_savings DESC, %[1]s.updated_at DESC, %[1]s.id
		)`, alias, accountAlias)
}

// PrimarySavingsSubquery builds a subquery over `recommendation` that keeps one
// row per opportunity (the highest-saving alternative), projecting groupCol and
// estimated_savings. scopeFilter is AND-ed into the inner WHERE and must be
// written against alias `r2` (e.g. " AND r2.tenant_id = $1").
//
// Every savings roll-up in this service goes through here so the AI cannot
// report one total from spend_summary and a different one from the
// recommendations agent.
// Callers should scope as narrowly as they can: the window function is an
// optimisation fence, so a filter left on the outer join (e.g. one account)
// cannot be pushed in, and the rank would be computed across the whole tenant —
// 247k open recommendations on the largest dev tenant.
func PrimarySavingsSubquery(groupCol, scopeFilter string) string {
	return fmt.Sprintf(`(
			SELECT %[1]s, SUM(estimated_savings) AS estimated_savings
			FROM (
				SELECT r2.%[1]s, r2.estimated_savings, %[2]s AS dedupe_rank
				FROM recommendation r2
				JOIN cloud_accounts ca2 ON ca2.id = r2.cloud_account_id
				WHERE r2.status = 'Open'%[3]s
			) primary_recs
			WHERE dedupe_rank = 1
			GROUP BY %[1]s
		)`, groupCol, PrimaryRecommendationRank("r2", "ca2"), scopeFilter)
}

// recommendationView is the read-only projection the recommendations agent
// queries. Composed at init (not a const) so the dedupe window comes from the
// single shared definition rather than a second copy.
var recommendationView = `
		SELECT r.id::text as id,
			t.name AS tenant,
			ca.account_name AS account,
			ca.id::text AS cloud_account_id,
			(
				CASE
					WHEN cr.meta ->> 'namespace' IS NOT NULL THEN cr.meta ->> 'namespace'
					WHEN cr.meta -> 'config' ->> 'namespace' IS NOT NULL THEN cr.meta -> 'config' ->> 'namespace'
					WHEN r.recommendation -> 'spec' -> 'claimRef' ->> 'namespace' IS NOT NULL THEN r.recommendation -> 'spec' -> 'claimRef' ->> 'namespace'
					WHEN r.recommendation -> 'metadata' ->> 'namespace' IS NOT NULL THEN r.recommendation -> 'metadata' ->> 'namespace'
					ELSE r.recommendation ->> 'namespace'
				END
        	) AS namespace,
			COALESCE(cr.service_name, r.recommendation ->> 'service_name') AS service,
			cr.name AS resource_name,
			r.recommendation ->> 'controller_name'::text AS controller_name,
			COALESCE((r.recommendation ->> 'estimated_saving'::text)::numeric, r.estimated_savings) AS estimated_saving,
			r.created_at,
			r.updated_at,
			r.recommendation::text AS recommendation,
			r.recommendation_action,
			r.category,
			r.note,
			r.severity,
			r.status,
			r.rule_name,
			r.dismissed_reason,
			r.is_dismissed,
			r.snoozed_until,
			r.account_object_id,
			r.updated_by::text,
			r.finops_score,
			r.finops_band,
			r.finops_score_breakdown ->> 'safety_band' AS safety_band,
			r.finops_score_breakdown -> 'impact_summary' ->> 'safety_reason' AS safety_reason,
			(r.finops_score_breakdown -> 'impact_summary' ->> 'dependent_count')::int AS dependent_count,
			(r.finops_score_breakdown -> 'impact_summary' ->> 'production_dependents')::int AS production_dependents,
			r.finops_score_breakdown -> 'impact_summary' -> 'dependents' AS dependents,
			r.dedupe_group,
			-- One row per opportunity; see PrimaryRecommendationRank.
			(` + PrimaryRecommendationRank("r", "ca") + ` = 1) AS is_primary_recommendation
		FROM recommendation r
		LEFT JOIN cloud_resourses cr ON r.resource_id = cr.id
		JOIN tenant t ON r.tenant_id = t.id
		JOIN cloud_accounts ca ON r.cloud_account_id = ca.id
	`

const ToolRecommendationExecuteSql = "recommendation_execute"

type RecommendationExecuteTool struct {
}

func (m RecommendationExecuteTool) Name() string {
	return ToolRecommendationExecuteSql
}

func (m RecommendationExecuteTool) GetType() core.NBToolType {
	return core.NBToolTypeTool
}

func (m RecommendationExecuteTool) Description() string {
	return "Executes a SQL query for recommendation_view and returns the result. Columns: id, namespace, service, resource_name, estimated_saving, category, severity, status, rule_name, is_dismissed, dismissed_reason, snoozed_until, recommendation, finops_score, finops_band, safety_band, safety_reason, dependent_count, production_dependents, dependents, dedupe_group, is_primary_recommendation. " +
		"Savings totals MUST filter is_primary_recommendation (rows sharing a dedupe_group are alternative ways to buy ONE opportunity — only one is purchasable, so summing them overstates savings)."
}

func (m RecommendationExecuteTool) InputSchema() core.ToolSchema {
	return core.ToolSchema{
		Type: core.ToolSchemaTypeObject,
		Properties: map[string]core.ToolSchemaProperty{
			"command": {
				Type:        core.ToolSchemaTypeString,
				Description: "recommendation_view SQL Query to execute",
			},
		},
		Required: []string{"command"},
	}
}

// recommendationMaxJSONChars caps the recommendation JSON field in tool
// responses to prevent token bloat in the ReAct scratchpad. The full JSON
// can exceed 100K chars; truncating to 500 keeps context small while
// preserving the most relevant leading content.
const recommendationMaxJSONChars = 500

// truncateRecommendationJSON caps the "recommendation" field in each row
// so that oversized JSON blobs do not inflate the LLM context window.
func truncateRecommendationJSON(r map[string]any, _ int, _ int) map[string]any {
	v, ok := r["recommendation"]
	if !ok || v == nil {
		return r
	}
	var s string
	switch val := v.(type) {
	case string:
		s = val
	case []byte:
		s = string(val)
	default:
		s = fmt.Sprintf("%v", val)
	}

	const suffix = "...(truncated)"
	if len(s) > recommendationMaxJSONChars {
		maxContentLen := recommendationMaxJSONChars - len(suffix)
		if maxContentLen < 0 {
			maxContentLen = 0
		}
		s = s[:maxContentLen] + suffix
	}
	r["recommendation"] = s
	return r
}

func (m RecommendationExecuteTool) Call(nbRequestContext core.NbToolContext, input core.NBToolCallRequest) (core.NBToolResponse, error) {
	resp, _, err := sqlToolCall(nbRequestContext, input.Command, "recommendation_view", recommendationView, 10, truncateRecommendationJSON)
	if err == nil {
		resp.References = []core.NBToolResponseReference{
			core.GetNudgebeeUIReferenceForClusterDetails(nbRequestContext, []string{"optimize", "summary"}, "Recommendation Details", nil, ""),
		}
	}
	return resp, err
}
