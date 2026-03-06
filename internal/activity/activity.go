package activity

import (
	"context"
	"fmt"
	"time"
)

// RetryPolicy defines how an activity should be retried on failure.
type RetryPolicy struct {
	MaxAttempts     int
	InitialInterval time.Duration
	MaxInterval     time.Duration
	BackoffFactor   float64
}

// DefaultRetryPolicy returns sensible retry defaults.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts:     3,
		InitialInterval: 5 * time.Second,
		MaxInterval:     2 * time.Minute,
		BackoffFactor:   2.0,
	}
}

// Activity defines the interface for all schedulable activities.
type Activity interface {
	Name() string
	Execute(ctx context.Context, input []byte) ([]byte, error)
	Retry() RetryPolicy
	Timeout() time.Duration
}

// Registry holds all registered activities keyed by name.
type Registry struct {
	activities map[string]Activity
}

// NewRegistry creates a new activity registry.
func NewRegistry() *Registry {
	return &Registry{
		activities: make(map[string]Activity),
	}
}

// Register adds an activity to the registry.
func (r *Registry) Register(a Activity) {
	r.activities[a.Name()] = a
}

// Get returns the activity for the given name.
func (r *Registry) Get(name string) (Activity, error) {
	a, ok := r.activities[name]
	if !ok {
		return nil, fmt.Errorf("activity: unknown activity %q", name)
	}
	return a, nil
}

// All returns all registered activity names.
func (r *Registry) All() []string {
	names := make([]string, 0, len(r.activities))
	for name := range r.activities {
		names = append(names, name)
	}
	return names
}
