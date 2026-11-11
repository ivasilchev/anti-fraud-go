package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/rules"
	"github.com/devastion/anti-fraud-go/internal/state"
)

var now = time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

// validEvents holds one valid event per type, with exactly the required fields.
var validEvents = map[string]map[string]any{
	"LOGIN":                {"deviceId": "d1"},
	"PASSWORD_RESET":       {},
	"DEPOSIT_ATTEMPT":      {"amount": "10.00", "currency": "EUR", "paymentMethodId": "pm1"},
	"DEPOSIT_APPROVED":     {"amount": "10.00", "currency": "EUR", "paymentMethodId": "pm1"},
	"DEPOSIT_DECLINED":     {"amount": "10.00", "currency": "EUR", "paymentMethodId": "pm1"},
	"BET":                  {"amount": "10.00", "currency": "EUR"},
	"WIN":                  {"amount": "10.00", "currency": "EUR"},
	"BET_VOIDED":           {"voidedEventId": "e0"},
	"PAYOUT_METHOD_ADDED":  {"paymentMethodId": "pm1"},
	"WITHDRAWAL_REQUESTED": {"amount": "10.00", "currency": "EUR", "paymentMethodId": "pm1"},
}

func event(typ string, overrides map[string]any) map[string]any {
	ev := map[string]any{
		"eventId":    "e1",
		"playerId":   "p1",
		"occurredAt": "2025-06-01T11:59:00Z",
		"type":       typ,
	}
	for k, v := range validEvents[typ] {
		ev[k] = v
	}
	for k, v := range overrides {
		if v == deleteField {
			delete(ev, k)
		} else {
			ev[k] = v
		}
	}
	return ev
}

type sentinel struct{}

// deleteField removes a field when used as an override value.
var deleteField = &sentinel{}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func intp(i int) *int { return &i }

func assertValidation(t *testing.T, err *Error, want ...Detail) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want VALIDATION_FAILED %v", want)
	}
	if err.Status != http.StatusBadRequest || err.Code != CodeValidationFailed {
		t.Fatalf("got %d %s (%s), want 400 VALIDATION_FAILED", err.Status, err.Code, err.Message)
	}
	got := fmt.Sprint(detailStrings(err.Details))
	if w := fmt.Sprint(detailStrings(want)); got != w {
		t.Fatalf("details = %s, want %s", got, w)
	}
}

func detailStrings(ds []Detail) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		idx := "-"
		if d.Index != nil {
			idx = fmt.Sprint(*d.Index)
		}
		out[i] = idx + ":" + d.Field + ":" + string(d.Code)
	}
	return out
}

func assertTopLevel(t *testing.T, err *Error, status int, code Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %d %s", status, code)
	}
	if err.Status != status || err.Code != code {
		t.Fatalf("got %d %s (%s), want %d %s", err.Status, err.Code, err.Message, status, code)
	}
}

func TestParseEventsValidEveryType(t *testing.T) {
	for typ := range validEvents {
		t.Run(typ, func(t *testing.T) {
			evs, err := ParseEvents(mustJSON(t, event(typ, nil)), now)
			if err != nil {
				t.Fatalf("unexpected error: %v %v", err, err.Details)
			}
			if len(evs) != 1 || evs[0].Type.String() != typ {
				t.Fatalf("got %+v", evs)
			}
		})
	}
}

