package errors

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsNonRetryable(t *testing.T) {
	t.Run("non-retryable error", func(t *testing.T) {
		err := NewNonRetryable(fmt.Errorf("plan not found"))
		assert.True(t, IsNonRetryable(err))
	})

	t.Run("regular error", func(t *testing.T) {
		err := fmt.Errorf("connection timeout")
		assert.False(t, IsNonRetryable(err))
	})

	t.Run("wrapped non-retryable", func(t *testing.T) {
		inner := NewNonRetryable(fmt.Errorf("bad input"))
		err := fmt.Errorf("outer: %w", inner)
		assert.True(t, IsNonRetryable(err))
	})
}

func TestNonRetryableError_Error(t *testing.T) {
	err := NewNonRetryable(ErrPlanNotFound)
	assert.Equal(t, "plan not found", err.Error())
}

func TestNonRetryableError_Unwrap(t *testing.T) {
	nre := &NonRetryableError{Err: ErrPlanNotFound}
	assert.Equal(t, ErrPlanNotFound, nre.Unwrap())
}
