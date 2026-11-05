package gen

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/devastion/anti-fraud-go/internal/money"
	"github.com/devastion/anti-fraud-go/internal/state"
)

const (
	minute = int64(time.Minute / time.Millisecond)
	hour   = 60 * minute
	day    = 24 * hour

	// Background sessions start at least this long before Now and last at
	// most maxSessionLen events of at most maxGap each, so no background
	// event is in the future.
	sessionMargin = 45 * minute
	maxSessionLen = 24
	minGap        = 5_000
	maxGap        = 90_000
)

type sessionKind uint8

const (
	registration sessionKind = iota // first session of a player
	regular                         // later sessions, players drawn by Zipf
)

type session struct {
	start  int64
	player int32
	length uint8
	kind   sessionKind
}

// generator holds the state of one Generate call.
type generator struct {
	cfg   Config
	m     *model
	rng   *rand.Rand
	now   int64
	seq   int64 // last event number assigned
	queue eventHeap
}

// Generate produces every event of the data set in occurredAt order (ties in
// creation order) and calls emit for each. ev is only valid during the call.
// It returns the manifest of planted scenarios.
func Generate(cfg Config, emit func(ev *state.Event) error) (*Manifest, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	g := &generator{
		cfg: cfg,
		m:   newModel(cfg),
		rng: rand.New(rand.NewPCG(cfg.Seed, 0x5eed)),
		now: cfg.Now.UnixMilli(),
	}
	planted, man := plant(g)
	background := cfg.Events - len(planted)
	sessions, err := g.sessions(background)
	if err != nil {
		return nil, err
	}
	for i := range planted {
		g.queue.push(planted[i], int64(i))
	}

	emitted := 0
	drain := func(until int64) error {
		for g.queue.len() > 0 && g.queue.min().OccurredAt <= until {
			ev := g.queue.pop()
			emitted++
			if err := emit(&ev); err != nil {
				return err
			}
		}
		return nil
	}
	for _, s := range sessions {
		if err := drain(s.start); err != nil {
			return nil, err
		}
		g.session(s)
	}
	if err := drain(1<<62 - 1); err != nil {
		return nil, err
	}
	if emitted != cfg.Events {
		return nil, fmt.Errorf("generated %d events, want %d", emitted, cfg.Events)
	}
	man.Devices = 2*cfg.Players + g.m.sharedGroups
	man.SharedDevices = g.m.sharedGroups
	man.SharedPlayers = g.m.sharedPlayers
	return man, nil
}

// sessions plans all background sessions, sorted by start, with lengths that
// add up to exactly total events. Every player gets a registration session;
// the rest go to Zipf-distributed players.
func (g *generator) sessions(total int) ([]session, error) {
	n := g.cfg.Players
	if total < 3*n {
		return nil, fmt.Errorf("%d events leave %d for background activity; need at least 3 per player (%d)",
			g.cfg.Events, total, 3*n)
	}
	sessions := make([]session, 0, n+max(0, total-4*n)/8+16)
	oldest := g.now - int64(g.cfg.Days)*day
	latest := g.now - sessionMargin
	sum := 0
	for i := range n {
		from := oldest
		if _, shared := g.m.sharedDevice(i); shared {
			from = max(oldest, g.now-29*day) // shared within the last 30 days
		}
		l := 3 + g.rng.IntN(3)
		sessions = append(sessions, session{start: g.uniform(from, latest), player: int32(i), length: uint8(l), kind: registration})
		sum += l
	}
	for i := 0; sum > total; i++ { // few events per player: shorten registrations to 3
		if d := int(sessions[i].length) - 3; d > 0 {
			d = min(d, sum-total)
			sessions[i].length -= uint8(d)
			sum -= d
		}
	}

	zipf := rand.NewZipf(g.rng, 1.1, 20, uint64(n-1))
	for sum < total {
		l := 2
		for l < maxSessionLen && g.rng.IntN(7) != 0 { // geometric, mean about 8
			l++
		}
		l = min(l, total-sum)
		player := g.m.zipfPlayer(zipf.Uint64())
		sessions = append(sessions, session{start: g.uniform(oldest, latest), player: int32(player), length: uint8(l), kind: regular})
		sum += l
	}
	slices.SortStableFunc(sessions, func(a, b session) int { return cmp.Compare(a.start, b.start) })
	return sessions, nil
}

func (g *generator) uniform(from, to int64) int64 { return from + g.rng.Int64N(to-from+1) }

func (g *generator) nextID() string {
	g.seq++
	return EventID(g.seq)
}

func (g *generator) amount(cents int64, c money.Currency) (e4, eur int64) {
	e4 = cents * 100
	return e4, money.ToEUR(e4, c)
}

