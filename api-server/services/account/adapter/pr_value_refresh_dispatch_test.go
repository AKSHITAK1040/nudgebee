package adapter

import (
	"strings"
	"testing"

	"nudgebee/services/internal/database"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureExec runs fn against a throwaway sqlmock and returns the SQL it issued.
// The matcher accepts anything so the statement can be asserted on directly,
// which is what these tests need: the difference between the two hand-back paths
// is a column one of them must NOT write.
func captureExec(t *testing.T, fn func(dbms *database.DatabaseManager)) string {
	t.Helper()

	var captured string
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherFunc(func(_, actualSQL string) error {
			captured = actualSQL
			return nil
		})))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()

	mock.ExpectExec(".*").WillReturnResult(sqlmock.NewResult(0, 1))
	fn(&database.DatabaseManager{Db: sqlx.NewDb(db, "postgres")})
	require.NoError(t, mock.ExpectationsWereMet())

	return captured
}

// TestReleaseValueRefreshCadenceHandsBackTheCooldown covers the failure path: a
// refresh that genuinely could not run should be retried by the next scheduled
// run rather than waiting out a cooldown no landed update ever earned. Since
// #36457, the cadence stamp (recommendation_resolution) and the pr_followup
// mutex are two separate rows/statements, so this only asserts the cadence
// side — see TestReleasePRFollowupClaimRestoresNeedsFollowup for the mutex side.
func TestReleaseValueRefreshCadenceHandsBackTheCooldown(t *testing.T) {
	sql := captureExec(t, func(dbms *database.DatabaseManager) {
		releaseValueRefreshCadence(dbms, "res-1", "code agent crashed")
	})

	assert.Contains(t, sql, "last_value_refresh_at = NULL",
		"a failed refresh must not consume the cooldown")
	assert.Contains(t, sql, "recommendation_resolution",
		"the cadence stamp lives on the resolution row, not pr_followup")
}

// TestReleasePRFollowupClaimRestoresNeedsFollowup covers releasing the
// pr_followup mutex a value refresh claimed (#36457): it must hand the row
// back to 'needs_followup' and only if this run is the one still holding it
// ('addressing'), so a concurrent terminal transition is never overwritten.
func TestReleasePRFollowupClaimRestoresNeedsFollowup(t *testing.T) {
	sql := captureExec(t, func(dbms *database.DatabaseManager) {
		releasePRFollowupClaim(dbms, "followup-1", "code agent crashed")
	})

	assert.Contains(t, sql, "pr_followup",
		"the mutex lives on pr_followup, not the resolution row")
	assert.Contains(t, sql, "pr_lifecycle_state = 'needs_followup'")
	assert.Contains(t, sql, "pr_lifecycle_state = 'addressing'",
		"only a row this run claimed may be handed back")
}

// TestReleasePRFollowupClaimAloneKeepsTheCooldown covers the no_op path, which
// is the one that produced hourly agent runs on dev.
//
// The agent read the branch and found nothing to change. The next run
// recomputes the same drift from the same values and would reach the same
// conclusion, so handing the cooldown back buys an identical agent run every
// hour for as long as the pull request stays open. The no_op path
// (DispatchPRValueRefresh's settle closure) releases only the pr_followup
// mutex and never calls releaseValueRefreshCadence, so the cooldown stamp
// claimValueRefreshCadence wrote stays in place — this test pins that the
// mutex-release statement alone never touches last_value_refresh_at.
func TestReleasePRFollowupClaimAloneKeepsTheCooldown(t *testing.T) {
	sql := captureExec(t, func(dbms *database.DatabaseManager) {
		releasePRFollowupClaim(dbms, "followup-1", "no_op")
	})

	assert.False(t, strings.Contains(sql, "last_value_refresh_at"),
		"the mutex-release statement must never touch the cadence stamp:\n"+sql)
}
