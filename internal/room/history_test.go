package room

import (
	"context"
	"testing"
	"time"

	"board/internal/crdt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- ring buffer ---

func TestHistoryRing_PushAndReadSequential(t *testing.T) {
	h := newHistoryRing(4)
	for i := uint64(1); i <= 3; i++ {
		h.push(Record{Seq: i, Origin: "a", Ops: []crdt.Op{up("x", i)}})
	}
	all := h.all()
	require.Len(t, all, 3)
	assert.Equal(t, uint64(1), all[0].Seq)
	assert.Equal(t, uint64(3), h.current())
}

func TestHistoryRing_WrapOverwritesOldest(t *testing.T) {
	h := newHistoryRing(3)
	for i := uint64(1); i <= 5; i++ {
		h.push(Record{Seq: i})
	}
	all := h.all()
	require.Len(t, all, 3, "cap=3 → ровно 3 записи")
	assert.Equal(t, uint64(3), all[0].Seq, "1,2 вытеснены")
	assert.Equal(t, uint64(5), all[2].Seq)
	assert.Equal(t, uint64(5), h.current())
}

func TestHistoryRing_RangeBetween(t *testing.T) {
	h := newHistoryRing(10)
	for i := uint64(1); i <= 6; i++ {
		h.push(Record{Seq: i})
	}
	mid := h.rangeBetween(2, 4)
	require.Len(t, mid, 3)
	assert.Equal(t, uint64(2), mid[0].Seq)
	assert.Equal(t, uint64(4), mid[2].Seq)

	// from==0 → без нижней.
	head := h.rangeBetween(0, 2)
	require.Len(t, head, 2)
	assert.Equal(t, uint64(1), head[0].Seq)

	// to==0 → без верхней.
	tail := h.rangeBetween(5, 0)
	require.Len(t, tail, 2)
	assert.Equal(t, uint64(5), tail[0].Seq)
	assert.Equal(t, uint64(6), tail[1].Seq)

	// Пустой диапазон.
	none := h.rangeBetween(10, 20)
	assert.Empty(t, none)
}

// --- integration with Room ---

// HistoryCmd должен читаться из того же Run-цикла, что и ApplyOps —
// иначе «прочитали то, чего ещё нет». Тест проверяет causality: после
// await-а ack-а на N батчей History(N, N) обязан вернуть N-ный Record.
func TestRoom_HistoryAfterApplyOps(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WalFlushTick = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("h1", fakeCodec{}, cfg, slogDiscard())
	go r.Run(ctx)

	cl := NewClient("alice", "Alice", "#aaa", 8)
	require.NoError(t, r.Join(ctx, cl))
	// Snapshot при Join — проглатываем, он не влияет на history.
	_ = readOne(t, cl, time.Second)

	for i := uint64(1); i <= 3; i++ {
		require.NoError(t, r.ApplyOps(ctx, "alice", "b", []crdt.Op{up("x", i)}))
		// Дождаться ack, чтобы гарантировать: dispatch уже обработал
		// этот батч (history.push выполнен).
		_ = readOne(t, cl, time.Second) // ops broadcast
		_ = readOne(t, cl, time.Second) // ack
	}

	recs, err := r.History(ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, recs, 3)
	assert.Equal(t, uint64(1), recs[0].Seq)
	assert.Equal(t, uint64(3), recs[2].Seq)
	assert.Equal(t, "alice", recs[2].Origin)
}

func TestRoom_HistoryRangeQuery(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WalFlushTick = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("h2", fakeCodec{}, cfg, slogDiscard())
	go r.Run(ctx)

	cl := NewClient("a", "A", "#aaa", 8)
	require.NoError(t, r.Join(ctx, cl))
	_ = readOne(t, cl, time.Second)

	for i := uint64(1); i <= 5; i++ {
		require.NoError(t, r.ApplyOps(ctx, "a", "b", []crdt.Op{up("x", i)}))
		_ = readOne(t, cl, time.Second)
		_ = readOne(t, cl, time.Second)
	}

	recs, err := r.History(ctx, 2, 4)
	require.NoError(t, err)
	require.Len(t, recs, 3)
	assert.Equal(t, uint64(2), recs[0].Seq)
	assert.Equal(t, uint64(4), recs[2].Seq)
}

func TestRoom_HistoryEmptyBeforeAnyOps(t *testing.T) {
	cfg := DefaultConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("empty", fakeCodec{}, cfg, slogDiscard())
	go r.Run(ctx)

	recs, err := r.History(ctx, 0, 0)
	require.NoError(t, err)
	assert.Empty(t, recs)
}

func TestRoom_HistoryRespectsRoomStop(t *testing.T) {
	cfg := DefaultConfig()
	ctx, cancel := context.WithCancel(context.Background())
	r := New("stop", fakeCodec{}, cfg, slogDiscard())
	go r.Run(ctx)

	// Дать Run-циклу стартовать.
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-r.Done()

	_, err := r.History(context.Background(), 0, 0)
	assert.ErrorIs(t, err, ErrRoomStopped)
}

func TestRoom_HistorySizeOverride(t *testing.T) {
	cfg := DefaultConfig()
	cfg.HistorySize = 2
	cfg.WalFlushTick = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("h3", fakeCodec{}, cfg, slogDiscard())
	go r.Run(ctx)

	cl := NewClient("a", "A", "#aaa", 8)
	require.NoError(t, r.Join(ctx, cl))
	_ = readOne(t, cl, time.Second)

	for i := uint64(1); i <= 4; i++ {
		require.NoError(t, r.ApplyOps(ctx, "a", "b", []crdt.Op{up("x", i)}))
		_ = readOne(t, cl, time.Second)
		_ = readOne(t, cl, time.Second)
	}
	// Кольцо cap=2, значит внутри должны остаться seq=3 и seq=4.
	recs, err := r.History(ctx, 0, 0)
	require.NoError(t, err)
	require.Len(t, recs, 2)
	assert.Equal(t, uint64(3), recs[0].Seq)
	assert.Equal(t, uint64(4), recs[1].Seq)
}
