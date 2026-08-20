package adapter

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"nudgebee/services/internal/database"
	"nudgebee/services/internal/database/models"
	"nudgebee/services/llm"
	"nudgebee/services/security"
)

// ValueRefreshOutcome is what came of a dispatched refresh. "Did not update the
// branch" is deliberately split in two: the agent declining because there is
// nothing to change is a different event from the refresh being unable to run,
// and they want different handling — different cooldown treatment, and different
// log severity, since only one of them is a problem.
type ValueRefreshOutcome int

const (
	// ValueRefreshUpdated — the agent changed the branch; the new values are live.
	ValueRefreshUpdated ValueRefreshOutcome = iota
	// ValueRefreshUnnecessary — the agent looked and found nothing to change.
	ValueRefreshUnnecessary
	// ValueRefreshFailed — the refresh could not be run or the agent errored.
	ValueRefreshFailed
)

// DispatchPRValueRefresh re-runs the code agent against an already-open pull
// request so it applies changed rightsizing values, and reports whether the
// update landed (#34959).
//
// It reuses the same followup contract the lifecycle cron uses to address review
// comments — same branch, same credentials, same bounded background run — with a
// different instruction. onDone runs once the agent has finished; success is only
// reported when the agent actually reported success, so the caller can hold off
// recording the new values until they are really on the branch.
//
// Priority over an in-flight review followup is deliberate: a value refresh is
// dispatched even when the pr_followup row is mid-followup. It preempts the row
// rather than running a second agent against the same branch concurrently, since
// two agents pushing the same branch would corrupt it. The mutex it preempts is
// the SAME pr_followup row the review-followup loop claims (#36457) — before
// that fix, this preempted `recommendation_resolution.pr_lifecycle_state`
// directly, which stopped working once review-followup's claim moved to a
// PR-URL-keyed table instead of the resolution row.
func DispatchPRValueRefresh(
	ctx AccountAdapterContext,
	resolution *models.RecommendationResolution,
	prompt string,
	maxRefreshes int,
	cooldown time.Duration,
	onDone func(outcome ValueRefreshOutcome, message string),
) {
	dbms, err := database.GetDatabaseManager(database.Metastore)
	if err != nil {
		onDone(ValueRefreshFailed, "failed to reach the database: "+err.Error())
		return
	}

	meta, tenantID, err := prMetadataForResolution(resolution)
	if err != nil {
		onDone(ValueRefreshFailed, err.Error())
		return
	}

	gitToken, err := getGitTokenForTenant(dbms, tenantID, meta.Provider)
	if err != nil {
		onDone(ValueRefreshFailed, "failed to get git token: "+err.Error())
		return
	}

	// Cadence guardrail (how OFTEN one PR may be rewritten) is unrelated to
	// #36457 and stays exactly where it was: value_refresh_count /
	// last_value_refresh_at on the recommendation_resolution row, checked and
	// stamped atomically so two concurrent replicas can't both win the same
	// cooldown window. This is independent of the mutex claim below — a value
	// refresh can be cadence-blocked without ever touching pr_followup.
	cadenceOK, err := claimValueRefreshCadence(dbms, resolution.Id, maxRefreshes, cooldown)
	if err != nil {
		onDone(ValueRefreshFailed, "failed to check the refresh cadence: "+err.Error())
		return
	}
	if !cadenceOK {
		onDone(ValueRefreshFailed, "another run is already updating this pull request, or a guardrail now blocks it")
		return
	}

	var createdAt time.Time
	if resolution.CreatedAt != nil {
		createdAt = *resolution.CreatedAt
	}
	followupID, claimed, err := claimPRFollowupForValueRefresh(dbms, meta.PRURL, tenantID, createdAt)
	if err != nil {
		releaseValueRefreshCadence(dbms, resolution.Id, "failed to claim the pull request for updating: "+err.Error())
		onDone(ValueRefreshFailed, "failed to claim the pull request for updating: "+err.Error())
		return
	}
	if !claimed {
		// Only a terminal PR (merged/closed/unresolvable) refuses this claim —
		// nothing to refresh. Hand the cadence stamp back so it isn't wasted on
		// a dead PR.
		releaseValueRefreshCadence(dbms, resolution.Id, "pull request already reached a terminal state")
		onDone(ValueRefreshFailed, "pull request already reached a terminal state")
		return
	}

	chatRequest := buildPRFollowupChatRequest(meta, gitToken, prompt)

	reqCtx := security.NewRequestContext(ctx.GetContext(), ctx.GetSecurityContext(), ctx.GetLogger(), nil, nil)

	// Every failure path below hands the claim back with the reason recorded, so a
	// refresh that did not land is visibly failed rather than indistinguishable
	// from one still running.
	fail := func(message string) {
		reason := truncateForStatus("Could not update the pull request with the changed values: " + message)
		releaseValueRefreshCadence(dbms, resolution.Id, reason)
		releasePRFollowupClaim(dbms, followupID, reason)
		onDone(ValueRefreshFailed, message)
	}

	// A no_op is the agent having looked and decided the branch already says what
	// we asked for. That is an answer, not a failure, so it keeps the cooldown the
	// claim consumed — handing it back would re-run the agent on the very next
	// scheduled run and every one after it, since nothing about the inputs has
	// changed. Only the pr_followup mutex is released.
	settle := func(message string) {
		reason := truncateForStatus("Pull request already matches the changed values: " + message)
		releasePRFollowupClaim(dbms, followupID, reason)
		onDone(ValueRefreshUnnecessary, message)
	}

	runPRFollowupAgent(reqCtx, tenantID, chatRequest, "resolution_id", resolution.Id,
		func(tenantCtx *security.RequestContext, response *llm.ChatCompletionResponse, err error) {
			if err != nil {
				fail(err.Error())
				return
			}
			if response == nil || len(response.Response) == 0 {
				fail("code agent returned an empty response")
				return
			}
			outcome := classifyFollowupOutcome(response.Response)
			switch outcome.name {
			case followupOutcomeSuccess.name:
				// The caller (recommendation.recordValueRefresh) records the new
				// values and clears the pr_followup mutex via ResetPRFollowupBudget
				// — deliberately not done here, so the mutex only clears once the
				// values are actually recorded (same ordering guarantee the
				// original design had via recommendation_resolution).
				onDone(ValueRefreshUpdated, "")
			case followupOutcomeNoOp.name:
				settle("code agent reported " + outcome.name)
			default:
				fail("code agent did not update the pull request: " + outcome.name)
			}
		})
}

