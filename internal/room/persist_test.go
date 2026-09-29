package room

import (
	"context"
	"log/slog"
	"sync"
	"testing"
	"time"

	"board/internal/crdt"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// discardLoggerInst — один общий «заглушенный» логгер для тестов persist.
var discardLoggerInst = slog.New(slog.NewTextHandler(discardWriter{}, nil))

// memStore — in-memory реализация Store/Loader для тестов восстановления.
// Не «production quality»: единственное назначение — проверить, что
// комната корректно дёргает Append/SaveSnapshot/Flush/Close и что
// WithLoaded действительно кладёт состояние в doc.
type memStore struct {
	mu       sync.Mutex
	appended []Record
	snaps    []Snapshot
	flushes  int
	closes   int
	failNext error // одна-shot ошибка, чтобы протестировать error-path
}

func (m *memStore) Append(_ string, r Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext != nil {
		err := m.failNext
		m.failNext = nil
		return err
	}
	m.appended = append(m.appended, r)
	return nil
}

func (m *memStore) SaveSnapshot(_ string, s Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.snaps = append(m.snaps, s)
	return nil
}

func (m *memStore) Flush(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.flushes++
	return nil
}

func (m *memStore) Close(string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closes++
	return nil
}

func (m *memStore) counts() (appends, snaps, flushes, closes int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.appended), len(m.snaps), m.flushes, m.closes
}

// --- ---

// Комната с store != nil обязана звать Append на каждый применённый
// батч. Это базовая «write path» инвариант: если Append не вызывается,
// рестарт сервера теряет историю.
func TestRoom_StoreAppendOnApply(t *testing.T) {
	st := &memStore{}
	cfg := DefaultConfig()
	cfg.WalFlushTick = 0        // тикер не нужен — проверим явные flush
	cfg.SnapshotEvery = 0       // снапшот не нужен — только append
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("demo", fakeCodec{}, cfg, slogDiscard(), WithStore(st))
	go r.Run(ctx)

	require.NoError(t, r.ApplyOps(ctx, "alice", "b1", []crdt.Op{up("x", 1)}))
	require.NoError(t, r.ApplyOps(ctx, "bob", "b2", []crdt.Op{up("y", 2), up("z", 3)}))

	// Ждём, пока актор разберёт команды. ApplyOps асинхронная.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a, _, _, _ := st.counts(); a >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	a, s, _, _ := st.counts()
	assert.Equal(t, 2, a, "по append на каждый применённый батч")
	assert.Equal(t, 0, s, "SnapshotEvery=0 — снапшотов не ждём")

	// Seq-и в записях соответствуют внутреннему seqN комнаты.
	st.mu.Lock()
	require.Len(t, st.appended, 2)
	assert.EqualValues(t, 1, st.appended[0].Seq)
	assert.Equal(t, "alice", st.appended[0].Origin)
	assert.EqualValues(t, 2, st.appended[1].Seq)
	assert.Len(t, st.appended[1].Ops, 2)
	st.mu.Unlock()
}

// SnapshotEvery=N означает: на N-м батче комната пишет снапшот.
func TestRoom_StoreSnapshotEvery(t *testing.T) {
	st := &memStore{}
	cfg := DefaultConfig()
	cfg.WalFlushTick = 0
	cfg.SnapshotEvery = 3
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("snap", fakeCodec{}, cfg, slogDiscard(), WithStore(st))
	go r.Run(ctx)

	for i := 1; i <= 6; i++ {
		require.NoError(t, r.ApplyOps(ctx, "a", "b", []crdt.Op{
			up("x", uint64(i)),
		}))
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		_, s, _, _ := st.counts()
		if s >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	_, s, _, _ := st.counts()
	assert.Equal(t, 2, s, "6 батчей при порог 3 → ровно 2 снапшота")
}

// WithLoaded: doc обязан «поглотить» снапшот + records ещё до Run.
func TestRoom_WithLoadedAppliesSnapshotAndRecords(t *testing.T) {
	els := []crdt.Element{
		{ID: "a", Type: crdt.TypeRect, Props: map[crdt.PropName]*crdt.Register{
			crdt.PropX: {Value: 1.0, TS: crdt.HLC{Wall: 1000, Node: "alice"}},
		}},
	}
	loaded := &Loaded{
		Snapshot: &Snapshot{Seq: 1, Elements: els, SavedAt: 1},
		Records: []Record{
			{Seq: 2, Origin: "bob", Ops: []crdt.Op{
				{Kind: crdt.OpUpsert, ID: "b",
					TS: crdt.HLC{Wall: 2000, Node: "bob"}, Type: crdt.TypeRect,
					Props: map[crdt.PropName]any{crdt.PropX: 2.0}},
			}},
		},
	}
	r := New("restored", fakeCodec{}, DefaultConfig(), slogDiscard(),
		WithLoaded(loaded))
	// seqN должен быть поднят до 2 (из loaded.Records).
	assert.EqualValues(t, 2, r.Seq())
	// Doc содержит и «a» из снапшота, и «b» из WAL.
	_, okA := r.doc.Get("a")
	_, okB := r.doc.Get("b")
	assert.True(t, okA, "снапшот-элемент a должен восстановиться")
	assert.True(t, okB, "WAL-элемент b должен восстановиться")

	// И последующий Now() вернёт Wall >= 2000 (max из loaded).
	ts := r.clock.Now()
	assert.GreaterOrEqual(t, ts.Wall, int64(2000),
		"HLC сервера обязан «догнать» восстановленные TS")
}

// Shutdown: flush + close вызываются обязательно.
func TestRoom_ShutdownFlushesAndCloses(t *testing.T) {
	st := &memStore{}
	cfg := DefaultConfig()
	cfg.WalFlushTick = 0 // тикер не нужен — только shutdown-вызов
	ctx, cancel := context.WithCancel(context.Background())
	r := New("x", fakeCodec{}, cfg, slogDiscard(), WithStore(st))
	go r.Run(ctx)

	require.NoError(t, r.ApplyOps(ctx, "a", "b", []crdt.Op{up("x", 1)}))
	// Дадим актору обработать append.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if a, _, _, _ := st.counts(); a > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-r.Done()

	_, _, f, c := st.counts()
	assert.GreaterOrEqual(t, f, 1, "shutdown обязан сделать финальный flush")
	assert.Equal(t, 1, c, "close ровно один раз")
}

// Ошибка Append не должна «ронять» комнату: ops всё равно применяются
// и расходятся; ошибка лишь логируется.
func TestRoom_StoreErrorDoesNotBreakRoom(t *testing.T) {
	st := &memStore{failNext: assertError("simulated IO failure")}
	cfg := DefaultConfig()
	cfg.WalFlushTick = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("err", fakeCodec{}, cfg, slogDiscard(), WithStore(st))
	go r.Run(ctx)

	cl := NewClient("a", "A", "#aaa", 32)
	require.NoError(t, r.Join(ctx, cl))
	readOne(t, cl, time.Second) // snapshot

	// Первый apply упадёт на Append, но ack всё равно уйдёт.
	require.NoError(t, r.ApplyOps(ctx, "a", "b1", []crdt.Op{up("x", 1)}))
	msgs := drain(t, cl, 2, time.Second)
	require.Len(t, msgs, 2, "ops-broadcast и ack должны прийти несмотря на ошибку store")
}

type assertError string

func (e assertError) Error() string { return string(e) }

// slogDiscard — маленький helper: тесты не должны спамить в stderr.
func slogDiscard() *slog.Logger { return discardLoggerInst }
