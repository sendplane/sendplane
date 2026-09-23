package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/sendplane/sendplane/host"
	"github.com/sendplane/sendplane/internal/control"
	"github.com/sendplane/sendplane/internal/ingest"
	"github.com/sendplane/sendplane/internal/platform"
	"github.com/sendplane/sendplane/internal/render"
	"github.com/sendplane/sendplane/store"
)

// apiError is the internal error the handlers raise. It carries the HTTP
// status and the Error body of the spec; everything a handler returns as an
// error is reduced to one of these by errorFor.
type apiError struct {
	status  int
	code    ErrorCode
	message string
	details []ErrorDetail
	cause   error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

func (e *apiError) Unwrap() error { return e.cause }

// body renders the wire representation.
func (e *apiError) body() Error {
	out := Error{Code: e.code, Message: e.message}
	if len(e.details) > 0 {
		d := e.details
		out.Details = &d
	}
	return out
}

func newErr(status int, code ErrorCode, format string, args ...any) *apiError {
	return &apiError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// The shorthands the handlers use. They exist so that a handler reads as
// "this is a 422 because the locale is malformed", not as a struct literal.
func errNotFound(kind, id string) *apiError {
	return newErr(http.StatusNotFound, ErrorCodeNotFound, "%s %s not found", kind, id)
}

func errInvalid(format string, args ...any) *apiError {
	return newErr(http.StatusUnprocessableEntity, ErrorCodeValidationFailed, format, args...)
}

func errBadRequest(format string, args ...any) *apiError {
	return newErr(http.StatusBadRequest, ErrorCodeInvalidRequest, format, args...)
}

func errConflict(code ErrorCode, format string, args ...any) *apiError {
	return newErr(http.StatusConflict, code, format, args...)
}

// errorFor maps anything a handler or a package it calls returned onto the
// status/code table of api/openapi.yaml. It is the single place that knows
// which sentinel means what, so a handler can just return the error it got.
func errorFor(err error) *apiError {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}

	switch {
	// host boundary.
	case errors.Is(err, host.ErrUnauthenticated):
		return &apiError{status: http.StatusUnauthorized, code: ErrorCodeUnauthenticated,
			message: "unauthenticated", cause: err}
	case errors.Is(err, host.ErrForbidden):
		return &apiError{status: http.StatusForbidden, code: ErrorCodeForbidden,
			message: "forbidden", cause: err}

	// store sentinels.
	case errors.Is(err, store.ErrReadOnly):
		// A platform resource is defined in the operator's configuration, so
		// there is nothing an API caller could change about it (ADR-0017).
		return &apiError{status: http.StatusForbidden, code: ErrorCodePlatformReadOnly,
			message: err.Error(), cause: err}
	case errors.Is(err, store.ErrNotFound):
		return &apiError{status: http.StatusNotFound, code: ErrorCodeNotFound,
			message: "not found", cause: err}
	case errors.Is(err, store.ErrConflict):
		return &apiError{status: http.StatusConflict, code: ErrorCodeVersionConflict,
			message: "the object changed since it was read", cause: err}
	case errors.Is(err, store.ErrInvalid):
		return &apiError{status: http.StatusUnprocessableEntity, code: ErrorCodeValidationFailed,
			message: err.Error(), cause: err}

	// ingest.
	case errors.Is(err, ingest.ErrTooManyRecipients):
		return &apiError{status: http.StatusRequestEntityTooLarge, code: ErrorCodeLimitExceeded,
			message: err.Error(), cause: err}
	case errors.Is(err, ingest.ErrCampaignNotEditable):
		return &apiError{status: http.StatusConflict, code: ErrorCodeInvalidState,
			message: err.Error(), cause: err}

	// campaign state machine.
	case errors.Is(err, control.ErrInvalidTransition):
		return &apiError{status: http.StatusConflict, code: ErrorCodeInvalidState,
			message: err.Error(), cause: err}
	case errors.Is(err, control.ErrNoRecipients),
		errors.Is(err, control.ErrNoSender),
		errors.Is(err, control.ErrNoVersion):
		return &apiError{status: http.StatusUnprocessableEntity, code: ErrorCodePreconditionFailed,
			message: err.Error(), cause: err}

	// sender-use policy and shared sender From templates (ADR-0017).
	case errors.Is(err, host.ErrSenderUseDenied):
		return &apiError{status: http.StatusForbidden, code: ErrorCodeSenderUseDenied,
			message: err.Error(), cause: err}
	case errors.Is(err, host.ErrTemplateUseDenied):
		return &apiError{status: http.StatusForbidden, code: ErrorCodeTemplateUseDenied,
			message: err.Error(), cause: err}
	case errors.Is(err, platform.ErrMissingVars), errors.Is(err, platform.ErrInvalidFrom):
		return &apiError{status: http.StatusUnprocessableEntity, code: ErrorCodeTenantVarsMissing,
			message: err.Error(), cause: err}

	// render / publish.
	case errors.Is(err, render.ErrMissingKeys):
		return &apiError{status: http.StatusUnprocessableEntity, code: ErrorCodeMissingI18nKeys,
			message: err.Error(), cause: err}
	case errors.Is(err, render.ErrInvalidBundle):
		return &apiError{status: http.StatusUnprocessableEntity, code: ErrorCodeValidationFailed,
			message: err.Error(), cause: err}
	case errors.Is(err, render.ErrNoContentSlot),
		errors.Is(err, render.ErrModeMismatch),
		errors.Is(err, render.ErrUnknownMode),
		errors.Is(err, render.ErrHeaderInjection),
		errors.Is(err, render.ErrOutputTooLarge),
		errors.Is(err, render.ErrNoTemplate),
		errors.Is(err, render.ErrNoVersion):
		return &apiError{status: http.StatusUnprocessableEntity, code: ErrorCodeRenderFailed,
			message: err.Error(), cause: err}

	// transport-level body caps.
	case isMaxBytes(err):
		return &apiError{status: http.StatusRequestEntityTooLarge, code: ErrorCodePayloadTooLarge,
			message: "request body exceeds the configured limit", cause: err}
	}

	return &apiError{status: http.StatusInternalServerError, code: ErrorCodeInternal,
		message: "internal error", cause: err}
}

// bindError is what the generated server reports when a request could not be
// turned into its request object: a path parameter that is not a UUID, a
// missing required query parameter, a body that is not the declared media
// type. All of them are 400 invalid_request, except a body that tripped the
// size cap, which is a 413.
func bindError(err error) *apiError {
	if isMaxBytes(err) {
		return errorFor(err)
	}
	var ae *apiError
	if errors.As(err, &ae) {
		return ae
	}
	return &apiError{status: http.StatusBadRequest, code: ErrorCodeInvalidRequest,
		message: err.Error(), cause: err}
}

func isMaxBytes(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// writeError renders an apiError as the spec's Error body. It is used by the
// middleware and by the request/response error hooks of the generated strict
// handler; handlers themselves return typed responses instead.
func writeError(w http.ResponseWriter, e *apiError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.status)
	_ = json.NewEncoder(w).Encode(e.body())
}