// prMetadataForResolution reads the pull request metadata a resolution carries,
// falling back to the tenant recorded in that metadata when the row itself has
// none (the same fallback the lifecycle cron uses).
func prMetadataForResolution(resolution *models.RecommendationResolution) (prMetadata, string, error) {
	var meta prMetadata

	blob, ok := resolution.Data.Object().(map[string]any)
	if !ok {
		return meta, "", errValueRefresh("pull request metadata is unreadable")
	}
	raw, err := json.Marshal(blob)
	if err != nil {
		return meta, "", errValueRefresh("pull request metadata is unreadable")
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return meta, "", errValueRefresh("pull request metadata is unreadable")
	}

	if meta.PRURL == "" || meta.RepoURL == "" {
		return meta, "", errValueRefresh("pull request metadata is missing the url or repository")
	}
	if strings.TrimSpace(meta.TenantID) == "" {
		return meta, "", errValueRefresh("pull request metadata has no tenant to scope the run")
	}
	return meta, meta.TenantID, nil
}

// claimValueRefreshCadence checks and stamps the value-refresh cadence
// guardrail (how often ONE pull request may be rewritten) on its own row lock,
// independent of the pr_followup mutex claim. Reports whether the claim was
// won.
//
// The cap and cooldown are conditions of the update rather than a separate
// check, so two replicas evaluating the same row at the same moment cannot
// both decide to proceed: the claim STAMPS the cooldown it checks, so the
// second update — serialised behind the first by the row lock — sees the
// fresh stamp and loses. A refresh that fails hands the stamp back
// (releaseValueRefreshCadence), so a failed attempt still retries on the next
// run rather than waiting out the cooldown.
func claimValueRefreshCadence(dbms *database.DatabaseManager, resolutionID string, maxRefreshes int, cooldown time.Duration) (bool, error) {
	result, err := dbms.Db.Exec(
		`UPDATE recommendation_resolution
		 SET last_value_refresh_at = now()
		 WHERE id = $1
		   AND value_refresh_count < $2
		   AND (last_value_refresh_at IS NULL OR last_value_refresh_at < $3)`,
		resolutionID, maxRefreshes, time.Now().UTC().Add(-cooldown))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// releaseValueRefreshCadence hands the cadence stamp back after a refresh
// that did not land, recording why. Clearing it (rather than restoring the
// previous value) is equivalent — the claim only succeeded because any
// previous stamp had already expired. Guarded against a PR that went terminal
// concurrently, matching recordValueRefresh's own terminal guard, so this
// never resurrects a dead resolution's status_message.
func releaseValueRefreshCadence(dbms *database.DatabaseManager, resolutionID, reason string) {
	_, _ = dbms.Db.Exec(
		`UPDATE recommendation_resolution
		 SET status_message = $1, updated_at = now(), last_value_refresh_at = NULL
		 WHERE id = $2
		   AND (pr_lifecycle_state IS NULL OR pr_lifecycle_state NOT IN ('merged', 'closed', 'unresolvable'))`,
		reason, resolutionID)
}

// claimPRFollowupForValueRefresh force-claims the pr_followup mutex for prURL,
// preempting whatever state a review-followup loop left it in (including
// 'addressing') — a value refresh reflects a change to the numbers under
// review, so it outranks an in-flight followup addressing comments on the
// stale ones. Refuses only a terminal state: can't refresh a merged/closed/
// unresolvable PR. A plain WHERE-conditioned UPDATE is enough for atomicity
// here (no CTE needed) — Postgres serialises concurrent UPDATEs on the same
// row, so a second claim always re-evaluates the WHERE against the first
// claim's committed result.
func claimPRFollowupForValueRefresh(dbms *database.DatabaseManager, prURL, tenantID string, createdAt time.Time) (followupID string, claimed bool, err error) {
	followupID, err = findOrCreatePRFollowup(dbms, prURL, tenantID, createdAt)
	if err != nil {
		return "", false, err
	}
	dbCtx, cancel := context.WithTimeout(context.Background(), prDBOpTimeout)
	defer cancel()
	res, err := dbms.Db.ExecContext(dbCtx,
		`UPDATE pr_followup SET pr_lifecycle_state = 'addressing', pr_followup_pending = false, last_pr_check_at = now()
		 WHERE id = $1 AND pr_lifecycle_state NOT IN ('merged', 'closed', 'unresolvable')`,
		followupID)
	if err != nil {
		return followupID, false, err
	}
	n, _ := res.RowsAffected()
	return followupID, n > 0, nil
}

// releasePRFollowupClaim hands the pr_followup mutex back to 'needs_followup'
// after a refresh that did not land, restoring the row to a normal open state
// rather than reconstructing whatever state a review-followup loop had it in
// — simpler, and safe since a value refresh only ever runs on a PR that was
// already open. Guarded on the row still being 'addressing' so a concurrent
// terminal transition (PR closed mid-run) is never overwritten back to open.
func releasePRFollowupClaim(dbms *database.DatabaseManager, followupID, reason string) {
	_, _ = dbms.Db.Exec(
		`UPDATE pr_followup SET pr_lifecycle_state = 'needs_followup', status_message = $1
		 WHERE id = $2 AND pr_lifecycle_state = 'addressing'`,
		reason, followupID)
}

type valueRefreshError string

func (e valueRefreshError) Error() string { return string(e) }

func errValueRefresh(msg string) error { return valueRefreshError(msg) }
