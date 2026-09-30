// Package auth keeps provider credentials outside request and runtime state.
package auth

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrForbidden          = errors.New("auth: forbidden")
	ErrRateLimited        = errors.New("auth: rate limited")
	ErrUnavailable        = errors.New("auth: unavailable")
)

// HTTPError deliberately does not retain a response body: connector error
// bodies routinely contain provider-specific, sometimes sensitive details.
type HTTPError struct{ StatusCode int }

func (e HTTPError) Error() string {
	return fmt.Sprintf("auth: provider request failed with status %d", e.StatusCode)
}

func (e HTTPError) Unwrap() error {
	switch {
	case e.StatusCode == 401:
		return ErrInvalidCredentials
	case e.StatusCode == 403:
		return ErrForbidden
	case e.StatusCode == 429:
		return ErrRateLimited
	case e.StatusCode >= 500 && e.StatusCode <= 599:
		return ErrUnavailable
	default:
		return nil
	}
}

// Sanitize keeps only actionable, safe categories across the auth boundary.
func Sanitize(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	for _, known := range []error{ErrInvalidCredentials, ErrForbidden, ErrRateLimited, ErrUnavailable} {
		if errors.Is(err, known) {
			return known
		}
	}
	return ErrUnavailable
}
