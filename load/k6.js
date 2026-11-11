// Load test (SPEC §10). Run through `make load`, which sets RUN_ID and then
// checks that every accepted event reached the event log.
//
// Env: BASE_URL (default http://localhost:8080), RUN_ID (required), DURATION
// (default 10m), PLAYERS (default 1000000, the generator's ID space),
// RESULTS_DIR (default load/results, relative to the working directory).
import http from 'k6/http';
import exec from 'k6/execution';
import { check } from 'k6';
import { Counter, Rate, Trend } from 'k6/metrics';

const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';
const RESULTS_DIR = __ENV.RESULTS_DIR || 'load/results';
const RUN_ID = __ENV.RUN_ID;
const DURATION = __ENV.DURATION || '10m';
const PLAYERS = parseInt(__ENV.PLAYERS || '1000000', 10);
if (!RUN_ID) {
  throw new Error('RUN_ID is required');
}

const acceptedEvents = new Counter('accepted_events'); // as reported by the server
const duplicateEvents = new Counter('duplicate_events');
const resentEvents = new Counter('resent_events'); // duplicates the script sent on purpose
const visibilityOK = new Rate('visibility_ok');
const visibilityMs = new Trend('visibility_ms', true);

export const options = {
  discardResponseBodies: false,
  summaryTrendStats: ['avg', 'min', 'med', 'max', 'p(90)', 'p(95)', 'p(99)', 'count'],
  scenarios: {
    events_single: {
      executor: 'constant-arrival-rate', exec: 'singleEvent',
      rate: 1500, timeUnit: '1s', duration: DURATION, preAllocatedVUs: 300, maxVUs: 1000,
    },
    events_batch: {
      executor: 'constant-arrival-rate', exec: 'batchEvents',
      rate: 30, timeUnit: '1s', duration: DURATION, preAllocatedVUs: 50, maxVUs: 300,
    },
    decisions: {
      executor: 'constant-arrival-rate', exec: 'decide',
      rate: 500, timeUnit: '1s', duration: DURATION, preAllocatedVUs: 100, maxVUs: 500,
    },
    visibility: {
      executor: 'constant-arrival-rate', exec: 'visibility',
      rate: 2, timeUnit: '1s', duration: DURATION, preAllocatedVUs: 10, maxVUs: 50,
    },
  },
  thresholds: {
    'http_req_duration{scenario:decisions}': ['p(95)<30', 'p(99)<80'],
    http_req_failed: ['rate<0.001'],
    visibility_ok: ['rate==1'],
    visibility_ms: ['p(99)<1000'],
    // Arrival-rate executors skip iterations when no VU is free; any skip
    // means the target rates were not offered and latencies look too good.
    dropped_iterations: ['count==0'],
  },
};

const JSON_HEADERS = { headers: { 'Content-Type': 'application/json' } };
const CURRENCIES = ['EUR', 'EUR', 'EUR', 'USD', 'GBP'];

function pad(n, width) {
  const s = String(n);
  return s.length >= width ? s : '0'.repeat(width - s.length) + s;
}
function randInt(n) { return Math.floor(Math.random() * n); }
function amount(minCents, maxCents) {
  const c = minCents + randInt(maxCents - minCents + 1);
  return `${Math.floor(c / 100)}.${pad(c % 100, 2)}`;
}
// Background IDs from the generator: p%07d, its primary device d%07d, and its
// payment methods pm%07d (3i+1 .. 3i+3).
function player() { return randInt(PLAYERS); }
function playerID(i) { return 'p' + pad(i + 1, 7); }
function deviceID(i) { return 'd' + pad(i + 1, 7); }
function methodID(i) { return 'pm' + pad(3 * i + 1 + randInt(3), 7); }
function occurredAt(maxAgeMs) { return new Date(Date.now() - randInt(maxAgeMs + 1)).toISOString(); }

// Per-VU state: recent events (for resends) and the last bet (for voids).
const recent = [];
let lastBet = null;

function eventID(i) {
  return `lt-${RUN_ID}-${exec.vu.idInTest}-${exec.vu.iterationInScenario}-${i}`;
}