func TestParseEventsFields(t *testing.T) {
	body := mustJSON(t, []map[string]any{
		event("DEPOSIT_APPROVED", map[string]any{
			"eventId": "e1", "occurredAt": "2025-06-01T13:59:00.1239+02:00",
			"amount": "10.01", "currency": "USD", "deviceId": "d1", "ip": "10.0.0.1",
		}),
		event("BET", map[string]any{"eventId": "e2", "amount": "5", "currency": "GBP"}),
		event("BET_VOIDED", map[string]any{"eventId": "e3"}),
	})
	evs, err := ParseEvents(body, now)
	if err != nil {
		t.Fatalf("unexpected error: %v %v", err, err.Details)
	}
	want := []state.Event{
		{
			EventID: "e1", PlayerID: "p1", Type: state.EventDepositApproved,
			OccurredAt: now.Add(-time.Minute + 123*time.Millisecond).UnixMilli(),
			AmountE4:   100100, AmountEURE4: 92092, Currency: money.USD,
			DeviceID: "d1", IP: "10.0.0.1", PaymentMethodID: "pm1",
		},
		{
			EventID: "e2", PlayerID: "p1", Type: state.EventBet,
			OccurredAt: now.Add(-time.Minute).UnixMilli(),
			AmountE4:   50000, AmountEURE4: 58500, Currency: money.GBP,
		},
		{
			EventID: "e3", PlayerID: "p1", Type: state.EventBetVoided,
			OccurredAt: now.Add(-time.Minute).UnixMilli(), VoidedEventID: "e0",
		},
	}
	if len(evs) != len(want) {
		t.Fatalf("got %d events, want %d", len(evs), len(want))
	}
	for i := range want {
		if evs[i] != want[i] {
			t.Errorf("event %d:\n got %+v\nwant %+v", i, evs[i], want[i])
		}
	}
}

func TestParseEventsBatchSizes(t *testing.T) {
	batch := func(n int) []byte {
		evs := make([]map[string]any, n)
		for i := range evs {
			evs[i] = event("PASSWORD_RESET", map[string]any{"eventId": fmt.Sprintf("e%d", i)})
		}
		return mustJSON(t, evs)
	}
	t.Run("EMPTY_BATCH", func(t *testing.T) {
		_, err := ParseEvents([]byte(" [ ] "), now)
		assertTopLevel(t, err, http.StatusBadRequest, CodeEmptyBatch)
	})
	t.Run("BATCH_TOO_LARGE at 501", func(t *testing.T) {
		_, err := ParseEvents(batch(501), now)
		assertTopLevel(t, err, http.StatusBadRequest, CodeBatchTooLarge)
	})
	t.Run("500 events accepted", func(t *testing.T) {
		evs, err := ParseEvents(batch(500), now)
		if err != nil || len(evs) != 500 {
			t.Fatalf("got %d events, err %v", len(evs), err)
		}
	})
	t.Run("1 event array accepted", func(t *testing.T) {
		if evs, err := ParseEvents(batch(1), now); err != nil || len(evs) != 1 {
			t.Fatalf("got %d events, err %v", len(evs), err)
		}
	})
}

func TestParseEventsInvalidJSON(t *testing.T) {
	for name, body := range map[string]string{
		"empty body":          "",
		"truncated object":    `{"eventId": "e1"`,
		"truncated array":     `[{"eventId": "e1"}`,
		"trailing garbage":    `{"eventId": "e1"} x`,
		"string at top level": `"event"`,
		"null at top level":   `null`,
		"number at top level": `42`,
		"non-object element":  `[1]`,
		"null element":        `[null]`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEvents([]byte(body), now)
			assertTopLevel(t, err, http.StatusBadRequest, CodeInvalidJSON)
		})
	}
}

func TestParseEventsMissingField(t *testing.T) {
	for _, field := range []string{"eventId", "playerId", "occurredAt", "type"} {
		t.Run(field, func(t *testing.T) {
			_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{field: deleteField})), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: field, Code: CodeMissingField})
		})
		t.Run(field+" null", func(t *testing.T) {
			_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{field: nil})), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: field, Code: CodeMissingField})
		})
	}
	for typ, fields := range validEvents {
		for field := range fields {
			t.Run(typ+" without "+field, func(t *testing.T) {
				_, err := ParseEvents(mustJSON(t, event(typ, map[string]any{field: deleteField})), now)
				assertValidation(t, err, Detail{Index: intp(0), Field: field, Code: CodeMissingField})
			})
		}
	}
}

func TestParseEventsAmountAndCurrencyComeAsAPair(t *testing.T) {
	_, err := ParseEvents(mustJSON(t, event("LOGIN", map[string]any{"amount": "1.00"})), now)
	assertValidation(t, err, Detail{Index: intp(0), Field: "currency", Code: CodeMissingField})

	_, err = ParseEvents(mustJSON(t, event("LOGIN", map[string]any{"currency": "EUR"})), now)
	assertValidation(t, err, Detail{Index: intp(0), Field: "amount", Code: CodeMissingField})

	evs, err := ParseEvents(mustJSON(t, event("LOGIN", map[string]any{"amount": "1.00", "currency": "GBP"})), now)
	if err != nil || evs[0].AmountEURE4 != 11700 {
		t.Fatalf("optional amount: got %+v, err %v", evs, err)
	}
}

