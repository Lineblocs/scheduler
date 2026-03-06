package activity

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	apperrors "lineblocs.com/scheduler/internal/errors"
)

type mockActivity struct {
	name     string
	calls    int
	failN    int
	nonRetry bool
}

func (m *mockActivity) Name() string { return m.name }

func (m *mockActivity) Execute(_ context.Context, _ []byte) ([]byte, error) {
	m.calls++
	if m.calls <= m.failN {
		if m.nonRetry {
			return nil, apperrors.NewNonRetryable(errors.New("non-retryable error"))
		}
		return nil, errors.New("transient error")
	}
	return []byte("ok"), nil
}

func (m *mockActivity) Retry() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     3,
		InitialInterval: 1 * time.Millisecond,
		MaxInterval:     10 * time.Millisecond,
		BackoffFactor:   2.0,
	}
}

func (m *mockActivity) Timeout() time.Duration { return 1 * time.Second }

func TestExecuteWithRetry_Success(t *testing.T) {
	a := &mockActivity{name: "test", failN: 0}
	log, _ := zap.NewDevelopment()

	out, err := ExecuteWithRetry(context.Background(), a, nil, log)
	assert.NoError(t, err)
	assert.Equal(t, []byte("ok"), out)
	assert.Equal(t, 1, a.calls)
}

func TestExecuteWithRetry_RetryThenSuccess(t *testing.T) {
	a := &mockActivity{name: "test", failN: 2}
	log, _ := zap.NewDevelopment()

	out, err := ExecuteWithRetry(context.Background(), a, nil, log)
	assert.NoError(t, err)
	assert.Equal(t, []byte("ok"), out)
	assert.Equal(t, 3, a.calls)
}

func TestExecuteWithRetry_MaxAttemptsExceeded(t *testing.T) {
	a := &mockActivity{name: "test", failN: 5}
	log, _ := zap.NewDevelopment()

	_, err := ExecuteWithRetry(context.Background(), a, nil, log)
	assert.Error(t, err)
	assert.Equal(t, 3, a.calls)
}

func TestExecuteWithRetry_NonRetryable(t *testing.T) {
	a := &mockActivity{name: "test", failN: 1, nonRetry: true}
	log, _ := zap.NewDevelopment()

	_, err := ExecuteWithRetry(context.Background(), a, nil, log)
	assert.Error(t, err)
	assert.Equal(t, 1, a.calls) // Should not retry
}

func TestExecuteWithRetry_ContextCanceled(t *testing.T) {
	a := &mockActivity{name: "test", failN: 5}
	log, _ := zap.NewDevelopment()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ExecuteWithRetry(ctx, a, nil, log)
	assert.Error(t, err)
}
