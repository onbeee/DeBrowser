package logging

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/onbeee/DeBrowser/internal/store"
)

// Unified event model (unified automatic recording). Every
// structured record the application writes about its own operation
// — user operations and their backend steps, errors with the exact
// message shown to the user, and runtime/lifecycle facts — is an
// Event in this shape, appended as one JSON line to one of the
// three category streams. The categories are fixed:
//
//	operation — user-initiated operations and their steps/results
//	error     — error records with full detail
//	runtime   — application lifecycle, page and module runtime facts
//	            (stored in the historical "run" stream files)
//
// "All records" is an aggregate view over the three streams; no
// event is ever copied into a second stream. Legacy lines (slog
// records, Stage 5B audit records) coexist in the same files and
// remain valid members of their stream for ranges and exports.
const (
	EventCategoryOperation = "operation"
	EventCategoryError     = "error"
	EventCategoryRuntime   = "runtime"
)

// Event types written by the recorder.
const (
	EventTypeOperationStart  = "operation_start"
	EventTypeOperationStep   = "operation_step"
	EventTypeOperationResult = "operation_result"
	EventTypeError           = "error"
	EventTypeRuntime         = "runtime"
	EventTypeLifecycle       = "lifecycle"
	EventTypeClient          = "client"
)

// Event statuses.
const (
	StatusStarted   = "started"
	StatusSuccess   = "success"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusInfo      = "info"
)

