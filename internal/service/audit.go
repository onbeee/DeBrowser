package service

import (
	"errors"
	"time"

	"github.com/onbeee/DeBrowser/internal/logging"
	"github.com/onbeee/DeBrowser/internal/secrets"
	"github.com/onbeee/DeBrowser/internal/store"
)

// Operation audit (Stage 5B): every user-initiated mutation of
// business data records exactly one entry in the operation log via
// the logging system's whitelist API. The record carries only the
// operation name, the target type and ID, the outcome, the duration
// and a coarse error class — never request data, credentials,
// configuration content or free-form user text.
//
// The auditor is optional: services work unchanged (and unaudited)
// when none is attached, which keeps unit tests and tools simple.
// main attaches the real logging system at startup.

// auditClass maps an operation error onto the small, safe
// classification vocabulary allowed in the operation log.
func auditClass(err error) string {
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

// auditOp emits one audit record when an auditor is attached.
func auditOp(auditor *logging.System, operation, targetType, targetID string, start time.Time, err error) {
	if auditor == nil {
		return
	}
	auditor.Audit(logging.AuditEvent{
		Operation:  operation,
		TargetType: targetType,
		TargetID:   targetID,
		OK:         err == nil,
		Duration:   time.Since(start),
		ErrClass:   auditClass(err),
	})
}

// The Auditor field on each service is an exported field (not a
// method) on purpose: Wails turns exported methods into frontend
// bindings, and the auditor wiring is startup plumbing, not a UI
// API. main assigns it once at startup; nil disables auditing.