func TestParseEventsInvalidField(t *testing.T) {
	long := strings.Repeat("x", MaxIDLen+1)
	for _, field := range []string{"eventId", "playerId", "deviceId", "paymentMethodId", "voidedEventId", "ip"} {
		for name, value := range map[string]any{
			"empty":       "",
			"too long":    long,
			"number":      42,
			"object":      map[string]any{},
			"array":       []string{"a"},
			"bool":        true,
			"65 runes":    strings.Repeat("é", MaxIDLen+1),
			"escaped 65":  strings.Repeat("\n", MaxIDLen+1),
			"unicode esc": strings.Repeat("\u00e9", MaxIDLen+1),
		} {
			t.Run(field+" "+name, func(t *testing.T) {
				_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{field: value})), now)
				assertValidation(t, err, Detail{Index: intp(0), Field: field, Code: CodeInvalidField})
			})
		}
	}
	t.Run("type not a string", func(t *testing.T) {
		_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"type": 3})), now)
		assertValidation(t, err, Detail{Index: intp(0), Field: "type", Code: CodeInvalidField})
	})
	t.Run("currency not a string", func(t *testing.T) {
		_, err := ParseEvents(mustJSON(t, event("BET", map[string]any{"currency": 978})), now)
		assertValidation(t, err, Detail{Index: intp(0), Field: "currency", Code: CodeInvalidField})
	})
}

func TestParseEventsIDLengthLimit(t *testing.T) {
	for name, id := range map[string]string{
		"64 ASCII":     strings.Repeat("x", MaxIDLen),
		"64 multibyte": strings.Repeat("é", MaxIDLen),
		"escaped":      `a"b\c`,
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"eventId": id})), now)
			if err != nil {
				t.Fatalf("unexpected error: %v %v", err, err.Details)
			}
			if evs[0].EventID != id {
				t.Fatalf("eventId = %q, want %q", evs[0].EventID, id)
			}
		})
	}
}

func TestParseEventsUnknownEventType(t *testing.T) {
	for _, typ := range []string{"", "login", "DEPOSIT", "WITHDRAWAL"} {
		t.Run(typ, func(t *testing.T) {
			_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"type": typ})), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: "type", Code: CodeUnknownEventType})
		})
	}
}

func TestParseEventsTimestamp(t *testing.T) {
	for _, ts := range []string{
		"2025-06-01T11:59:00",       // no offset
		"2025-06-01 11:59:00Z",      // space separator
		"2025-06-01",                // date only
		"2025-06-01T11:59Z",         // no seconds
		"2025-06-01T11:59:00+0100",  // offset without colon
		"2025-06-01T25:00:00Z",      // bad hour
		"2025-02-30T00:00:00Z",      // bad day
		"1717243140",                // unix seconds as string
		"2025-06-01T11:59:00+24:00", // offset hour out of range
		"2025-06-01T11:59:00+01:60", // offset minute out of range
		"2025-06-01T11:59:00-30:00", // offset hour out of range
		"2025-06-01T11:59:00,5Z",    // comma decimal separator
		"2025-06-01T11:59:00z",      // lowercase z
		"yesterday",
		"",
	} {
		t.Run("INVALID_TIMESTAMP "+ts, func(t *testing.T) {
			_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"occurredAt": ts})), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: "occurredAt", Code: CodeInvalidTimestamp})
		})
	}
	t.Run("INVALID_TIMESTAMP number", func(t *testing.T) {
		_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"occurredAt": 1717243140})), now)
		assertValidation(t, err, Detail{Index: intp(0), Field: "occurredAt", Code: CodeInvalidTimestamp})
	})
	for name, ts := range map[string]string{
		"Z":                 "2025-06-01T11:59:00Z",
		"offset":            "2025-06-01T14:59:00+03:00",
		"fractional":        "2025-06-01T11:59:00.123456789Z",
		"exactly 5m ahead":  now.Add(MaxFutureSkew).Format(time.RFC3339Nano),
		"far in the past":   "2001-01-01T00:00:00Z",
		"negative offset":   "2025-06-01T06:59:00-05:00",
		"max offset":        "2025-06-02T11:58:00+23:59",
		"min offset":        "2025-05-31T12:00:00-23:59",
		"leap day accepted": "2024-02-29T00:00:00Z",
	} {
		t.Run("valid "+name, func(t *testing.T) {
			if _, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"occurredAt": ts})), now); err != nil {
				t.Fatalf("unexpected error: %v %v", err, err.Details)
			}
		})
	}
	t.Run("EVENT_IN_FUTURE beyond 5m", func(t *testing.T) {
		ts := now.Add(MaxFutureSkew + time.Millisecond).Format(time.RFC3339Nano)
		_, err := ParseEvents(mustJSON(t, event("PASSWORD_RESET", map[string]any{"occurredAt": ts})), now)
		assertValidation(t, err, Detail{Index: intp(0), Field: "occurredAt", Code: CodeEventInFuture})
	})
}