// newEvent returns a fresh event, or with 1% probability an exact resend of
// a recent one (marked by wasResend, for the caller to count once sent).
let wasResend = false;
function newEvent(i) {
  wasResend = recent.length > 0 && Math.random() < 0.01;
  if (wasResend) {
    return recent[randInt(recent.length)];
  }
  const p = player();
  const ev = { eventId: eventID(i), playerId: playerID(p), occurredAt: occurredAt(300000), deviceId: deviceID(p) };
  const cur = CURRENCIES[randInt(CURRENCIES.length)];
  const r = randInt(100);
  if (r < 15) {
    ev.type = 'LOGIN';
    ev.ip = `10.${randInt(256)}.${randInt(256)}.${randInt(256)}`;
  } else if (r < 31) {
    ev.type = r < 23 ? 'DEPOSIT_ATTEMPT' : r < 29 ? 'DEPOSIT_APPROVED' : 'DEPOSIT_DECLINED';
    Object.assign(ev, { amount: amount(1000, 50000), currency: cur, paymentMethodId: methodID(p) });
  } else if (r < 71) {
    Object.assign(ev, { type: 'BET', amount: amount(100, 5000), currency: cur });
  } else if (r < 92) {
    Object.assign(ev, { type: 'WIN', amount: amount(50, 10000), currency: cur });
  } else if (r < 93 && lastBet) {
    Object.assign(ev, { type: 'BET_VOIDED', playerId: lastBet.playerId, deviceId: lastBet.deviceId, voidedEventId: lastBet.eventId });
    lastBet = null;
  } else if (r < 94) {
    ev.type = 'PASSWORD_RESET';
  } else if (r < 95) {
    Object.assign(ev, { type: 'PAYOUT_METHOD_ADDED', paymentMethodId: methodID(p) });
  } else {
    Object.assign(ev, { type: 'WITHDRAWAL_REQUESTED', amount: amount(2000, 50000), currency: cur, paymentMethodId: methodID(p) });
  }
  if (ev.type === 'BET') {
    lastBet = ev;
  }
  recent.push(ev);
  if (recent.length > 50) {
    recent.shift();
  }
  return ev;
}

function postEvents(body) {
  const res = http.post(`${BASE_URL}/events`, JSON.stringify(body), JSON_HEADERS);
  if (res.status === 200) {
    const r = res.json();
    acceptedEvents.add(r.accepted);
    duplicateEvents.add(r.duplicates);
  }
  return res;
}

export function singleEvent() {
  const ev = newEvent(0);
  if (wasResend) {
    resentEvents.add(1);
  }
  const res = postEvents(ev);
  check(res, { 'event accepted': (r) => r.status === 200 });
}

export function batchEvents() {
  const batch = [];
  const ids = {};
  while (batch.length < 50) {
    const ev = newEvent(batch.length);
    if (ids[ev.eventId]) {
      continue; // keep exactly 50 events; duplicates within one batch are covered by the integration tests
    }
    ids[ev.eventId] = true;
    if (wasResend) {
      resentEvents.add(1);
    }
    batch.push(ev);
  }
  const res = postEvents(batch);
  check(res, { 'batch accepted': (r) => r.status === 200 });
}

export function decide() {
  const p = player();
  const r = randInt(100);
  const req = { playerId: playerID(p) };
  if (Math.random() < 0.9) {
    req.deviceId = deviceID(p);
  }
  if (r < 40) {
    Object.assign(req, { action: 'DEPOSIT_ATTEMPT', amount: amount(1000, 50000), currency: 'EUR', paymentMethodId: methodID(p) });
  } else if (r < 80) {
    Object.assign(req, { action: 'WITHDRAWAL', amount: amount(2000, 50000), currency: 'EUR', paymentMethodId: methodID(p) });
  } else {
    req.action = 'BONUS_CLAIM';
  }
  const res = http.post(`${BASE_URL}/decisions`, JSON.stringify(req), JSON_HEADERS);
  check(res, { 'decision returned': (r) => r.status === 200 });
}

