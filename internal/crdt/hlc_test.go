package crdt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHLC_Compare(t *testing.T) {
	cases := []struct {
		a, b HLC
		want int
	}{
		{HLC{100, 0, "x"}, HLC{100, 0, "x"}, 0},
		{HLC{100, 0, "a"}, HLC{101, 0, "a"}, -1},
		{HLC{100, 1, "a"}, HLC{100, 0, "a"}, 1},
		{HLC{100, 0, "b"}, HLC{100, 0, "a"}, 1},
		{HLC{100, 0, "a"}, HLC{100, 0, "b"}, -1},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, c.a.Compare(c.b), "a=%v b=%v", c.a, c.b)
	}
}

func TestHLC_After(t *testing.T) {
	a := HLC{Wall: 1000, Logical: 1, Node: "x"}
	b := HLC{Wall: 1000, Logical: 0, Node: "y"}
	assert.True(t, a.After(b))
	assert.False(t, b.After(a))
}

// Monotonicity: много Now() подряд строго возрастают.
// Это инвариант: если он сломается — два события получат одинаковый
// таймстемп, и «кто старше» станет неопределённым.
func TestClock_NowIsMonotonic(t *testing.T) {
	c := NewClock("node1")
	prev := c.Now()
	for i := 0; i < 500; i++ {
		cur := c.Now()
		require.Greater(t, cur.Compare(prev), 0,
			"iteration %d: %v !> %v", i, cur, prev)
		prev = cur
	}
}

// После Observe локальные часы НЕ МОГУТ отстать от увиденного remote.
// Это каузальность: локальное событие, «ответ» на remote, должно быть
// строго больше remote.
func TestClock_ObserveCausality(t *testing.T) {
	c := NewClock("alice")
	remote := HLC{Wall: time.Now().Add(time.Hour).UnixMilli(), Logical: 5, Node: "bob"}
	got := c.Observe(remote)
	assert.Greater(t, got.Compare(remote), 0, "response must be > remote")

	// И следующие Now() всё ещё больше remote.
	next := c.Now()
	assert.Greater(t, next.Compare(remote), 0)
}

// Две вершины, «ping-pong» обмен: каждый ответ строго больше запроса.
func TestClock_PingPongCausality(t *testing.T) {
	a := NewClock("alice")
	b := NewClock("bob")
	var lastA, lastB HLC
	for i := 0; i < 20; i++ {
		lastA = a.Now()
		// bob «видит» alice и отвечает.
		lastB = b.Observe(lastA)
		require.Greater(t, lastB.Compare(lastA), 0, "step %d: bob must be > alice", i)
		// alice «видит» bob и отвечает.
		lastA = a.Observe(lastB)
		require.Greater(t, lastA.Compare(lastB), 0, "step %d: alice must be > bob", i)
	}
}

// Physical — удобная конвертация для UI (Этап 5 — история).
func TestHLC_Physical(t *testing.T) {
	ts := HLC{Wall: 1_700_000_000_000}
	assert.Equal(t, int64(1_700_000_000), ts.Physical().Unix())
}
