package activity

import (
	"context"
	"math"
	"time"

	"go.uber.org/zap"
	apperrors "lineblocs.com/scheduler/internal/errors"
)

// ExecuteWithRetry runs an activity with exponential backoff retry.
func ExecuteWithRetry(ctx context.Context, a Activity, input []byte, log *zap.Logger) ([]byte, error) {
	policy := a.Retry()
	var lastErr error

	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		actCtx, cancel := context.WithTimeout(ctx, a.Timeout())
		output, err := a.Execute(actCtx, input)
		cancel()

		if err == nil {
			return output, nil
		}

		lastErr = err

		if apperrors.IsNonRetryable(err) {
			log.Warn("non-retryable error, giving up",
				zap.String("activity", a.Name()),
				zap.Int("attempt", attempt),
				zap.Error(err))
			return nil, err
		}

		if attempt == policy.MaxAttempts {
			break
		}

		delay := time.Duration(float64(policy.InitialInterval) * math.Pow(policy.BackoffFactor, float64(attempt-1)))
		if delay > policy.MaxInterval {
			delay = policy.MaxInterval
		}

		log.Info("retrying activity",
			zap.String("activity", a.Name()),
			zap.Int("attempt", attempt),
			zap.Duration("delay", delay),
			zap.Error(err))

		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, lastErr
}
