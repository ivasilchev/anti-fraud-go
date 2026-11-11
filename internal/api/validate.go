package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/rules"
	"github.com/devastion/anti-fraud-go/internal/state"
)

// Request limits.
const (
	MaxBodyBytes  = 1 << 20
	MaxBatch      = 500
	MaxIDLen      = 64 // characters (runes)
	MaxFutureSkew = 5 * time.Minute
)

// ReadBody reads at most MaxBodyBytes from r.
func ReadBody(r io.Reader) ([]byte, *Error) {
	body, err := io.ReadAll(io.LimitReader(r, MaxBodyBytes+1))
	if err != nil {
		return nil, newError(http.StatusBadRequest, CodeInvalidJSON, "cannot read request body")
	}
	if len(body) > MaxBodyBytes {
		return nil, errPayloadTooLarge()
	}
	return body, nil
}

// rawEvent keeps every field raw so that missing, null, wrongly typed and
// invalid values can be told apart and reported per field.
type rawEvent struct {
	EventID         json.RawMessage `json:"eventId"`
	PlayerID        json.RawMessage `json:"playerId"`
	OccurredAt      json.RawMessage `json:"occurredAt"`
	Type            json.RawMessage `json:"type"`
	Amount          json.RawMessage `json:"amount"`
	Currency        json.RawMessage `json:"currency"`
	DeviceID        json.RawMessage `json:"deviceId"`
	IP              json.RawMessage `json:"ip"`
	PaymentMethodID json.RawMessage `json:"paymentMethodId"`
	VoidedEventID   json.RawMessage `json:"voidedEventId"`
}

type requirement uint8

const (
	needDevice requirement = 1 << iota
	needAmount             // amount and currency
	needPaymentMethod
	needVoidedEvent
)

func requirementsFor(t state.EventType) requirement {
	switch t {
	case state.EventLogin:
		return needDevice
	case state.EventDepositAttempt, state.EventDepositApproved, state.EventDepositDeclined,
		state.EventWithdrawalRequested:
		return needAmount | needPaymentMethod
	case state.EventBet, state.EventWin:
		return needAmount
	case state.EventBetVoided:
		return needVoidedEvent
	case state.EventPayoutMethodAdded:
		return needPaymentMethod
	}
	return 0
}

// ParseEvents parses and validates a POST /events body: one event object or an
// array of 1..MaxBatch events. Any invalid event rejects the whole request.
func ParseEvents(body []byte, now time.Time) ([]state.Event, *Error) {
	var raws []json.RawMessage
	switch firstByte(body) {
	case '{':
		raws = []json.RawMessage{body}
	case '[':
		var e *Error
		if raws, e = splitBatch(body); e != nil {
			return nil, e
		}
	default:
		return nil, errInvalidJSON("body must be an event object or an array of events")
	}

	events := make([]state.Event, len(raws))
	c := collector{}
	for i, raw := range raws {
		var re rawEvent
		if firstByte(raw) != '{' {
			return nil, errInvalidJSON("event at index " + strconv.Itoa(i) + " is not an object")
		}
		if err := json.Unmarshal(raw, &re); err != nil {
			return nil, errInvalidJSON("malformed JSON: " + err.Error())
		}
		c.index = i
		events[i] = c.event(&re, now)
	}
	if len(c.details) > 0 {
		return nil, errValidation(c.details)
	}
	return events, nil
}

// splitBatch splits a JSON array into its elements. It stops after MaxBatch+1
// elements, so an oversized array of tiny values costs at most MaxBatch+1
// element allocations before BATCH_TOO_LARGE.
func splitBatch(body []byte) ([]json.RawMessage, *Error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if _, err := dec.Token(); err != nil { // '['
		return nil, errInvalidJSON("malformed JSON: " + err.Error())
	}
	raws := make([]json.RawMessage, 0, 16)
	for dec.More() {
		if len(raws) == MaxBatch {
			return nil, newError(http.StatusBadRequest, CodeBatchTooLarge,
				"batch has more than "+strconv.Itoa(MaxBatch)+" events")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, errInvalidJSON("malformed JSON: " + err.Error())
		}
		raws = append(raws, raw)
	}
	if _, err := dec.Token(); err != nil { // ']'
		return nil, errInvalidJSON("malformed JSON: " + err.Error())
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errInvalidJSON("malformed JSON: unexpected data after array")
	}
	if len(raws) == 0 {
		return nil, newError(http.StatusBadRequest, CodeEmptyBatch, "batch must contain at least one event")
	}
	return raws, nil
}

