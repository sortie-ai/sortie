package scmcore

import (
	"errors"

	"github.com/sortie-ai/sortie/internal/domain"
)

// alreadyMergedMarker is the phrase MergePR callers dispatch on to treat
// a rejected merge as an idempotent success. It MUST stay lower case, since
// the reconcile pass compares it case-insensitively against a lower-cased
// message.
const alreadyMergedMarker = "already merged"

var trackerToSCMKind = map[domain.TrackerErrorKind]domain.SCMErrorKind{
	domain.ErrTrackerTransport: domain.ErrSCMTransport,
	domain.ErrTrackerAuth:      domain.ErrSCMAuth,
	domain.ErrTrackerAPI:       domain.ErrSCMAPI,
	domain.ErrTrackerNotFound:  domain.ErrSCMNotFound,
	domain.ErrTrackerPayload:   domain.ErrSCMPayload,
}

// ToSCMError maps a tracker-layer error to its SCM equivalent. A
// [*domain.TrackerError] is mapped by its Kind onto the matching SCM kind,
// carrying its message and preserving the unwrap chain; an unrecognized
// kind maps to [domain.ErrSCMAPI]. Any other error is wrapped as
// [domain.ErrSCMTransport].
//
// ToSCMError performs no conflict promotion. A caller on a merge write
// path applies [AsMergeConflict] to the result.
//
// A caller holding an error returned directly by an [httpkit.Client] call
// uses ToSCMError. A caller holding an error from a paginator walk, whose
// decoder may already have constructed a [*domain.SCMError], uses
// [AsSCMError] instead.
func ToSCMError(err error) *domain.SCMError {
	var te *domain.TrackerError
	if !errors.As(err, &te) {
		return &domain.SCMError{
			Kind:    domain.ErrSCMTransport,
			Message: err.Error(),
			Err:     err,
		}
	}

	scmKind, ok := trackerToSCMKind[te.Kind]
	if !ok {
		scmKind = domain.ErrSCMAPI
	}

	return &domain.SCMError{
		Kind:    scmKind,
		Message: te.Message,
		Err:     err,
	}
}

// AsSCMError returns err's chain as a [*domain.SCMError]. When err already
// unwraps to a [*domain.SCMError], AsSCMError returns that value unwrapped,
// discarding the outer wrapper. When err instead unwraps to a
// [*domain.TrackerError], or to neither, AsSCMError delegates to
// [ToSCMError].
//
// err must be non-nil; AsSCMError has no nil branch. A caller holding an
// error from an httpkit paginator walk, whose decoder may already have
// constructed a [*domain.SCMError], uses AsSCMError. A caller holding an
// error returned directly by an [httpkit.Client] call uses [ToSCMError]
// instead.
func AsSCMError(err error) *domain.SCMError {
	var scmErr *domain.SCMError
	if errors.As(err, &scmErr) {
		return scmErr
	}
	return ToSCMError(err)
}

// AsMergeConflict promotes err to [domain.ErrSCMConflict] when, and only
// when, the originating [domain.TrackerError.Status] is 405 (method not
// allowed) or 409 (conflict), and returns err unchanged otherwise.
//
// Only a merge write path may call AsMergeConflict: no read path,
// RemoveLabel, or DeleteBranch may produce ErrSCMConflict.
func AsMergeConflict(err *domain.SCMError) *domain.SCMError {
	if err == nil {
		return nil
	}

	var te *domain.TrackerError
	if !errors.As(err.Err, &te) {
		return err
	}
	if te.Status != 405 && te.Status != 409 {
		return err
	}

	return &domain.SCMError{
		Kind:    domain.ErrSCMConflict,
		Message: err.Message,
		Err:     err.Err,
	}
}

// AlreadyMergedConflict returns the [domain.ErrSCMConflict] an adapter
// reports when it has confirmed, by re-reading the pull request, that the
// pull request was already merged. Message carries the lower-case marker
// phrase the auto-merge reconcile pass dispatches on.
func AlreadyMergedConflict(detail string, cause error) *domain.SCMError {
	return &domain.SCMError{
		Kind:    domain.ErrSCMConflict,
		Message: alreadyMergedMarker + ": " + detail,
		Err:     cause,
	}
}