func TestParseEventsInvalidAmount(t *testing.T) {
	for name, amount := range map[string]any{
		"zero":            "0",
		"zero decimals":   "0.00",
		"3 decimals":      "1.234",
		"13 int digits":   "1234567890123",
		"negative":        "-1",
		"exponent":        "1e3",
		"leading space":   " 1",
		"trailing dot":    "1.",
		"comma":           "1,00",
		"empty":           "",
		"JSON number":     10.5,
		"JSON int":        10,
		"bool":            true,
		"object":          map[string]any{"v": "1"},
		"escaped digits":  "\u0031\u0032\u002e\u0033\u0034\u0035",
		"full-width one":  "１",
		"thousands sep":   "1 000",
		"hex":             "0x1",
		"plus sign":       "+1",
		"leading dot":     ".5",
		"double dot":      "1..0",
		"trailing letter": "1.0a",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEvents(mustJSON(t, event("BET", map[string]any{"amount": amount})), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: "amount", Code: CodeInvalidAmount})
		})
	}
}

func TestParseEventsUnsupportedCurrency(t *testing.T) {
	for _, cur := range []string{"JPY", "eur", "EURO", "", " EUR"} {
		t.Run(cur, func(t *testing.T) {
			_, err := ParseEvents(mustJSON(t, event("DEPOSIT_ATTEMPT", map[string]any{"currency": cur})), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: "currency", Code: CodeUnsupportedCurrency})
		})
	}
}

func TestParseEventsBatchIsAllOrNothing(t *testing.T) {
	body := mustJSON(t, []map[string]any{
		event("BET", map[string]any{"eventId": "ok-0"}),
		event("BET", map[string]any{"eventId": "bad-1", "amount": "0"}),
		event("BET", map[string]any{"eventId": "ok-2"}),
		event("NOPE", map[string]any{"eventId": "bad-3", "playerId": deleteField}),
	})
	evs, err := ParseEvents(body, now)
	if evs != nil {
		t.Fatalf("got %d events, want none", len(evs))
	}
	assertValidation(t, err,
		Detail{Index: intp(1), Field: "amount", Code: CodeInvalidAmount},
		Detail{Index: intp(3), Field: "playerId", Code: CodeMissingField},
		Detail{Index: intp(3), Field: "type", Code: CodeUnknownEventType},
	)
}

func TestReadBody(t *testing.T) {
	t.Run("PAYLOAD_TOO_LARGE over 1 MB", func(t *testing.T) {
		_, err := ReadBody(bytes.NewReader(make([]byte, MaxBodyBytes+1)))
		assertTopLevel(t, err, http.StatusBadRequest, CodePayloadTooLarge)
	})
	t.Run("exactly 1 MB accepted", func(t *testing.T) {
		b, err := ReadBody(bytes.NewReader(make([]byte, MaxBodyBytes)))
		if err != nil || len(b) != MaxBodyBytes {
			t.Fatalf("got %d bytes, err %v", len(b), err)
		}
	})
}

func decisionBody(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	d := map[string]any{
		"action": "WITHDRAWAL", "playerId": "p1", "amount": "25.50",
		"currency": "USD", "paymentMethodId": "pm1", "deviceId": "d1",
	}
	for k, v := range overrides {
		if v == deleteField {
			delete(d, k)
		} else {
			d[k] = v
		}
	}
	return mustJSON(t, d)
}

