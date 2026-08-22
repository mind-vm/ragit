package jobs

import (
	"errors"
	"fmt"
	"testing"

	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/jryannel/ragit/embed"
	"github.com/jryannel/ragit/extract"
)

func TestClassify_Success_ReturnsNil(t *testing.T) {
	require.NoError(t, classify(nil))
}

func TestClassify_TransportFailures_AreRetried(t *testing.T) {
	for _, cause := range []error{
		fmt.Errorf("wrap: %w", extract.ErrUnavailable),
		fmt.Errorf("wrap: %w", embed.ErrUnavailable),
	} {
		got := classify(cause)
		require.Error(t, got)
		require.True(t, errors.Is(got, cause) || got.Error() == cause.Error(),
			"transient errors should be returned as-is for River's normal retry, got: %v", got)

		var cancelErr *river.JobCancelError
		require.False(t, errors.As(got, &cancelErr), "a transport failure must not be canceled")
		var snoozeErr *river.JobSnoozeError
		require.False(t, errors.As(got, &snoozeErr), "a transport failure must not be snoozed")
	}
}

func TestClassify_RateLimited_IsSnoozed(t *testing.T) {
	got := classify(fmt.Errorf("wrap: %w", embed.ErrRateLimited))
	require.Error(t, got)

	var snoozeErr *river.JobSnoozeError
	require.True(t, errors.As(got, &snoozeErr), "a rate limit should snooze the job, got: %v", got)
}

func TestClassify_OtherErrors_AreCanceled(t *testing.T) {
	got := classify(errors.New("document rejected: unsupported format"))
	require.Error(t, got)

	var cancelErr *river.JobCancelError
	require.True(t, errors.As(got, &cancelErr), "a document-level verdict should cancel the job (no retry), got: %v", got)
}
