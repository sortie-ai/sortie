package gitea

import (
	"errors"

	"github.com/sortie-ai/sortie/internal/domain"
)

// giteaToSCMError converts an error from the shared Gitea transport into a
// [*domain.SCMError] at the SCM boundary.
//
// A [*domain.TrackerError] is mapped by its Kind onto the matching SCM kind,
// carrying its message and preserving the chain; an unrecognized kind maps to
// [domain.ErrSCMAPI]. Any other error is wrapped as [domain.ErrSCMTransport].
func giteaToSCMError(err error) *domain.SCMError {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return &domain.SCMError{
			Kind:    domain.ErrSCMTransport,
			Message: err.Error(),
			Err:     err,
		}
	}

	kindMap := map[domain.TrackerErrorKind]domain.SCMErrorKind{
		domain.ErrTrackerTransport: domain.ErrSCMTransport,
		domain.ErrTrackerAuth:      domain.ErrSCMAuth,
		domain.ErrTrackerAPI:       domain.ErrSCMAPI,
		domain.ErrTrackerNotFound:  domain.ErrSCMNotFound,
		domain.ErrTrackerPayload:   domain.ErrSCMPayload,
	}

	scmKind, ok := kindMap[te.Kind]
	if !ok {
		scmKind = domain.ErrSCMAPI
	}

	return &domain.SCMError{
		Kind:    scmKind,
		Message: te.Message,
		Err:     err,
	}
}
