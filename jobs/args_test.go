package jobs_test

import (
	"testing"

	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit/jobs"
)

// TestDeleteExpiredIsUniqueOnlyWhileInFlight pins the one thing about the
// retention sweep's InsertOpts that is easy to get wrong and impossible to
// notice.
//
// DeleteExpiredArgs is an empty struct, so ByArgs makes every instance
// identical. River's *default* unique states include JobStateCompleted, and a
// completed unique job keeps blocking duplicates until the job cleaner removes
// it — a day later by default. Taking that default would silently turn a
// fifteen-minute schedule into one sweep per day, with no error at any point:
// a skipped unique insert is a success that returns the existing job.
//
// So the assertion is specifically that Completed is absent, not that the set
// has some particular size.
func TestDeleteExpiredIsUniqueOnlyWhileInFlight(t *testing.T) {
	t.Parallel()

	states := jobs.DeleteExpiredArgs{}.InsertOpts().UniqueOpts.ByState
	require.NotEmpty(t, states,
		"ByState must be set explicitly; River's default includes completed, which would let the sweep run only once per job-retention window")
	require.NotContains(t, states, rivertype.JobStateCompleted,
		"a completed sweep must not block the next one")

	// River rejects ByState unless these four are present.
	for _, required := range []rivertype.JobState{
		rivertype.JobStateAvailable,
		rivertype.JobStatePending,
		rivertype.JobStateRunning,
		rivertype.JobStateScheduled,
	} {
		require.Contains(t, states, required, "River requires %s in ByState", required)
	}
}
