package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"board/internal/crdt"
	"board/internal/hub"
	"board/internal/room"
	"board/internal/store"
)

// TestWSPersistence_ReopenServerSeesPriorState — ключевой сценарий
// Этапа 4: сервер «упал и поднялся» (в тесте — новый hub поверх
// того же kаталога) — новый join видит ровно тот doc, что был до
// перезапуска.
//
// Проверяем без «graceful shutdown» — просто закрываем первый hub,
// пересоздаём всё поверх того же store.root, и снова читаем.
func TestWSPersistence_ReopenServerSeesPriorState(t *testing.T) {
	dir := t.TempDir()

	// --- инкарнация #1 ---
	fsA, err := store.New(dir)
	require.NoError(t, err)
	srvA := newPersistenceServer(t, fsA)

	a := dial(t, srvA.url, "persist", "alice")
	a.readEnvelope(time.Second) // snapshot
	a.write(TypeOps, OpsPayload{BatchID: "b1", Ops: []crdt.Op{
		rectOp("e1", "alice", 1000, 10, 20, 30, 40),
	}})
	waitAck(t, a, "b1", 2*time.Second)
	a.write(TypeOps, OpsPayload{BatchID: "b2", Ops: []crdt.Op{
		rectOp("e2", "alice", 2000, 1, 2, 3, 4),
	}})
	waitAck(t, a, "b2", 2*time.Second)

	srvA.close() // graceful shutdown: flush + close

	// --- инкарнация #2: тот же dir, новые hub/room/actor ---
	fsB, err := store.New(dir)
	require.NoError(t, err)
	srvB := newPersistenceServer(t, fsB)

	b := dial(t, srvB.url, "persist", "bob")
	env := b.readEnvelope(2 * time.Second)
	require.Equal(t, TypeSnapshot, env.Type)
	var snap SnapshotPayload
	require.NoError(t, json.Unmarshal(env.Payload, &snap))
	require.Len(t, snap.Elements, 2,
		"после рестарта новый участник обязан увидеть оба элемента")

	byID := map[string]crdt.Element{}
	for _, e := range snap.Elements {
		byID[e.ID] = e
	}
	e1, ok := byID["e1"]
	require.True(t, ok)
	assert.Equal(t, crdt.TypeRect, e1.Type)
	xr, ok := e1.Props[crdt.PropX]
	require.True(t, ok)
	assert.Equal(t, 10.0, xr.Value.(float64))

	e2, ok := byID["e2"]
	require.True(t, ok)
	yr, ok := e2.Props[crdt.PropY]
	require.True(t, ok)
	assert.Equal(t, 2.0, yr.Value.(float64))

	srvB.close()
}

// TestWSPersistence_SnapshotTriggersAfterThreshold — убеждаемся, что
// снапшот реально пишется на disk, а не только WAL. Для этого ставим
// SnapshotEvery=2 и пишем 4 батча: на disk должен появиться snapshot.json.
func TestWSPersistence_SnapshotTriggersAfterThreshold(t *testing.T) {
	dir := t.TempDir()
	fs, err := store.New(dir)
	require.NoError(t, err)

	log := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	codec := NewJSONCodec()
	cfg := room.DefaultConfig()
	cfg.SnapshotEvery = 2
	cfg.WalFlushTick = 10 * time.Millisecond
	h := hub.New(ctx, codec, cfg, log, hub.WithStore(fs, fs))
	handler := NewHandler(h, "srv", log,
		WithAllowAllOrigins(true), WithPingInterval(time.Hour))
	mux := chi.NewMux()
	mux.Handle("/room/{id}", handler)
	hs := httptest.NewServer(mux)
	t.Cleanup(hs.Close)
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http")

	a := dial(t, wsURL, "snaproom", "alice")
	a.readEnvelope(time.Second)
	for i := 1; i <= 4; i++ {
		a.write(TypeOps, OpsPayload{BatchID: "b" + string(rune('0'+i)), Ops: []crdt.Op{
			rectOp("e"+string(rune('0'+i)), "alice", int64(1000*i), 1, 1, 1, 1),
		}})
		waitAck(t, a, "b"+string(rune('0'+i)), 2*time.Second)
	}
	// Graceful: закрываем hub, он должен сделать Flush+Close+snapshot.
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	require.NoError(t, h.Shutdown(sctx))
	require.NoError(t, fs.CloseAll())

	// Проверяем disk-состояние: хотя бы один снапшот должен быть.
	loaded, err := fs.Load("snaproom")
	require.NoError(t, err)
	require.NotNil(t, loaded.Snapshot, "порог SnapshotEvery=2 → снапшот обязан быть")
	assert.GreaterOrEqual(t, loaded.Snapshot.Seq, uint64(4))
	assert.Len(t, loaded.Snapshot.Elements, 4)
}