// visibility: a fresh player gets 5 declines with 3 methods; the very next
// decision must already see them.
export function visibility() {
  const p = `lt-${RUN_ID}-vis-${exec.vu.idInTest}-${exec.vu.iterationInScenario}`;
  const now = new Date().toISOString();
  const events = [];
  for (let i = 0; i < 5; i++) {
    events.push({
      eventId: eventID(i), playerId: p, type: 'DEPOSIT_DECLINED', occurredAt: now,
      amount: '10.00', currency: 'EUR', paymentMethodId: `${p}-pm${i % 3}`,
    });
  }
  const start = Date.now();
  const posted = postEvents(events);
  const res = http.post(`${BASE_URL}/decisions`, JSON.stringify({
    action: 'DEPOSIT_ATTEMPT', playerId: p, amount: '10.00', currency: 'EUR', paymentMethodId: `${p}-pm9`,
  }), JSON_HEADERS);
  visibilityMs.add(Date.now() - start);
  const ok = posted.status === 200 && res.status === 200 && res.json().decision === 'REVIEW';
  visibilityOK.add(ok);
  check(res, { 'visibility: REVIEW': () => ok });
}

function fmt(v, digits) { return v === undefined ? '-' : v.toFixed(digits); }

function thresholdsOK(m) {
  if (!m || !m.thresholds) return '';
  return Object.values(m.thresholds).every((t) => t.ok) ? 'pass' : 'FAIL';
}

export function handleSummary(data) {
  const m = data.metrics;
  const v = (name) => (m[name] ? m[name].values : {});
  const dec = v('http_req_duration{scenario:decisions}');
  const reqs = v('http_reqs');
  const secs = data.state.testRunDurationMs / 1000;
  const accepted = v('accepted_events').count || 0;
  const rows = [
    ['run id', RUN_ID, ''],
    ['duration', `${fmt(secs, 0)} s`, ''],
    ['requests', `${reqs.count} (${fmt(reqs.rate, 0)}/s)`, ''],
    ['events accepted (unique)', `${accepted} (${fmt(accepted / secs, 0)}/s)`, ''],
    ['duplicates reported / resent', `${v('duplicate_events').count || 0} / ${v('resent_events').count || 0}`, ''],
    ['decisions', `${dec.count} (${fmt(dec.count / secs, 0)}/s)`, ''],
    ['dropped iterations (k6 could not keep the rate)', `${v('dropped_iterations').count || 0}`,
      thresholdsOK(m.dropped_iterations) || 'pass'],
    ['decision latency p50 / p95 / p99 / max (ms)',
      `${fmt(dec.med, 2)} / ${fmt(dec['p(95)'], 2)} / ${fmt(dec['p(99)'], 2)} / ${fmt(dec.max, 2)}`,
      thresholdsOK(m['http_req_duration{scenario:decisions}'])],
    ['all requests p50 / p95 / p99 (ms)',
      `${fmt(v('http_req_duration').med, 2)} / ${fmt(v('http_req_duration')['p(95)'], 2)} / ${fmt(v('http_req_duration')['p(99)'], 2)}`, ''],
    ['error rate', `${fmt(100 * (v('http_req_failed').rate || 0), 4)} %`, thresholdsOK(m.http_req_failed)],
    ['visibility ok', `${fmt(100 * (v('visibility_ok').rate || 0), 2)} %`, thresholdsOK(m.visibility_ok)],
    ['visibility p50 / p99 / max (ms)',
      `${fmt(v('visibility_ms').med, 1)} / ${fmt(v('visibility_ms')['p(99)'], 1)} / ${fmt(v('visibility_ms').max, 1)}`,
      thresholdsOK(m.visibility_ms)],
  ];
  const w0 = Math.max(...rows.map((r) => r[0].length));
  const w1 = Math.max(...rows.map((r) => r[1].length));
  const table = rows.map((r) => `${r[0].padEnd(w0)}  ${r[1].padEnd(w1)}  ${r[2]}`).join('\n') + '\n';
  return {
    stdout: table,
    [`${RESULTS_DIR}/table.txt`]: table,
    [`${RESULTS_DIR}/summary.json`]: JSON.stringify(data, null, 2),
    [`${RESULTS_DIR}/run.env`]: `RUN_ID=${RUN_ID}\nACCEPTED=${accepted}\n`,
  };
}
