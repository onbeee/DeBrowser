package service

import (
	"errors"

	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Operation recording (unified recording system): every
// user-initiated business operation is recorded through the
// logging system's unified event API — a root event when the
// operation starts, step events for its backend phases, and a
// terminal event with the outcome; failures additionally write a
// full error record (code, category, detail, and the exact
// message returned to — and displayed by — the frontend).
//
// The frontend generates the operation ID for each user action
// and passes it as the first argument of the binding call, so the
// frontend request, the backend execution, its steps and its
// result all share one correlation ID. An empty ID means the
// backend generates one (internal calls, tests).
//
// The recording system is optional: services work unchanged (and
// unrecorded) when none is attached, which keeps unit tests and
// tools simple. main attaches the real logging system at startup
// through each service's Auditor field (an exported field, not a
// method, on purpose: Wails turns exported methods into frontend
// bindings, and the wiring is startup plumbing, not a UI API).

// beginOp starts recording an operation; safe when sys is nil.
func beginOp(sys *logging.System, operationID, module, action, targetType, targetID string) *logging.Operation {
	if sys == nil {
		return nil
	}
	return sys.BeginOperation(operationID, module, action, targetType, targetID, "user")
}

// finishOp records the terminal event for an operation started
// with beginOp. On failure the user message is the exact error
// text returned to the frontend.
func finishOp(op *logging.Operation, err error) {
	if op == nil {
		return
	}
	if err == nil {
		op.Succeed(nil)
		return
	}
	op.Fail(err, "operation_failed", errorCategoryFor(err), err.Error())
}

// errorCategoryFor maps an operation error onto the small, safe
// classification vocabulary used in error records.
func errorCategoryFor(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrNotFound), errors.Is(err, secrets.ErrNotFound):
		return "not_found"
	case errors.Is(err, secrets.ErrCorrupt), errors.Is(err, secrets.ErrCorruptVault):
		return "credential_unavailable"
	default:
		return "failed"
	}
}