func TestParseDecisionValid(t *testing.T) {
	req, err := ParseDecision(decisionBody(t, nil))
	if err != nil {
		t.Fatalf("unexpected error: %v %v", err, err.Details)
	}
	want := DecisionRequest{
		Action: rules.Withdrawal, PlayerID: "p1", DeviceID: "d1", PaymentMethodID: "pm1",
		AmountE4: 255000, AmountEURE4: 234600, Currency: money.USD,
	}
	if req != want {
		t.Fatalf("got %+v, want %+v", req, want)
	}

	bonus, err := ParseDecision([]byte(`{"action":"BONUS_CLAIM","playerId":"p1"}`))
	if err != nil || bonus.Action != rules.BonusClaim || bonus.PlayerID != "p1" || bonus.DeviceID != "" {
		t.Fatalf("BONUS_CLAIM with only playerId: got %+v, err %v", bonus, err)
	}
}

func TestParseDecisionErrors(t *testing.T) {
	tests := []struct {
		name      string
		overrides map[string]any
		want      []Detail
	}{
		{"MISSING_FIELD action", map[string]any{"action": deleteField},
			[]Detail{{Field: "action", Code: CodeMissingField}}},
		{"MISSING_FIELD playerId", map[string]any{"playerId": deleteField},
			[]Detail{{Field: "playerId", Code: CodeMissingField}}},
		{"UNKNOWN_ACTION", map[string]any{"action": "REFUND"},
			[]Detail{{Field: "action", Code: CodeUnknownAction}}},
		{"INVALID_FIELD action not a string", map[string]any{"action": 1},
			[]Detail{{Field: "action", Code: CodeInvalidField}}},
		{"INVALID_FIELD empty deviceId", map[string]any{"deviceId": ""},
			[]Detail{{Field: "deviceId", Code: CodeInvalidField}}},
		{"INVALID_AMOUNT", map[string]any{"amount": "12.345"},
			[]Detail{{Field: "amount", Code: CodeInvalidAmount}}},
		{"UNSUPPORTED_CURRENCY", map[string]any{"currency": "CHF"},
			[]Detail{{Field: "currency", Code: CodeUnsupportedCurrency}}},
		{"BONUS_CLAIM validates optional amount", map[string]any{"action": "BONUS_CLAIM", "amount": "abc"},
			[]Detail{{Field: "amount", Code: CodeInvalidAmount}}},
	}
	for _, action := range []string{"DEPOSIT_ATTEMPT", "WITHDRAWAL"} {
		for _, field := range []string{"amount", "currency", "paymentMethodId"} {
			tests = append(tests, struct {
				name      string
				overrides map[string]any
				want      []Detail
			}{
				"MISSING_FIELD " + action + " " + field,
				map[string]any{"action": action, field: deleteField},
				[]Detail{{Field: field, Code: CodeMissingField}},
			})
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseDecision(decisionBody(t, tt.overrides))
			assertValidation(t, err, tt.want...)
		})
	}
	t.Run("deviceId is optional", func(t *testing.T) {
		if _, err := ParseDecision(decisionBody(t, map[string]any{"deviceId": deleteField})); err != nil {
			t.Fatalf("unexpected error: %v %v", err, err.Details)
		}
	})
	t.Run("INVALID_JSON", func(t *testing.T) {
		for _, body := range []string{"", "[]", `[{"action":"WITHDRAWAL"}]`, `{"action":`, `"x"`, "null"} {
			_, err := ParseDecision([]byte(body))
			assertTopLevel(t, err, http.StatusBadRequest, CodeInvalidJSON)
		}
	})
}

