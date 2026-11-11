// Package gen generates the synthetic data set (background activity plus
// planted rule scenarios), loads it into the event log and verifies the planted
// scenarios against a running service.
package gen

import (
	"errors"
	"strconv"
	"time"

	"github.com/devastion/anti-fraud-go/internal/money"
)

// Config describes a data set. The same Config gives the same events, except
// that every timestamp is relative to Now.
type Config struct {
	Players int
	Events  int // total, including planted events
	Days    int
	Seed    uint64
	Now     time.Time // generation time
}

func (c Config) validate() error {
	switch {
	case c.Players < 1:
		return errors.New("players must be at least 1")
	case c.Days < 2:
		return errors.New("days must be at least 2")
	case c.Players > 9_999_999/3:
		return errors.New("players must be at most 3,333,333 (7-digit IDs)")
	}
	return nil
}

// ID formats. Background IDs are zero-padded so that the k6 load test can draw
// existing IDs from the same space. Planted scenarios use the "pl-" prefix
// instead, so no load-test traffic can touch them.
func PlayerID(i int) string { return padded("p", i+1, 7) }
func DeviceID(i int) string { return padded("d", i+1, 7) }
func MethodID(i int) string { return padded("pm", i+1, 7) }
func EventID(n int64) string {
	return padded("g-", int(n), 8)
}

func padded(prefix string, n, width int) string {
	var d [20]byte
	digits := strconv.AppendInt(d[:0], int64(n), 10)
	b := make([]byte, 0, len(prefix)+max(width, len(digits)))
	b = append(b, prefix...)
	for i := len(digits); i < width; i++ {
		b = append(b, '0')
	}
	return string(append(b, digits...))
}

// Device index space: [0, n) primary devices, [n, 2n) second devices (only
// for players that have one), [2n, 2n+sharedGroups) shared devices.

// model derives every per-player attribute from the player index and the
// seed, so nothing per player has to be stored.
type model struct {
	n    int
	seed uint64

	// Two affine permutations of [0, n): one spreads the shared-device
	// players, the other maps Zipf ranks to players.
	sharedA, sharedB, sharedInv uint64
	zipfA, zipfB                uint64

	sharedGroups  int // number of shared devices
	sharedPlayers int // players assigned to a shared device
}

// Shared devices have 3, 4, 5, 6 players in turn: 18 players per 4 devices.
var groupOffsets = [4]int{0, 3, 7, 12}

const groupCycle = 18

func newModel(c Config) *model {
	m := &model{n: c.Players, seed: c.Seed}
	n := uint64(c.Players)
	m.sharedA = coprime(n, 0.6180339887)
	m.sharedB = splitmix(c.Seed^0x51) % n
	m.sharedInv = modInverse(m.sharedA, n)
	m.zipfA = coprime(n, 0.7548776662)
	m.zipfB = splitmix(c.Seed^0x2f) % n

	// About 2% of all used devices are shared: S / (n + 0.3n + S) = 0.02.
	groups := int(0.02 * 1.3 * float64(c.Players) / 0.98)
	for groups > 0 && sharedPlayersFor(groups) > c.Players {
		groups--
	}
	m.sharedGroups = groups
	m.sharedPlayers = sharedPlayersFor(groups)
	return m
}

func sharedPlayersFor(groups int) int {
	full, rest := groups/4, groups%4
	return full*groupCycle + groupOffsets[rest]
}

func (m *model) hash(i int, salt uint64) uint64 {
	return splitmix(m.seed ^ salt*0x9e3779b97f4a7c15 ^ uint64(i)*0xbf58476d1ce4e5b9)
}

// currency: 60% EUR, 25% USD, 15% GBP.
func (m *model) currency(i int) money.Currency {
	switch h := m.hash(i, 1) % 100; {
	case h < 60:
		return money.EUR
	case h < 85:
		return money.USD
	default:
		return money.GBP
	}
}

// methods returns how many payment methods player i has (1–3).
func (m *model) methods(i int) int { return 1 + int(m.hash(i, 2)%3) }

func (m *model) method(i, k int) string { return MethodID(3*i + k) }

// secondDevice reports whether player i has a second device (30%).
func (m *model) secondDevice(i int) bool { return m.hash(i, 3)%100 < 30 }

func (m *model) primaryDevice(i int) string { return DeviceID(i) }

func (m *model) secondDeviceID(i int) string { return DeviceID(m.n + i) }

// sharedDevice returns the shared device of player i, if it has one.
func (m *model) sharedDevice(i int) (string, bool) {
	rank := int((uint64(i) + uint64(m.n) - m.sharedB) % uint64(m.n) * m.sharedInv % uint64(m.n))
	if rank >= m.sharedPlayers {
		return "", false
	}
	g := rank / groupCycle * 4
	off := rank % groupCycle
	for k := 3; k >= 0; k-- {
		if off >= groupOffsets[k] {
			g += k
			break
		}
	}
	return DeviceID(2*m.n + g), true
}

// zipfPlayer maps a Zipf rank to a player.
func (m *model) zipfPlayer(rank uint64) int {
	return int((rank%uint64(m.n)*m.zipfA + m.zipfB) % uint64(m.n))
}

func (m *model) ip(i int) string {
	h := m.hash(i, 4)
	b := []byte("10.")
	b = strconv.AppendUint(b, h&0xff, 10)
	b = append(b, '.')
	b = strconv.AppendUint(b, h>>8&0xff, 10)
	b = append(b, '.')
	b = strconv.AppendUint(b, h>>16&0xff, 10)
	return string(b)
}

func splitmix(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ x>>30) * 0xbf58476d1ce4e5b9
	x = (x ^ x>>27) * 0x94d049bb133111eb
	return x ^ x>>31
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// coprime returns a multiplier near frac*n that is coprime with n.
func coprime(n uint64, frac float64) uint64 {
	if n == 1 {
		return 1
	}
	a := max(uint64(float64(n)*frac), 1)
	for gcd(a, n) != 1 {
		a++
	}
	return a % n
}

// modInverse returns x with a*x ≡ 1 (mod n), for a coprime with n.
func modInverse(a, n uint64) uint64 {
	if n == 1 {
		return 0
	}
	t, newT := int64(0), int64(1)
	r, newR := int64(n), int64(a)
	for newR != 0 {
		q := r / newR
		t, newT = newT, t-q*newT
		r, newR = newR, r-q*newR
	}
	if t < 0 {
		t += int64(n)
	}
	return uint64(t)
}
