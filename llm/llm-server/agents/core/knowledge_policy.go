package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"nudgebee/llm/common"
	"nudgebee/llm/security"
)

type KnowledgePolicy string

// NBAgentAutomaticKnowledgeConsumer covers legacy custom consumers that load
// knowledge themselves rather than requesting executor-supplied chunks.
type NBAgentAutomaticKnowledgeConsumer interface {
	UsesAutomaticKnowledge() bool
}

func validateKnowledgePolicyForAgent(policy KnowledgePolicy, agent NBAgent) error {
	if policy != KnowledgeLLMOnly || agent.GetPlannerType() != AgentPlannerTypeCustom {
		return nil
	}
	requiresAutomatic := false
	if provider, ok := agent.(NBAgentKnowledgeModeProvider); ok {
		requiresAutomatic = provider.GetKnowledgeMode() != AgentKnowledgeDisabled
	}
	if provider, ok := agent.(NBAgentAutomaticKnowledgeConsumer); ok {
		requiresAutomatic = requiresAutomatic || provider.UsesAutomaticKnowledge()
	}
	if requiresAutomatic {
		return fmt.Errorf("knowledge_policy llm_only is unsupported by custom planner %s: no knowledge tool loop", agent.GetName())
	}
	return nil
}

const (
	KnowledgeAlways   KnowledgePolicy = "always"
	KnowledgeAuto     KnowledgePolicy = "auto"
	KnowledgeLLMOnly  KnowledgePolicy = "llm_only"
	KnowledgeDisabled KnowledgePolicy = "disabled"
)

func parseKnowledgePolicy(value string) (KnowledgePolicy, error) {
	policy := KnowledgePolicy(strings.ToLower(strings.TrimSpace(value)))
	switch policy {
	case "":
		return KnowledgeAuto, nil
	case KnowledgeAlways, KnowledgeAuto, KnowledgeLLMOnly, KnowledgeDisabled:
		return policy, nil
	default:
		return "", fmt.Errorf("invalid knowledge_policy: expected always, auto, llm_only, or disabled")
	}
}

// Read on each invocation: policy changes (especially disabled) must not wait
// for a process-local cache to expire. No tenant or per-agent overrides.
func resolveKnowledgePolicy(ctx *security.RequestContext, accountID string) (KnowledgePolicy, error) {
	if accountID == "" {
		return KnowledgeAuto, nil
	}
	db, err := common.GetDatabaseManager(common.Metastore)
	if err != nil {
		return "", fmt.Errorf("knowledge policy database: %w", err)
	}
	readCtx, cancel := context.WithTimeout(ctx.GetContext(), time.Second)
	defer cancel()
	return readKnowledgePolicy(readCtx, db.Db, accountID)
}

func readKnowledgePolicy(ctx context.Context, db *sqlx.DB, accountID string) (KnowledgePolicy, error) {
	var value sql.NullString
	err := db.GetContext(ctx, &value, `SELECT value FROM cloud_account_attrs WHERE cloud_account_id = $1 AND name = 'knowledge_policy'`, accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return KnowledgeAuto, nil
	}
	if err != nil {
		return "", fmt.Errorf("read account knowledge policy: %w", err)
	}
	return parseKnowledgePolicy(value.String)
}

// Conservative initial platform algorithm. Exact non-operational turns can
// skip retrieval; unknown/new tasks still search. Do not infer scope equivalence
// from fuzzy text or reuse old message-scoped candidate IDs.
func shouldDiscoverKnowledge(policy KnowledgePolicy, request NBAgentRequest) bool {
	if policy == KnowledgeDisabled || policy == KnowledgeLLMOnly {
		return false
	}
	query := strings.TrimSpace(request.Query)
	if query == "" {
		query = strings.TrimSpace(request.OriginalQuery)
	}
	if query == "" {
		return false
	}
	if policy == KnowledgeAlways {
		return true
	}
	if request.ConversationSource == ConversationSourceInvestigation || request.QueryConfig.EventId != "" || IsInvestigationRequestTask(query) {
		return true
	}
	query = strings.Trim(strings.ToLower(query), " .!?\n\t")
	switch query {
	case "hi", "hello", "hey", "thanks", "thank you", "ok", "okay":
		return false
	case "summarize your answer", "make it shorter", "format your answer as a table", "put your answer in bullet points":
		return request.ConversationContext == ""
	default:
		return true
	}
}

// AutomaticKnowledgeAllowed also gates legacy custom/prompt retrieval paths.
// An unresolved policy preserves direct-call compatibility; the executor always
// resolves account policy before invoking an agent or rendering its prompt.
func AutomaticKnowledgeAllowed(request NBAgentRequest) bool {
	return request.KnowledgePolicy == "" || shouldDiscoverKnowledge(request.KnowledgePolicy, request)
}

func applyKnowledgePolicy(request *NBAgentRequest, policy KnowledgePolicy) {
	request.KnowledgePolicy = policy
	request.KnowledgePolicyResolved = true
	if policy != KnowledgeDisabled && policy != KnowledgeLLMOnly {
		return
	}
	request.SkillListsMenu = ""
	request.KBPrestepContent = ""
	request.SkillsContext = ""
	request.KBReferences = nil
	if policy == KnowledgeDisabled {
		// Copy before appending: sibling delegates may share slice storage.
		denied := append([]string(nil), request.Capabilities.DisabledTools...)
		request.Capabilities.DisabledTools = append(denied, "search_skills", "load_skills", "search_docs")
	}
}
