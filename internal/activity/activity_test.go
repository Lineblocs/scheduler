package activity

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type stubActivity struct {
	name string
}

func (s *stubActivity) Name() string { return s.name }
func (s *stubActivity) Execute(_ context.Context, _ []byte) ([]byte, error) {
	return []byte("done"), nil
}
func (s *stubActivity) Retry() RetryPolicy  { return DefaultRetryPolicy() }
func (s *stubActivity) Timeout() time.Duration { return time.Minute }

func TestRegistry_RegisterAndGet(t *testing.T) {
	reg := NewRegistry()
	act := &stubActivity{name: "test.activity"}

	reg.Register(act)

	got, err := reg.Get("test.activity")
	require.NoError(t, err)
	assert.Equal(t, "test.activity", got.Name())
}

func TestRegistry_GetUnknown(t *testing.T) {
	reg := NewRegistry()

	_, err := reg.Get("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown activity")
}

func TestRegistry_All(t *testing.T) {
	reg := NewRegistry()
	reg.Register(&stubActivity{name: "a"})
	reg.Register(&stubActivity{name: "b"})

	all := reg.All()
	assert.Len(t, all, 2)
	assert.Contains(t, all, "a")
	assert.Contains(t, all, "b")
}

func TestDefaultRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()
	assert.Equal(t, 3, p.MaxAttempts)
	assert.Equal(t, 5*time.Second, p.InitialInterval)
	assert.Equal(t, 2*time.Minute, p.MaxInterval)
	assert.Equal(t, 2.0, p.BackoffFactor)
}