// TestWSPersistence_RecoveredClockDominatesLateClientClock — HLC
// сервера после recovery обязан быть строго больше всех восстановленных
// TS. Иначе новый op от клиента с «правильными» часами может
// «выиграть» у history.
func TestWSPersistence_RecoveredClockDominatesLateClientClock(t *testing.T) {
	dir := t.TempDir()
	fs, err := store.New(dir)
	require.NoError(t, err)
	srv := newPersistenceServer(t, fs)

	a := dial(t, srv.url, "clock", "alice")
	a.readEnvelope(time.Second)
	// «Дальнее будущее»: ставим Wall заведомо больше текущего времени.
	a.write(TypeOps, OpsPayload{BatchID: "future", Ops: []crdt.Op{
		rectOp("e", "alice", 4_000_000_000_000, 1, 1, 1, 1),
	}})
	waitAck(t, a, "future", 2*time.Second)
	srv.close()

	fs2, err := store.New(dir)
	require.NoError(t, err)
	srv2 := newPersistenceServer(t, fs2)
	b := dial(t, srv2.url, "clock", "bob")
	b.readEnvelope(time.Second)

	// Bob шлёт op с текущими часами (~1.7e12). Серверный Observe должен
	// «подтянуть» wall до 4e12 из recovery, а затем Now() даст TS,
	// строго больший и 4e12, и «bob-овских» 1.7e12.
	b.write(TypeOps, OpsPayload{BatchID: "now", Ops: []crdt.Op{
		{Kind: crdt.OpUpsert, ID: "e",
			TS:    crdt.HLC{Wall: time.Now().UnixMilli(), Node: "bob"},
			Props: map[crdt.PropName]any{crdt.PropStroke: "#ff0000"}},
	}})
	ack := waitAck(t, b, "now", 2*time.Second)
	_ = ack

	// Читаем snapshot через третьего клиента: stroke от bob должен
	// «победить», а x=1 (в составе первоначального rectOp) — сохраниться.
	c := dial(t, srv2.url, "clock", "carol")
	env := c.readEnvelope(2 * time.Second)
	require.Equal(t, TypeSnapshot, env.Type)
	var snap SnapshotPayload
	require.NoError(t, json.Unmarshal(env.Payload, &snap))
	require.Len(t, snap.Elements, 1)
	el := snap.Elements[0]
	sr, ok := el.Props[crdt.PropStroke]
	require.True(t, ok, "stroke должен быть")
	assert.Equal(t, "#ff0000", sr.Value)
	// И TS.stroke строго больше исходного {Wall:4e12, Logical:0, Node:"alice"}.
	// Wall при этом РАВЕН 4e12 — серверный Now() не «перегоняет» будущее,
	// а поднимает Logical. Это и есть инвариант HLC: каузально больше,
	// даже если физически «в том же миллисекундном тике».
	farFuture := crdt.HLC{Wall: 4_000_000_000_000, Logical: 0, Node: "alice"}
	assert.Greater(t, sr.TS.Compare(farFuture), 0,
		"server-stamped TS must causally dominate the recovered far-future op")
	srv2.close()
}

// --- helpers specific to persistence tests ---

type persistServer struct {
	hs    *httptest.Server
	h     *hub.Hub
	url   string
	cance context.CancelFunc
}

func newPersistenceServer(t *testing.T, fs *store.FileStore) *persistServer {
	t.Helper()
	log := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithCancel(context.Background())
	codec := NewJSONCodec()
	cfg := room.DefaultConfig()
	cfg.WalFlushTick = 10 * time.Millisecond
	h := hub.New(ctx, codec, cfg, log, hub.WithStore(fs, fs))
	handler := NewHandler(h, "srv", log,
		WithAllowAllOrigins(true), WithPingInterval(time.Hour))
	mux := chi.NewMux()
	mux.Handle("/room/{id}", handler)
	hs := httptest.NewServer(mux)
	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http")
	s := &persistServer{hs: hs, h: h, url: wsURL, cance: cancel}
	t.Cleanup(s.close)
	return s
}

// close — «graceful restart»: закрываем listeners, дожидаемся, что
// комнаты отработают shutdown-путь (Flush+Close для store).
func (s *persistServer) close() {
	if s.hs != nil {
		s.hs.Close()
	}
	if s.h != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = s.h.Shutdown(ctx)
	}
	if s.cance != nil {
		s.cance()
	}
}
