// Package api holds HTTP request validation, error responses and handlers.
package api

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// Code is a machine-readable error code.
type Code string

// Top-level error codes.
const (
	CodeValidationFailed Code = "VALIDATION_FAILED"
	CodeInvalidJSON      Code = "INVALID_JSON"
	CodeEmptyBatch       Code = "EMPTY_BATCH"
	CodeBatchTooLarge    Code = "BATCH_TOO_LARGE"
	CodePayloadTooLarge  Code = "PAYLOAD_TOO_LARGE"
	CodeOverloaded       Code = "OVERLOADED"
)

// Field-level error codes, reported in Error.Details under VALIDATION_FAILED.
const (
	CodeMissingField        Code = "MISSING_FIELD"
	CodeInvalidField        Code = "INVALID_FIELD"
	CodeUnknownEventType    Code = "UNKNOWN_EVENT_TYPE"
	CodeUnknownAction       Code = "UNKNOWN_ACTION"
	CodeInvalidTimestamp    Code = "INVALID_TIMESTAMP"
	CodeEventInFuture       Code = "EVENT_IN_FUTURE"
	CodeInvalidAmount       Code = "INVALID_AMOUNT"
	CodeUnsupportedCurrency Code = "UNSUPPORTED_CURRENCY"
)

// AllCodes lists every error code the API can return.
var AllCodes = []Code{
	CodeValidationFailed, CodeInvalidJSON, CodeEmptyBatch, CodeBatchTooLarge,
	CodePayloadTooLarge, CodeOverloaded,
	CodeMissingField, CodeInvalidField, CodeUnknownEventType, CodeUnknownAction,
	CodeInvalidTimestamp, CodeEventInFuture, CodeInvalidAmount, CodeUnsupportedCurrency,
}

// Detail is one field-level problem. Index is the event's position in the
// request; it is omitted for decision requests.
type Detail struct {
	Index *int   `json:"index,omitempty"`
	Field string `json:"field,omitempty"`
	Code  Code   `json:"code"`
}

// Error is an API error and its HTTP status.
type Error struct {
	Status  int      `json:"-"`
	Code    Code     `json:"code"`
	Message string   `json:"message"`
	Details []Detail `json:"details"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// MarshalBody returns the response body: {"error": {...}}.
func (e *Error) MarshalBody() []byte {
	out := *e
	if out.Details == nil {
		out.Details = []Detail{}
	}
	b, err := json.Marshal(struct {
		Error *Error `json:"error"`
	}{&out})
	if err != nil {
		// Error contains only strings and ints; marshaling cannot fail.
		panic(err)
	}
	return b
}

func newError(status int, code Code, msg string) *Error {
	return &Error{Status: status, Code: code, Message: msg}
}

func errInvalidJSON(msg string) *Error {
	return newError(http.StatusBadRequest, CodeInvalidJSON, msg)
}

func errPayloadTooLarge() *Error {
	return newError(http.StatusRequestEntityTooLarge, CodePayloadTooLarge,
		"request body exceeds "+strconv.Itoa(MaxBodyBytes)+" bytes")
}

// ErrOverloaded is returned when the ingest queue is full.
func ErrOverloaded() *Error {
	return newError(http.StatusServiceUnavailable, CodeOverloaded, "ingest queue is full, retry later")
}

func errValidation(details []Detail) *Error {
	e := newError(http.StatusBadRequest, CodeValidationFailed,
		strconv.Itoa(len(details))+" validation error(s)")
	e.Details = details
	return e
}
