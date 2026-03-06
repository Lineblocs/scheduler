package errors

import "fmt"

// Domain error types.
var (
	ErrPlanNotFound      = fmt.Errorf("plan not found")
	ErrWorkspaceNotFound = fmt.Errorf("workspace not found")
	ErrUserNotFound      = fmt.Errorf("user not found")
	ErrSubNotFound       = fmt.Errorf("subscription not found")
	ErrInsufficientFunds = fmt.Errorf("insufficient funds")
	ErrPaymentFailed     = fmt.Errorf("payment failed")
	ErrInvoiceNotFound   = fmt.Errorf("invoice not found")
)

// NonRetryableError wraps errors that should not be retried.
type NonRetryableError struct {
	Err error
}

func (e *NonRetryableError) Error() string {
	return e.Err.Error()
}

func (e *NonRetryableError) Unwrap() error {
	return e.Err
}

// NewNonRetryable wraps an error as non-retryable.
func NewNonRetryable(err error) error {
	return &NonRetryableError{Err: err}
}

// IsNonRetryable returns true if the error should not be retried.
func IsNonRetryable(err error) bool {
	var nre *NonRetryableError
	for {
		if _, ok := err.(*NonRetryableError); ok {
			return true
		}
		unwrapped := unwrap(err)
		if unwrapped == nil {
			break
		}
		err = unwrapped
		_ = nre
	}
	return false
}

func unwrap(err error) error {
	u, ok := err.(interface{ Unwrap() error })
	if !ok {
		return nil
	}
	return u.Unwrap()
}