func TestErrOverloaded(t *testing.T) {
	err := ErrOverloaded()
	assertTopLevel(t, err, http.StatusServiceUnavailable, CodeOverloaded)
	if got, want := string(err.MarshalBody()),
		`{"error":{"code":"OVERLOADED","message":"ingest queue is full, retry later","details":[]}}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

func TestErrorBodyShape(t *testing.T) {
	_, err := ParseEvents(mustJSON(t, []map[string]any{
		event("BET", nil), event("BET", nil), event("BET", nil),
		event("BET", map[string]any{"amount": "1.001"}),
	}), now)
	want := `{"error":{"code":"VALIDATION_FAILED","message":"1 validation error(s)",` +
		`"details":[{"index":3,"field":"amount","code":"INVALID_AMOUNT"}]}}`
	if got := string(err.MarshalBody()); got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}

	_, derr := ParseDecision([]byte(`{"action":"X","playerId":"p"}`))
	want = `{"error":{"code":"VALIDATION_FAILED","message":"1 validation error(s)",` +
		`"details":[{"field":"action","code":"UNKNOWN_ACTION"}]}}`
	if got := string(derr.MarshalBody()); got != want {
		t.Fatalf("decision body = %s, want %s", got, want)
	}
}

// TestEveryErrorCodeIsProduced triggers each code through the public API and
// checks that together they cover AllCodes, so a new code needs a trigger here.
func TestEveryErrorCodeIsProduced(t *testing.T) {
	seen := map[Code]bool{}
	record := func(err *Error) {
		t.Helper()
		if err == nil {
			t.Fatal("expected an error")
		}
		seen[err.Code] = true
		for _, d := range err.Details {
			seen[d.Code] = true
		}
	}
	ev := func(overrides map[string]any) []byte { return mustJSON(t, event("BET", overrides)) }
	for _, body := range [][]byte{
		[]byte("{"),
		[]byte("[]"),
		mustJSON(t, make([]map[string]any, MaxBatch+1)),
		ev(map[string]any{"playerId": deleteField}),
		ev(map[string]any{"playerId": ""}),
		ev(map[string]any{"type": "NOPE"}),
		ev(map[string]any{"occurredAt": "2025-06-01T11:59:00"}),
		ev(map[string]any{"occurredAt": now.Add(time.Hour).Format(time.RFC3339)}),
		ev(map[string]any{"amount": "0"}),
		ev(map[string]any{"currency": "JPY"}),
	} {
		_, err := ParseEvents(body, now)
		record(err)
	}
	_, err := ParseDecision([]byte(`{"action":"REFUND","playerId":"p1"}`))
	record(err)
	_, err = ReadBody(bytes.NewReader(make([]byte, MaxBodyBytes+1)))
	record(err)
	record(ErrOverloaded())
	record(errInternal())
	record(errNotReady())

	for _, code := range AllCodes {
		if !seen[code] {
			t.Errorf("code %s is never produced", code)
		}
	}
	if len(seen) != len(AllCodes) {
		t.Errorf("produced %d distinct codes, AllCodes lists %d", len(seen), len(AllCodes))
	}
}

func TestParseEventsOversizedArrayIsRejectedCheaply(t *testing.T) {
	// 1 MiB of "1," is ~524k elements; decoding them all used ~64 MB.
	body := append([]byte{'['}, bytes.Repeat([]byte("1,"), MaxBodyBytes/2-1)...)
	body = append(body, '1', ']')
	var err *Error
	allocs := testing.AllocsPerRun(3, func() { _, err = ParseEvents(body, now) })
	assertTopLevel(t, err, http.StatusBadRequest, CodeBatchTooLarge)
	// Bounded by MaxBatch+1 elements (~6 allocs each), not by the ~524k in the body.
	if allocs > 10*MaxBatch {
		t.Fatalf("%.0f allocations to reject an oversized batch, want <= %d", allocs, 10*MaxBatch)
	}
}

func TestParseEventsArraySyntax(t *testing.T) {
	ev := string(mustJSON(t, event("PASSWORD_RESET", nil)))
	for name, body := range map[string]string{
		"missing comma":         "[" + ev + " " + ev + "]",
		"trailing comma":        "[" + ev + ",]",
		"trailing data":         "[" + ev + "] x",
		"second array":          "[" + ev + "][" + ev + "]",
		"extra closing bracket": "[" + ev + "]]",
		"unterminated":          "[" + ev + ",",
		"leading comma":         "[," + ev + "]",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEvents([]byte(body), now)
			assertTopLevel(t, err, http.StatusBadRequest, CodeInvalidJSON)
		})
	}
	t.Run("whitespace around elements accepted", func(t *testing.T) {
		evs, err := ParseEvents([]byte(" [ "+ev+" ,\n"+ev+" ]\n"), now)
		if err != nil || len(evs) != 2 {
			t.Fatalf("got %d events, err %v", len(evs), err)
		}
	})
}

// idBody builds a PASSWORD_RESET event whose eventId is the raw JSON string literal lit.
func idBody(lit string) []byte {
	return []byte(`{"eventId":` + lit + `,"playerId":"p1","occurredAt":"2025-06-01T11:59:00Z","type":"PASSWORD_RESET"}`)
}

func TestParseEventsRejectsIDsThatWouldBeRewritten(t *testing.T) {
	// encoding/json would turn each of these into U+FFFD, merging distinct IDs.
	for name, lit := range map[string]string{
		"invalid byte after escape":   "\"\\n\xffb\"",
		"invalid byte after \\u":      "\"\\u0041\xff\"",
		"lone high surrogate":         `"\ud800"`,
		"lone high surrogate at end":  `"a\udbff"`,
		"lone low surrogate":          `"\udfff"`,
		"high followed by non-low":    `"\ud800\u0041"`,
		"high followed by plain char": `"\ud800x"`,
		"reversed pair":               `"\ude00\ud83d"`,
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := ParseEvents(idBody(lit), now)
			if err == nil {
				t.Fatalf("accepted %+q", evs[0].EventID)
			}
			assertValidation(t, err, Detail{Index: intp(0), Field: "eventId", Code: CodeInvalidField})
		})
	}
	for name, tc := range map[string]struct{ lit, want string }{
		"valid surrogate pair":          {`"\ud83d\ude00"`, "😀"},
		"uppercase hex pair":            {`"\uD83D\uDE00"`, "😀"},
		"escaped backslash then u text": {`"\\ud800"`, `\ud800`},
		"literal U+FFFD":                {`"a\ufffdb"`, "a\ufffdb"},
	} {
		t.Run("accepts "+name, func(t *testing.T) {
			evs, err := ParseEvents(idBody(tc.lit), now)
			if err != nil {
				t.Fatalf("unexpected error: %v %v", err, err.Details)
			}
			if evs[0].EventID != tc.want {
				t.Fatalf("eventId = %+q, want %+q", evs[0].EventID, tc.want)
			}
		})
	}
}

func TestParseEventsRejectsControlCharactersInIDs(t *testing.T) {
	for name, lit := range map[string]string{
		"newline": `"a\nb"`,
		"tab":     `"a\tb"`,
		"escape":  `"\u001b[31m"`,
		"DEL":     `"a\u007fb"`,
		"C1 NEL":  `"a\u0085b"`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseEvents(idBody(lit), now)
			assertValidation(t, err, Detail{Index: intp(0), Field: "eventId", Code: CodeInvalidField})
		})
	}
}

func TestParseEventsRejectsNULAndInvalidUTF8(t *testing.T) {
	for name, body := range map[string]string{
		"NUL escape":        `{"eventId":"a\u0000b","playerId":"p1","occurredAt":"2025-06-01T11:59:00Z","type":"PASSWORD_RESET"}`,
		"invalid UTF-8":     "{\"eventId\":\"a\xffb\",\"playerId\":\"p1\",\"occurredAt\":\"2025-06-01T11:59:00Z\",\"type\":\"PASSWORD_RESET\"}",
		"NUL in ip":         `{"eventId":"e1","ip":"\u0000","playerId":"p1","occurredAt":"2025-06-01T11:59:00Z","type":"PASSWORD_RESET"}`,
		"invalid UTF-8 pid": "{\"eventId\":\"e1\",\"playerId\":\"\xc3\",\"occurredAt\":\"2025-06-01T11:59:00Z\",\"type\":\"PASSWORD_RESET\"}",
	} {
		t.Run(name, func(t *testing.T) {
			evs, err := ParseEvents([]byte(body), now)
			if err == nil {
				t.Fatalf("accepted %+v", evs[0])
			}
			if err.Code != CodeValidationFailed || len(err.Details) != 1 || err.Details[0].Code != CodeInvalidField {
				t.Fatalf("got %s %v, want one INVALID_FIELD", err.Code, detailStrings(err.Details))
			}
		})
	}
}