// DecisionRequest is a validated POST /decisions body.
type DecisionRequest struct {
	Action          rules.Action
	PlayerID        string
	DeviceID        string // "" when absent
	PaymentMethodID string // "" when absent
	AmountE4        int64  // 0 when absent
	AmountEURE4     int64
	Currency        money.Currency
}

type rawDecision struct {
	Action          json.RawMessage `json:"action"`
	PlayerID        json.RawMessage `json:"playerId"`
	Amount          json.RawMessage `json:"amount"`
	Currency        json.RawMessage `json:"currency"`
	DeviceID        json.RawMessage `json:"deviceId"`
	PaymentMethodID json.RawMessage `json:"paymentMethodId"`
}

// ParseDecision parses and validates a POST /decisions body.
func ParseDecision(body []byte) (DecisionRequest, *Error) {
	var rd rawDecision
	if firstByte(body) != '{' {
		return DecisionRequest{}, errInvalidJSON("body must be a JSON object")
	}
	if err := json.Unmarshal(body, &rd); err != nil {
		return DecisionRequest{}, errInvalidJSON("malformed JSON: " + err.Error())
	}
	c := collector{index: -1}
	var req DecisionRequest
	req.Action = c.action(rd.Action)
	req.PlayerID = c.id(rd.PlayerID, "playerId", true)
	payment := req.Action == rules.DepositAttempt || req.Action == rules.Withdrawal
	req.PaymentMethodID = c.id(rd.PaymentMethodID, "paymentMethodId", payment)
	req.DeviceID = c.id(rd.DeviceID, "deviceId", false)
	req.AmountE4, req.AmountEURE4, req.Currency = c.amount(rd.Amount, rd.Currency, payment)
	if len(c.details) > 0 {
		return DecisionRequest{}, errValidation(c.details)
	}
	return req, nil
}

// collector accumulates field-level problems for one request.
type collector struct {
	index   int // event index; -1 for decision requests
	details []Detail
}

func (c *collector) add(field string, code Code) {
	d := Detail{Field: field, Code: code}
	if c.index >= 0 {
		i := c.index
		d.Index = &i
	}
	c.details = append(c.details, d)
}

func (c *collector) event(re *rawEvent, now time.Time) state.Event {
	var ev state.Event
	ev.EventID = c.id(re.EventID, "eventId", true)
	ev.PlayerID = c.id(re.PlayerID, "playerId", true)
	ev.OccurredAt = c.timestamp(re.OccurredAt, now)
	ev.Type = c.eventType(re.Type)
	req := requirementsFor(ev.Type)
	ev.DeviceID = c.id(re.DeviceID, "deviceId", req&needDevice != 0)
	ev.PaymentMethodID = c.id(re.PaymentMethodID, "paymentMethodId", req&needPaymentMethod != 0)
	ev.VoidedEventID = c.id(re.VoidedEventID, "voidedEventId", req&needVoidedEvent != 0)
	ev.IP = c.id(re.IP, "ip", false)
	ev.AmountE4, ev.AmountEURE4, ev.Currency = c.amount(re.Amount, re.Currency, req&needAmount != 0)
	return ev
}

// id validates an identifier: a non-empty string of at most MaxIDLen
// characters with no control characters. stringField already guarantees valid
// UTF-8; NUL breaks C-string tooling, and control characters invite log injection.
func (c *collector) id(raw json.RawMessage, field string, required bool) string {
	s, present, ok := stringField(raw)
	switch {
	case !present:
		if required {
			c.add(field, CodeMissingField)
		}
		return ""
	case !ok || !validID(s):
		c.add(field, CodeInvalidField)
		return ""
	}
	return s
}

func validID(s string) bool {
	n := 0
	for _, r := range s {
		n++
		if n > MaxIDLen || unicode.IsControl(r) {
			return false
		}
	}
	return n > 0
}

// timestamp validates occurredAt: RFC 3339 with an explicit offset or Z, at most
// MaxFutureSkew after now. It returns unix milliseconds.
func (c *collector) timestamp(raw json.RawMessage, now time.Time) int64 {
	const field = "occurredAt"
	s, present, ok := stringField(raw)
	if !present {
		c.add(field, CodeMissingField)
		return 0
	}
	if !ok || !strictRFC3339(s) {
		c.add(field, CodeInvalidTimestamp)
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		c.add(field, CodeInvalidTimestamp)
		return 0
	}
	if t.Sub(now) > MaxFutureSkew {
		c.add(field, CodeEventInFuture)
		return 0
	}
	return t.UnixMilli()
}

