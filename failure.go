package beesuh

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrchypark/beesuh/auth"
)

// Failure categories are a small, closed set. They are the durable half of the
// public error contract: an initial failure and its replay must report the same
// category, so callers and the HTTP layer can classify a stored result without
// parsing diagnostic text.
const (
	FailureInvalidInput          = "invalid_request"
	FailureNotFound              = "not_found"
	FailureRequestConflict       = "request_conflict"
	FailureUnauthorized          = "unauthenticated"
	FailureForbidden             = "forbidden"
	FailureConnectionUnavailable = "connection_unavailable"
	FailureRateLimited           = "rate_limited"
	FailureServiceUnavailable    = "service_unavailable"
	FailureOverloaded            = "overloaded"
	FailureDeadlineExceeded      = "deadline_exceeded"
	FailureUpstream              = "upstream_failure"
)

// publicMessages are the only failure strings a stored result may carry. The
// reviewed root cause stays in logs; the durable record keeps the safe message
// and the category.
var publicMessages = map[string]string{
	FailureInvalidInput:          "invalid request",
	FailureNotFound:              "requested resource was not found",
	FailureRequestConflict:       "request cannot be completed",
	FailureUnauthorized:          "authorization required",
	FailureForbidden:             "authorization required",
	FailureConnectionUnavailable: "connection is unavailable",
	FailureRateLimited:           "service is rate limited",
	FailureServiceUnavailable:    "service is unavailable",
	FailureOverloaded:            "service is overloaded",
	FailureDeadlineExceeded:      "request timed out",
	FailureUpstream:              "upstream service failed",
}

// failureCategoryFor classifies err into a bounded public category. It matches
// sentinels rather than inspecting error text.
func failureCategoryFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidInput):
		return FailureInvalidInput
	case errors.Is(err, ErrNotFound):
		return FailureNotFound
	case errors.Is(err, ErrConflict), errors.Is(err, ErrUncertain):
		return FailureRequestConflict
	case errors.Is(err, ErrUnauthenticated):
		return FailureUnauthorized
	case errors.Is(err, ErrForbidden):
		return FailureForbidden
	case errors.Is(err, ErrConnectionUnavailable):
		return FailureConnectionUnavailable
	case errors.Is(err, ErrOverloaded):
		return FailureOverloaded
	case errors.Is(err, auth.ErrRateLimited):
		return FailureRateLimited
	case errors.Is(err, auth.ErrUnavailable):
		return FailureServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return FailureDeadlineExceeded
	default:
		return FailureUpstream
	}
}

// FailureCategory returns the bounded public category for err. Transport layers
// use it so an initial failure and a replayed one report the same code.
func FailureCategory(err error) string { return failureCategoryFor(err) }

// PublicFailureMessage returns the safe message stored for a category.
func PublicFailureMessage(category string) string {
	if safe, ok := publicMessages[category]; ok {
		return safe
	}
	return publicMessages[FailureUpstream]
}

// setFailure records the public category and safe message on out. Diagnostic
// text is deliberately dropped before persistence.
func setFailure(out *Result, err error) {
	out.FailureCategory = failureCategoryFor(err)
	out.Error = publicMessages[out.FailureCategory]
}

// replayFailure reconstructs a stored failure so callers regain errors.Is and
// the HTTP layer regains the original category. A row written before
// categories existed has no category and falls back to the stored message.
func replayFailure(category, message string) error {
	if category == "" {
		if message == "" {
			return ErrUpstream
		}
		return errors.New(message)
	}
	safe, ok := publicMessages[category]
	if !ok {
		safe = publicMessages[FailureUpstream]
		category = FailureUpstream
	}
	// A single safe message joined to the sentinel, instead of a multi-error
	// join, so the public text a caller prints never mixes two messages.
	return fmt.Errorf("%s: %w", safe, categorySentinel(category))
}

// ErrUpstream is the generic replay fallback for a stored failure with no
// recognized category.
var ErrUpstream = errors.New("beesuh: upstream service failed")

func categorySentinel(category string) error {
	switch category {
	case FailureInvalidInput:
		return ErrInvalidInput
	case FailureNotFound:
		return ErrNotFound
	case FailureRequestConflict:
		return ErrConflict
	case FailureUnauthorized:
		return ErrUnauthenticated
	case FailureForbidden:
		return ErrForbidden
	case FailureConnectionUnavailable:
		return ErrConnectionUnavailable
	case FailureRateLimited:
		return auth.ErrRateLimited
	case FailureServiceUnavailable:
		return auth.ErrUnavailable
	case FailureOverloaded:
		return ErrOverloaded
	case FailureDeadlineExceeded:
		return context.DeadlineExceeded
	default:
		return ErrUpstream
	}
}