// Event is one unified record. Fields are emitted in a stable
// order; empty optional fields are omitted.
type Event struct {
	EventID       string            `json:"event_id"`
	Timestamp     string            `json:"timestamp"`
	EventType     string            `json:"event_type"`
	Category      string            `json:"category"`
	Module        string            `json:"module"`
	Action        string            `json:"action"`
	TargetType    string            `json:"target_type,omitempty"`
	TargetID      string            `json:"target_id,omitempty"`
	OperationID   string            `json:"operation_id,omitempty"`
	ParentEventID string            `json:"parent_event_id,omitempty"`
	Trigger       string            `json:"trigger"`
	Status        string            `json:"status,omitempty"`
	DurationMs    int64             `json:"duration_ms"` // always present; meaningful on terminal events, 0 elsewhere
	ErrorCode     string            `json:"error_code,omitempty"`
	ErrorCategory string            `json:"error_category,omitempty"`
	ErrorDetail   string            `json:"error_detail,omitempty"`
	UserMessage   string            `json:"user_message,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
}

// newEventID returns a random UUID for an event or operation. The
// store generator only fails when the system random source does,
// in which case a timestamp-based ID keeps recording functional.
func newEventID() string {
	if id, err := store.NewID(); err == nil {
		return id
	}
	return fmt.Sprintf("id-%d", time.Now().UnixNano())
}

// streamFor maps an event category onto the physical stream. The
// runtime category lives in the historical "run" files so existing
// installations keep one continuous history.
func streamFor(category string) (Category, bool) {
	switch category {
	case EventCategoryOperation:
		return CategoryOperation, true
	case EventCategoryError:
		return CategoryError, true
	case EventCategoryRuntime:
		return CategoryRun, true
	}
	return "", false
}

// StreamCategoryName maps a physical stream back to the unified
// category name used in views and exports.
func StreamCategoryName(c Category) string {
	if c == CategoryRun {
		return EventCategoryRuntime
	}
	return string(c)
}

var sensitiveMetaKeyParts = []string{
	"password", "passwd", "pwd", "secret", "token", "cookie",
	"credential", "auth", "api_key", "apikey", "vault", "url",
}

// sanitizeEvent makes an event safe to persist: free-text fields
// pass through the same scrubber as slog error values, metadata
// entries with credential-shaped keys are dropped and the rest are
// scrubbed, and free-text lengths are capped. Event content must
// never carry credentials, tokens, cookies, profile content or
// full authentication material.
func sanitizeEvent(e *Event) {
	e.ErrorDetail = ScrubErrorText(e.ErrorDetail)
	e.UserMessage = ScrubErrorText(e.UserMessage)
	if len(e.Metadata) == 0 {
		e.Metadata = nil
		return
	}
	clean := make(map[string]string, len(e.Metadata))
	for k, v := range e.Metadata {
		lk := strings.ToLower(k)
		drop := false
		for _, part := range sensitiveMetaKeyParts {
			if strings.Contains(lk, part) {
				drop = true
				break
			}
		}
		if drop {
			continue
		}
		clean[k] = ScrubErrorText(v)
	}
	if len(clean) == 0 {
		e.Metadata = nil
	} else {
		e.Metadata = clean
	}
}

// RecordEvent appends one event to its category stream. It never
// returns an error and never panics: a logging failure must not
// fail the business operation it describes. Failures are counted
// (see WriteErrors) and surface in diagnostics instead of being
// silently ignored or recursively logged.
func (s *System) RecordEvent(e Event) {
	if s == nil {
		return
	}
	stream, ok := streamFor(e.Category)
	if !ok {
		atomic.AddInt64(&s.writeErrors, 1)
		return
	}
	if e.EventID == "" {
		e.EventID = newEventID()
	}
	if e.Timestamp == "" {
		e.Timestamp = time.Now().Format(time.RFC3339Nano)
	}
	if e.Trigger == "" {
		e.Trigger = "system"
	}
	sanitizeEvent(&e)
	line, err := json.Marshal(e)
	if err != nil {
		atomic.AddInt64(&s.writeErrors, 1)
		return
	}
	w := s.writers[stream]
	if w == nil {
		atomic.AddInt64(&s.writeErrors, 1)
		return
	}
	if _, err := w.Write(append(line, '\n')); err != nil {
		atomic.AddInt64(&s.writeErrors, 1)
	}
}

// WriteErrors returns how many structured-event writes have failed
// since startup (degradation signal for diagnostics).
func (s *System) WriteErrors() int64 {
	if s == nil {
		return 0
	}
	return atomic.LoadInt64(&s.writeErrors)
}

// RecordRuntime appends a runtime-category fact (lifecycle, page,
// module). module/action are short stable identifiers.
func (s *System) RecordRuntime(module, action, status string, metadata map[string]string) {
	s.RecordEvent(Event{
		EventType: EventTypeRuntime,
		Category:  EventCategoryRuntime,
		Module:    module,
		Action:    action,
		Trigger:   "system",
		Status:    status,
		Metadata:  metadata,
	})
}

// RecordLifecycle appends an application-lifecycle runtime event.
func (s *System) RecordLifecycle(action, status string, metadata map[string]string) {
	s.RecordEvent(Event{
		EventType: EventTypeLifecycle,
		Category:  EventCategoryRuntime,
		Module:    "应用",
		Action:    action,
		Trigger:   "system",
		Status:    status,
		Metadata:  metadata,
	})
}

// Operation is one in-flight user operation being recorded. Its
// root event is written by BeginOperation; steps and the terminal
// result share the operation ID, and steps point at the root event
// through ParentEventID, so the full chain
//
//	user action → frontend request → backend steps → result →
//	message shown to the user
//
// can be reconstructed from the streams. All methods are safe on a
// nil *Operation (recording disabled) and safe for concurrent use.
type Operation struct {
	sys       *System
	id        string
	rootID    string
	module    string
	action    string
	startedAt time.Time
	lastStep  atomic.Value // string: event ID of the most recent step
}

// ID returns the operation's correlation ID.
func (o *Operation) ID() string {
	if o == nil {
		return ""
	}
	return o.id
}

// RootEventID returns the event ID of the operation's root event.
func (o *Operation) RootEventID() string {
	if o == nil {
		return ""
	}
	return o.rootID
}

// BeginOperation starts recording a user operation and writes its
// root event. When operationID is empty the backend generates one;
// the frontend passes the ID it generated for the user action so
// the whole chain shares one ID. trigger is "user" for UI-initiated
// operations.
func (s *System) BeginOperation(operationID, module, action, targetType, targetID, trigger string) *Operation {
	if s == nil {
		return nil
	}
	if operationID == "" {
		operationID = newEventID()
	}
	if trigger == "" {
		trigger = "user"
	}
	root := Event{
		EventID:     newEventID(),
		EventType:   EventTypeOperationStart,
		Category:    EventCategoryOperation,
		Module:      module,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		OperationID: operationID,
		Trigger:     trigger,
		Status:      StatusStarted,
	}
	s.RecordEvent(root)
	return &Operation{
		sys:       s,
		id:        operationID,
		rootID:    root.EventID,
		module:    module,
		action:    action,
		startedAt: time.Now(),
	}
}

// Step records one backend step of the operation (for example
// "读取文件", "写入数据库"). status is StatusSuccess/StatusFailed/
// StatusInfo; a failed step also produces an error-category event
// when Fail is used for the terminal failure — intermediate step
// failures that the operation recovers from stay operation-only.
func (o *Operation) Step(action, status string, metadata map[string]string) {
	if o == nil || o.sys == nil {
		return
	}
	ev := Event{
		EventID:       newEventID(),
		EventType:     EventTypeOperationStep,
		Category:      EventCategoryOperation,
		Module:        o.module,
		Action:        action,
		OperationID:   o.id,
		ParentEventID: o.rootID,
		Trigger:       "system",
		Status:        status,
		Metadata:      metadata,
	}
	o.sys.RecordEvent(ev)
	o.lastStep.Store(ev.EventID)
}

// parentForTerminal returns the most specific parent for terminal
// events: the last recorded step when there is one, else the root.
func (o *Operation) parentForTerminal() string {
	if v := o.lastStep.Load(); v != nil {
		if id, ok := v.(string); ok && id != "" {
			return id
		}
	}
	return o.rootID
}

// Succeed records the operation's successful terminal event.
func (o *Operation) Succeed(metadata map[string]string) {
	if o == nil || o.sys == nil {
		return
	}
	o.sys.RecordEvent(Event{
		EventType:     EventTypeOperationResult,
		Category:      EventCategoryOperation,
		Module:        o.module,
		Action:        o.action,
		OperationID:   o.id,
		ParentEventID: o.parentForTerminal(),
		Trigger:       "system",
		Status:        StatusSuccess,
		DurationMs:    time.Since(o.startedAt).Milliseconds(),
		Metadata:      metadata,
	})
}

// Cancelled records the operation's cancelled terminal event.
func (o *Operation) Cancelled(reason string) {
	if o == nil || o.sys == nil {
		return
	}
	md := map[string]string{}
	if reason != "" {
		md["reason"] = reason
	}
	o.sys.RecordEvent(Event{
		EventType:     EventTypeOperationResult,
		Category:      EventCategoryOperation,
		Module:        o.module,
		Action:        o.action,
		OperationID:   o.id,
		ParentEventID: o.parentForTerminal(),
		Trigger:       "system",
		Status:        StatusCancelled,
		DurationMs:    time.Since(o.startedAt).Milliseconds(),
		Metadata:      md,
	})
}

// Fail records the operation's failed terminal event in the
// operation stream AND the full error record in the error stream:
// error code, category, detail, the exact message shown to the
// user and the elapsed time are all preserved, linked by the
// operation ID.
func (o *Operation) Fail(err error, errorCode, errorCategory, userMessage string) {
	if o == nil || o.sys == nil {
		return
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if userMessage == "" {
		userMessage = detail
	}
	dur := time.Since(o.startedAt).Milliseconds()
	o.sys.RecordEvent(Event{
		EventType:     EventTypeOperationResult,
		Category:      EventCategoryOperation,
		Module:        o.module,
		Action:        o.action,
		OperationID:   o.id,
		ParentEventID: o.parentForTerminal(),
		Trigger:       "system",
		Status:        StatusFailed,
		DurationMs:    dur,
		ErrorCode:     errorCode,
		ErrorCategory: errorCategory,
		UserMessage:   userMessage,
	})
	o.sys.RecordEvent(Event{
		EventType:     EventTypeError,
		Category:      EventCategoryError,
		Module:        o.module,
		Action:        o.action,
		OperationID:   o.id,
		ParentEventID: o.parentForTerminal(),
		Trigger:       "system",
		Status:        StatusFailed,
		DurationMs:    dur,
		ErrorCode:     errorCode,
		ErrorCategory: errorCategory,
		ErrorDetail:   detail,
		UserMessage:   userMessage,
	})
}

// RecordError records a standalone error (not tied to a user
// operation): module, action, code, category, detail and the
// message shown to the user.
func (s *System) RecordError(module, action string, err error, errorCode, errorCategory, userMessage string) {
	if s == nil {
		return
	}
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	if userMessage == "" {
		userMessage = detail
	}
	s.RecordEvent(Event{
		EventType:     EventTypeError,
		Category:      EventCategoryError,
		Module:        module,
		Action:        action,
		Trigger:       "system",
		Status:        StatusFailed,
		ErrorCode:     errorCode,
		ErrorCategory: errorCategory,
		ErrorDetail:   detail,
		UserMessage:   userMessage,
	})
}