// strictRFC3339 rejects what time.Parse(RFC3339Nano) tolerates but RFC 3339
// forbids: a comma before fractional seconds, and offsets with hours above 23
// or minutes above 59. time.Parse still checks everything else.
func strictRFC3339(s string) bool {
	if strings.IndexByte(s, ',') >= 0 {
		return false
	}
	if strings.HasSuffix(s, "Z") {
		return true
	}
	if len(s) < 6 {
		return false
	}
	off := s[len(s)-6:] // ±hh:mm
	if (off[0] != '+' && off[0] != '-') || off[3] != ':' {
		return false
	}
	return off[1] <= '2' && (off[1] < '2' || off[2] <= '3') && off[4] <= '5'
}

func (c *collector) eventType(raw json.RawMessage) state.EventType {
	const field = "type"
	s, present, ok := stringField(raw)
	switch {
	case !present:
		c.add(field, CodeMissingField)
		return state.EventUnknown
	case !ok:
		c.add(field, CodeInvalidField)
		return state.EventUnknown
	}
	t, known := state.ParseEventType(s)
	if !known {
		c.add(field, CodeUnknownEventType)
	}
	return t
}

func (c *collector) action(raw json.RawMessage) rules.Action {
	const field = "action"
	s, present, ok := stringField(raw)
	switch {
	case !present:
		c.add(field, CodeMissingField)
		return rules.ActionUnknown
	case !ok:
		c.add(field, CodeInvalidField)
		return rules.ActionUnknown
	}
	a, known := rules.ParseAction(s)
	if !known {
		c.add(field, CodeUnknownAction)
	}
	return a
}

// amount validates amount and currency together. They are required when
// required is set; otherwise they are optional but must come as a pair.
// It returns the amount in 10^-4 units of its currency and in 10^-4 EUR.
func (c *collector) amount(rawAmount, rawCurrency json.RawMessage, required bool) (e4, eurE4 int64, cur money.Currency) {
	as, aPresent, aOK := stringField(rawAmount)
	cs, cPresent, cOK := stringField(rawCurrency)
	if !aPresent && !cPresent && !required {
		return 0, 0, money.NoCurrency
	}
	valid := true
	if !aPresent {
		c.add("amount", CodeMissingField)
		valid = false
	} else {
		var err error
		if e4, err = money.ParseE4(as); !aOK || err != nil {
			c.add("amount", CodeInvalidAmount)
			valid = false
		}
	}
	switch {
	case !cPresent:
		c.add("currency", CodeMissingField)
		valid = false
	case !cOK:
		c.add("currency", CodeInvalidField)
		valid = false
	default:
		var known bool
		if cur, known = money.ParseCurrency(cs); !known {
			c.add("currency", CodeUnsupportedCurrency)
			valid = false
		}
	}
	if !valid {
		return 0, 0, money.NoCurrency
	}
	return e4, money.ToEUR(e4, cur), cur
}

// stringField decodes a raw JSON value. present is false for a missing field or
// null; ok is false when the value is present but not a JSON string, or when
// it holds invalid UTF-8 or an unpaired surrogate escape. encoding/json would
// silently turn those into U+FFFD, so distinct inputs could decode to the same ID.
func stringField(raw json.RawMessage) (s string, present, ok bool) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", false, false
	}
	if raw[0] != '"' || !utf8.Valid(raw) {
		return "", true, false
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1]), true, true
	}
	if !surrogatesPaired(raw) {
		return "", true, false
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", true, false
	}
	return s, true, true
}

// surrogatesPaired reports whether every \uD800-\uDBFF escape in the JSON
// string raw is immediately followed by a \uDC00-\uDFFF escape, and no low
// surrogate escape stands alone. raw is syntactically valid JSON.
func surrogatesPaired(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		if raw[i+1] != 'u' {
			i++ // skip the escaped character, e.g. the second '\' of "\\"
			continue
		}
		r := hex4(raw[i+2 : i+6])
		i += 5
		switch {
		case r >= 0xDC00 && r <= 0xDFFF:
			return false
		case r >= 0xD800 && r <= 0xDBFF:
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			if lo := hex4(raw[i+3 : i+7]); lo < 0xDC00 || lo > 0xDFFF {
				return false
			}
			i += 6
		}
	}
	return true
}

// hex4 decodes 4 hex digits; JSON syntax has already been validated.
func hex4(b []byte) rune {
	var r rune
	for _, c := range b {
		r <<= 4
		switch {
		case c >= '0' && c <= '9':
			r |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			r |= rune(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			r |= rune(c - 'A' + 10)
		}
	}
	return r
}

// firstByte returns the first non-whitespace byte of b, or 0.
func firstByte(b []byte) byte {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return c
	}
	return 0
}
