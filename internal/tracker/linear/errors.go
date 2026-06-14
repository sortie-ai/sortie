package linear

import (
	"net/http"
	"strings"

	"github.com/sortie-ai/sortie/internal/domain"
)

// notFoundPrefix is the message prefix Linear uses for a missing entity. There
// is no dedicated not-found type or code, so the message is the only signal.
const notFoundPrefix = "entity not found"

// classifyGraphQLErrors maps a decoded GraphQL errors array to a
// [*domain.TrackerError]. It returns nil when errs is empty.
//
// The not-found-by-message check runs first, before any type-based rule,
// because a missing entity arrives under the generic "invalid input" type.
// Classification keys on extensions.type rather than the diagnostic
// extensions.code, with one exception: the rate-limit signal also accepts the
// documented extensions.code "RATELIMITED" because Linear reports it under the
// generic "invalid input" type. The returned message carries the first error's
// userPresentableMessage, falling back to its message.
func classifyGraphQLErrors(errs []graphQLError) error {
	if len(errs) == 0 {
		return nil
	}

	message := classifiedMessage(errs)

	for _, e := range errs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(e.Message)), notFoundPrefix) {
			return &domain.TrackerError{Kind: domain.ErrTrackerNotFound, Message: message}
		}
	}

	for _, e := range errs {
		if e.Extensions.Code == "RATELIMITED" || e.Extensions.Type == "ratelimited" {
			return &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: message}
		}
	}

	return &domain.TrackerError{Kind: kindFromType(errs), Message: message}
}

// kindFromType resolves the error kind from the first error's extensions.type
// and userError flag, scanning all errors for the type signal.
func kindFromType(errs []graphQLError) domain.TrackerErrorKind {
	for _, e := range errs {
		switch e.Extensions.Type {
		case "authentication error":
			return domain.ErrTrackerAuth
		case "forbidden", "feature not accessible":
			return domain.ErrTrackerAuth
		case "invalid input", "user error", "graphql error":
			return domain.ErrTrackerPayload
		case "internal error", "network error", "lock timeout", "bootstrap error":
			return domain.ErrTrackerTransport
		}
		if e.Extensions.UserError {
			return domain.ErrTrackerPayload
		}
	}
	return domain.ErrTrackerAPI
}

// classifiedMessage returns the first error's userPresentableMessage, falling
// back to its message, so operators see Linear's own wording.
func classifiedMessage(errs []graphQLError) string {
	first := errs[0]
	if first.Extensions.UserPresentableMessage != "" {
		return first.Extensions.UserPresentableMessage
	}
	return first.Message
}

// classifyHTTPStatus maps a non-200 HTTP status with no body errors array to a
// [*domain.TrackerError]. It is the fallback for responses that fail at the
// HTTP layer rather than carrying a GraphQL errors array.
func classifyHTTPStatus(status int, message string) error {
	switch status {
	case http.StatusBadRequest:
		return &domain.TrackerError{Kind: domain.ErrTrackerPayload, Message: message}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &domain.TrackerError{Kind: domain.ErrTrackerAuth, Message: message}
	case http.StatusTooManyRequests:
		return &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: message}
	default:
		if status >= 500 {
			return &domain.TrackerError{Kind: domain.ErrTrackerTransport, Message: message}
		}
		return &domain.TrackerError{Kind: domain.ErrTrackerAPI, Message: message}
	}
}
