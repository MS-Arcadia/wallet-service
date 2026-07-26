package abuse_test

import (
	"testing"
	"time"

	"github.com/MS-Arcadia/arcadia-platform/pkg/errs"
	"github.com/MS-Arcadia/wallet-service/internal/domain/abuse"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultPolicyMatchesTheRequirements(t *testing.T) {
	policy := abuse.DefaultPolicy()
	thresholds := policy.Thresholds()

	require.Len(t, thresholds, 2)
	assert.Equal(t, abuse.WindowMinute, thresholds[0].Name, "the tightest window must be evaluated first")
	assert.EqualValues(t, 5, thresholds[0].Limit)
	assert.Equal(t, time.Minute, thresholds[0].Window)

	assert.Equal(t, abuse.WindowHour, thresholds[1].Name)
	assert.EqualValues(t, 30, thresholds[1].Limit)
	assert.Equal(t, time.Hour, thresholds[1].Window)

	assert.EqualValues(t, 10, policy.FlagAt())
	assert.Less(t, policy.FlagAt(), thresholds[1].Limit,
		"Support must hear about a pattern before the user is fully locked out")
}

func TestNewPolicyValidation(t *testing.T) {
	_, err := abuse.NewPolicy(0, 30, 10)
	assert.Error(t, err)

	_, err = abuse.NewPolicy(5, 0, 10)
	assert.Error(t, err)

	_, err = abuse.NewPolicy(5, 3, 10)
	assert.Error(t, err, "an hourly limit below the per-minute limit is incoherent")

	_, err = abuse.NewPolicy(5, 30, 0)
	assert.Error(t, err)

	policy, err := abuse.NewPolicy(3, 20, 8)
	require.NoError(t, err)
	assert.EqualValues(t, 3, policy.Thresholds()[0].Limit)
	assert.EqualValues(t, 20, policy.Thresholds()[1].Limit)
	assert.EqualValues(t, 8, policy.FlagAt())
}

func TestAssessAllowsNormalBehaviour(t *testing.T) {
	policy := abuse.DefaultPolicy()

	// A user who mistyped a code twice is not an attacker.
	assessment := policy.Assess(false, "", 0, 2)
	assert.False(t, assessment.Blocked)
	assert.False(t, assessment.Flagged)
	assert.EqualValues(t, 2, assessment.FailedAttempts)
}

func TestAssessBlocksWhenTheLimiterSaysSo(t *testing.T) {
	policy := abuse.DefaultPolicy()

	assessment := policy.Assess(true, abuse.WindowMinute, 42*time.Second, 6)
	assert.True(t, assessment.Blocked)
	assert.Equal(t, abuse.WindowMinute, assessment.BlockedBy)
	assert.Equal(t, 42*time.Second, assessment.RetryAfter)
	assert.False(t, assessment.Flagged, "six failures is a burst, not yet a pattern worth Support's time")
}

func TestAssessFlagsAtTheReviewThreshold(t *testing.T) {
	policy := abuse.DefaultPolicy()

	assert.False(t, policy.Assess(false, "", 0, 9).Flagged)
	assert.True(t, policy.Assess(false, "", 0, 10).Flagged, "the threshold is inclusive")
	assert.True(t, policy.Assess(true, abuse.WindowHour, time.Minute, 31).Flagged)
}

func TestErrTooManyAttempts(t *testing.T) {
	err := abuse.ErrTooManyAttempts(90 * time.Second)

	assert.Equal(t, errs.CodeResourceExhausted, errs.CodeOf(err))
	assert.Equal(t, abuse.ReasonCodeTooManyAttempts, errs.ReasonOf(err))
	assert.False(t, errs.IsRetryable(err), "a throttled caller must not be retried automatically")

	// The message must not reveal which window tripped or how many guesses remain.
	assert.NotContains(t, err.Error(), "per-minute")
	assert.NotContains(t, err.Error(), "5")
	assert.Contains(t, err.Error(), "2 minutes")
}

func TestErrTooManyAttemptsHumanisesShortDelays(t *testing.T) {
	assert.Contains(t, abuse.ErrTooManyAttempts(30*time.Second).Error(), "30 seconds")
	assert.Contains(t, abuse.ErrTooManyAttempts(0).Error(), "1 seconds",
		"a zero delay must still read as a positive wait")
}
