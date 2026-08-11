package scmcore

import (
	"context"
	"errors"

	"github.com/sortie-ai/sortie/internal/domain"
)

// trackerToCIKind maps a tracker-layer error category onto the matching CI
// category. A kind absent from this table falls back to
// [domain.ErrCIAPI] in [ToCIError].
var trackerToCIKind = map[domain.TrackerErrorKind]domain.CIErrorKind{
	domain.ErrTrackerTransport:      domain.ErrCITransport,
	domain.ErrTrackerAuth:           domain.ErrCIAuth,
	domain.ErrMissingTrackerAPIKey:  domain.ErrCIAuth,
	domain.ErrTrackerAPI:            domain.ErrCIAPI,
	domain.ErrTrackerNotFound:       domain.ErrCINotFound,
	domain.ErrTrackerPayload:        domain.ErrCIPayload,
	domain.ErrMissingTrackerProject: domain.ErrCIPayload,
}

// ToCIError maps an error raised by a forge adapter's transport to the
// CI-feedback error type.
//
// A nil err returns nil. An error matching [context.Canceled] or
// [context.DeadlineExceeded] returns unchanged, so a caller can still
// match it with [errors.Is]. An error already unwrapping to
// [*domain.CIError] returns unchanged, preserving a page decoder's own
// constructed kind. An error unwrapping to [*domain.TrackerError] converts
// to a [*domain.CIError] carrying the tracker error's message and the kind
// from [trackerToCIKind], defaulting to [domain.ErrCIAPI] for an
// unrecognized kind. Any other error becomes [domain.ErrCIAPI].
func ToCIError(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}

	var ciErr *domain.CIError
	if errors.As(err, &ciErr) {
		return err
	}

	var te *domain.TrackerError
	if errors.As(err, &te) {
		kind, ok := trackerToCIKind[te.Kind]
		if !ok {
			kind = domain.ErrCIAPI
		}
		return &domain.CIError{
			Kind:    kind,
			Message: te.Message,
			Err:     err,
		}
	}

	return &domain.CIError{
		Kind:    domain.ErrCIAPI,
		Message: err.Error(),
		Err:     err,
	}
}