// session queues the events of one background session:
// LOGIN → DEPOSIT_ATTEMPT → APPROVED/DECLINED (and maybe a retry) →
// BET/WIN with occasional BET_VOIDED → WITHDRAWAL_REQUESTED.
// It produces exactly s.length events.
func (g *generator) session(s session) {
	i := int(s.player)
	pid := PlayerID(i)
	cur := g.m.currency(i)
	nMethods := g.m.methods(i)
	shared, isShared := g.m.sharedDevice(i)

	dev := g.m.primaryDevice(i)
	if s.kind == regular {
		switch r := g.rng.IntN(100); {
		case isShared && s.start >= g.now-29*day && r < 40:
			dev = shared
		case g.m.secondDevice(i) && r >= 75:
			dev = g.m.secondDeviceID(i)
		}
	}

	t := s.start
	left := int(s.length)
	// add queues an event and returns it for filling in. The pointer is
	// valid only until the next add.
	add := func(typ state.EventType) *state.Event {
		ev := state.Event{EventID: g.nextID(), PlayerID: pid, Type: typ, OccurredAt: t, DeviceID: dev}
		t += minGap + g.rng.Int64N(maxGap-minGap+1)
		left--
		g.queue.push(ev, g.seq)
		return g.queue.last()
	}
	randAmount := func(minCents, maxCents int64) (e4, eur int64) {
		return g.amount(minCents+g.rng.Int64N(maxCents-minCents+1), cur)
	}
	setAmount := func(ev *state.Event, minCents, maxCents int64) {
		ev.AmountE4, ev.AmountEURE4 = randAmount(minCents, maxCents)
		ev.Currency = cur
	}
	deposit := func(typ state.EventType, pm string, e4, eur int64) {
		ev := add(typ)
		ev.AmountE4, ev.AmountEURE4, ev.Currency, ev.PaymentMethodID = e4, eur, cur, pm
	}
	method := func() string { return g.m.method(i, g.rng.IntN(nMethods)) }

	login := add(state.EventLogin)
	login.IP = g.m.ip(i)
	if s.kind == registration && isShared {
		dev = shared // the rest of the first session happens on the shared device
	}

	if left >= 2 {
		pm := method()
		e4, eur := randAmount(1_000, 50_000)
		deposit(state.EventDepositAttempt, pm, e4, eur)
		if g.rng.IntN(100) < 88 {
			deposit(state.EventDepositApproved, pm, e4, eur)
		} else {
			deposit(state.EventDepositDeclined, pm, e4, eur)
			if left >= 2 && g.rng.IntN(2) == 0 { // retry, maybe with another method
				pm = method()
				e4, eur = randAmount(1_000, 50_000)
				deposit(state.EventDepositAttempt, pm, e4, eur)
				deposit(state.EventDepositApproved, pm, e4, eur)
			}
		}
	}

	withdraw := left >= 2 && g.rng.IntN(100) < 15
	if withdraw {
		left-- // reserved for the end
	}
	if left >= 2 && g.rng.IntN(100) < 2 {
		add(state.EventPasswordReset)
	}
	if left >= 2 && g.rng.IntN(100) < 2 {
		add(state.EventPayoutMethodAdded).PaymentMethodID = method()
	}
	lastBet := ""
	for left > 0 {
		switch r := g.rng.IntN(100); {
		case lastBet != "" && r < 3:
			add(state.EventBetVoided).VoidedEventID = lastBet
			lastBet = ""
		case r < 60:
			bet := add(state.EventBet)
			setAmount(bet, 100, 5_000)
			lastBet = bet.EventID
		default:
			setAmount(add(state.EventWin), 50, 10_000)
		}
	}
	if withdraw {
		left++
		pm := method()
		w := add(state.EventWithdrawalRequested)
		setAmount(w, 2_000, 50_000)
		w.PaymentMethodID = pm
	}
}

// eventHeap is a min-heap of events by (occurredAt, creation number). Events
// are pushed by value and may be edited through last() until the next push.
type eventHeap struct {
	items []heapItem
	dirty bool // the last pushed item may still be edited; not yet sifted
}

type heapItem struct {
	ev state.Event
	n  int64
}

func (h *eventHeap) len() int { h.fix(); return len(h.items) }

func (h *eventHeap) less(a, b int) bool {
	x, y := &h.items[a], &h.items[b]
	if x.ev.OccurredAt != y.ev.OccurredAt {
		return x.ev.OccurredAt < y.ev.OccurredAt
	}
	return x.n < y.n
}

// fix sifts up the last pushed item once it can no longer be edited.
func (h *eventHeap) fix() {
	if !h.dirty {
		return
	}
	h.dirty = false
	for i := len(h.items) - 1; i > 0; {
		p := (i - 1) / 2
		if !h.less(i, p) {
			break
		}
		h.items[i], h.items[p] = h.items[p], h.items[i]
		i = p
	}
}

func (h *eventHeap) push(ev state.Event, n int64) {
	h.fix()
	h.items = append(h.items, heapItem{ev, n})
	h.dirty = true
}

// last returns the item pushed last, for editing before the next push.
func (h *eventHeap) last() *state.Event { return &h.items[len(h.items)-1].ev }

func (h *eventHeap) min() *state.Event { h.fix(); return &h.items[0].ev }

func (h *eventHeap) pop() state.Event {
	h.fix()
	top := h.items[0].ev
	last := len(h.items) - 1
	h.items[0] = h.items[last]
	h.items[last] = heapItem{}
	h.items = h.items[:last]
	for i := 0; ; {
		l, r, m := 2*i+1, 2*i+2, i
		if l < len(h.items) && h.less(l, m) {
			m = l
		}
		if r < len(h.items) && h.less(r, m) {
			m = r
		}
		if m == i {
			return top
		}
		h.items[i], h.items[m] = h.items[m], h.items[i]
		i = m
	}
}
