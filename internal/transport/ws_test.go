package transport

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"nhooyr.io/websocket"

	"board/internal/crdt"
	"board/internal/hub"
	"board/internal/presence"
	"board/internal/room"
)

func TestMain(m *testing.M) {
	// Даём горутинам hub/ws chance доработать перед проверкой.
	// nhooyr.io/websocket держит reader-горутину в syscall.Read до фактического
	// закрытия сокета; на Windows это проявляется как «зависшая» горутина после
	// t.Cleanup. Игнорируем её по имени верхней функции.
	goleak.VerifyTestMain(m, goleak.IgnoreTopFunction("internal/poll.(*FD).Read"))
}

// --- integration harness ---

type testServer struct {
	hs  *httptest.Server
	hub *hub.Hub
	log *slog.Logger
	url string // ws://...
}

func newTestServer(t *testing.T) *testServer {
	t.Helper()
	log := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	codec := NewJSONCodec()
	h := hub.New(ctx, codec, room.DefaultConfig(), log)
	handler := NewHandler(h, "test-server", log,
		WithAllowAllOrigins(true),
		WithPingInterval(time.Hour), // в тестах heartbeat мешает
	)

	mux := chi.NewMux()
	mux.Handle("/room/{id}", handler)
	hs := httptest.NewServer(mux)
	t.Cleanup(func() {
		hs.Close()
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		_ = h.Shutdown(sctx)
	})

	wsURL := "ws" + strings.TrimPrefix(hs.URL, "http")
	return &testServer{hs: hs, hub: h, log: log, url: wsURL}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

type wsClient struct {
	t    *testing.T
	id   string
	conn *websocket.Conn
}

// dial — connect + hello + welcome. Возвращает клиента, у которого в буфере
// чтения уже лежит следующий кадр (обычно snapshot).
func dial(t *testing.T, baseURL, roomID, clientID string) *wsClient {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, baseURL+"/room/"+roomID, nil)
	require.NoError(t, err)
	t.Cleanup(func() { conn.CloseNow() })

	hello, err := marshalEnvelope(TypeHello, HelloPayload{ClientID: clientID, Name: clientID})
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, hello))

	c := &wsClient{t: t, id: clientID, conn: conn}
	env := c.readEnvelope(2 * time.Second)
	require.Equal(t, TypeWelcome, env.Type, "первым должен прийти welcome")
	return c
}

// readEnvelope читает один кадр с таймаутом. Фейлы — fatal.
func (c *wsClient) readEnvelope(d time.Duration) Envelope {
	c.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	_, data, err := c.conn.Read(ctx)
	require.NoError(c.t, err, "read frame")
	var env Envelope
	require.NoError(c.t, json.Unmarshal(data, &env))
	return env
}

func (c *wsClient) write(typ string, payload any) {
	c.t.Helper()
	body, err := json.Marshal(payload)
	require.NoError(c.t, err)
	env, err := json.Marshal(Envelope{Type: typ, Proto: ProtoV1, Payload: body})
	require.NoError(c.t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(c.t, c.conn.Write(ctx, websocket.MessageText, env))
}

// --- helpers to build ops compactly ---

func rectOp(id, node string, wall int64, x, y, w, h float64) crdt.Op {
	return crdt.Op{
		Kind: crdt.OpUpsert, ID: id, TS: crdt.HLC{Wall: wall, Node: node},
		Type: crdt.TypeRect,
		Props: map[crdt.PropName]any{
			crdt.PropX: x, crdt.PropY: y, crdt.PropW: w, crdt.PropH: h,
		},
	}
}

// --- tests ---

func TestWS_TwoClients_SyncOps(t *testing.T) {
	srv := newTestServer(t)

	a := dial(t, srv.url, "demo", "alice")
	b := dial(t, srv.url, "demo", "bob")

	envA := a.readEnvelope(time.Second)
	require.Equal(t, TypeSnapshot, envA.Type)
	var snapA SnapshotPayload
	require.NoError(t, json.Unmarshal(envA.Payload, &snapA))
	assert.Empty(t, snapA.Elements)

	envB := b.readEnvelope(time.Second)
	require.Equal(t, TypeSnapshot, envB.Type)

	// Alice шлёт батч из 2 upsert-ов.
	batchID := "b-1"
	a.write(TypeOps, OpsPayload{BatchID: batchID, Ops: []crdt.Op{
		rectOp("x", "alice", 1000, 100, 100, 50, 50),
		rectOp("y", "alice", 1001, 200, 200, 30, 30),
	}})

	// Bob должен получить broadcast с теми же ops.
	envBroadcast := b.readEnvelope(2 * time.Second)
	require.Equal(t, TypeOps, envBroadcast.Type)
	var gotB OpsBroadcastPayload
	require.NoError(t, json.Unmarshal(envBroadcast.Payload, &gotB))
	assert.Equal(t, "alice", gotB.Origin)
	require.Len(t, gotB.Ops, 2)
	assert.Equal(t, "x", gotB.Ops[0].ID)

	// Alice получает и broadcast, и ack.
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		e := a.readEnvelope(2 * time.Second)
		seen[e.Type] = true
		if e.Type == TypeAck {
			var ack AckPayload
			require.NoError(t, json.Unmarshal(e.Payload, &ack))
			assert.Equal(t, batchID, ack.BatchID)
			assert.Greater(t, ack.Seq, uint64(0))
		}
	}
	assert.True(t, seen[TypeOps], "alice должна увидеть свой broadcast (эхо)")
	assert.True(t, seen[TypeAck], "alice должна получить ack")

	// Bob кидает новый op, alice видит.
	b.write(TypeOps, OpsPayload{BatchID: "b-2", Ops: []crdt.Op{
		rectOp("z", "bob", 2000, 1, 1, 2, 2),
	}})
	env := a.readEnvelope(2 * time.Second)
	require.Equal(t, TypeOps, env.Type)
	var got OpsBroadcastPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	assert.Equal(t, "bob", got.Origin)
	require.Len(t, got.Ops, 1)
	assert.Equal(t, "z", got.Ops[0].ID)
}

func TestWS_NewClientGetsSnapshotOfExistingState(t *testing.T) {
	srv := newTestServer(t)

	a := dial(t, srv.url, "sync", "alice")
	a.readEnvelope(time.Second) // snapshot
	a.write(TypeOps, OpsPayload{BatchID: "1", Ops: []crdt.Op{
		rectOp("e1", "alice", 5000, 7, 8, 9, 10),
	}})
	waitAck(t, a, "1", 2*time.Second)

	b := dial(t, srv.url, "sync", "bob")
	env := b.readEnvelope(2 * time.Second)
	require.Equal(t, TypeSnapshot, env.Type)
	var snap SnapshotPayload
	require.NoError(t, json.Unmarshal(env.Payload, &snap))
	require.Len(t, snap.Elements, 1)
	assert.Equal(t, "e1", snap.Elements[0].ID)
	v, ok := snap.Elements[0].Props[crdt.PropX]
	require.True(t, ok)
	// JSON round-trip: float64 приходит как float64.
	num, ok := v.Value.(float64)
	require.True(t, ok, "got %T", v.Value)
	assert.Equal(t, 7.0, num)
}

// Ключевой для Этапа 3 сценарий: «A двигает — Б красит». Bob и Alice
// параллельно шлют частичные ops (разные props) на один и тот же id.
// Оба изменения должны сохраниться на Bob-е.
func TestWS_PartialOpsMerge(t *testing.T) {
	srv := newTestServer(t)
	a := dial(t, srv.url, "merge", "alice")
	b := dial(t, srv.url, "merge", "bob")
	a.readEnvelope(time.Second)
	b.readEnvelope(time.Second)

	a.write(TypeOps, OpsPayload{BatchID: "init", Ops: []crdt.Op{
		rectOp("e", "alice", 100, 0, 0, 20, 20),
	}})
	waitAck(t, a, "init", 2*time.Second)
	// Bob должен увидеть init.
	readUntilType(t, b, TypeOps, 2*time.Second)

	// Bob меняет color, Alice двигает — независимо.
	b.write(TypeOps, OpsPayload{BatchID: "c1", Ops: []crdt.Op{
		{Kind: crdt.OpUpsert, ID: "e", TS: crdt.HLC{Wall: 200, Node: "bob"},
			Props: map[crdt.PropName]any{crdt.PropStroke: "#ff0000"}},
	}})
	a.write(TypeOps, OpsPayload{BatchID: "m1", Ops: []crdt.Op{
		{Kind: crdt.OpUpsert, ID: "e", TS: crdt.HLC{Wall: 201, Node: "alice"},
			Props: map[crdt.PropName]any{crdt.PropX: 100.0, crdt.PropY: 100.0}},
	}})
	// Дожидаемся ack-ов.
	waitAck(t, b, "c1", 2*time.Second)
	waitAck(t, a, "m1", 2*time.Second)

	// Bob делает новый «join» через snapshot — проще: отправляем «ping»,
	// читаем snapshot при подключении третьего клиента.
	c := dial(t, srv.url, "merge", "carol")
	env := c.readEnvelope(2 * time.Second)
	require.Equal(t, TypeSnapshot, env.Type)
	var snap SnapshotPayload
	require.NoError(t, json.Unmarshal(env.Payload, &snap))
	require.Len(t, snap.Elements, 1)
	el := snap.Elements[0]
	// И color, и X/Y должны быть живы.
	colorR, ok := el.Props[crdt.PropStroke]
	require.True(t, ok, "stroke должен сохраниться")
	assert.Equal(t, "#ff0000", colorR.Value)
	xR, ok := el.Props[crdt.PropX]
	require.True(t, ok, "x должен сохраниться")
	xf, _ := xR.Value.(float64)
	assert.Equal(t, 100.0, xf)
}

func TestWS_RejectBadHello(t *testing.T) {
	srv := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, srv.url+"/room/x", nil)
	require.NoError(t, err)
	defer conn.CloseNow()

	// Первая команда — не hello.
	require.NoError(t, conn.Write(ctx, websocket.MessageText,
		[]byte(`{"type":"ops","proto":"v1","payload":{"batchId":"b","ops":[]}}`)))

	_, _, err = conn.Read(ctx)
	assert.Error(t, err, "close expected")
}

func TestWS_RejectDuplicateClientID(t *testing.T) {
	srv := newTestServer(t)
	a := dial(t, srv.url, "dup", "same")
	a.readEnvelope(time.Second) // snapshot

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, srv.url+"/room/dup", nil)
	require.NoError(t, err)
	defer conn.CloseNow()
	hello, _ := marshalEnvelope(TypeHello, HelloPayload{ClientID: "same"})
	require.NoError(t, conn.Write(ctx, websocket.MessageText, hello))
	// Сервер обязан прислать welcome, потом error{join_failed}, потом закрыть.
	// Читаем до обрыва и проверяем, что error-конверт был.
	sawError := false
	for {
		_, data, rerr := conn.Read(ctx)
		if rerr != nil {
			break
		}
		if strings.Contains(string(data), `"type":"error"`) {
			sawError = true
		}
	}
	assert.True(t, sawError, "expected error envelope before close (duplicate clientId)")
}

// TestWS_PasswordGate — сквозная проверка «модалки входа":
// создатель запирает комнату паролем, чужого обязан отбить с
// кодом room_password и close 1008 (чтобы фронт НЕ реконнектился).
func TestWS_PasswordGate(t *testing.T) {
	srv := newTestServer(t)

	// Создатель — с паролем.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	creator, _, err := websocket.Dial(ctx, srv.url+"/room/locked", nil)
	require.NoError(t, err)
	defer creator.CloseNow()
	hello, _ := marshalEnvelope(TypeHello, HelloPayload{ClientID: "owner", Password: "sekret"})
	require.NoError(t, creator.Write(ctx, websocket.MessageText, hello))
	_, data, err := creator.Read(ctx)
	require.NoError(t, err, "creator should get welcome")
	assert.Contains(t, string(data), `"type":"welcome"`)

	// Чужой без пароля — error{room_password} + закрытие 1008.
	intruder, _, err := websocket.Dial(ctx, srv.url+"/room/locked", nil)
	require.NoError(t, err)
	defer intruder.CloseNow()
	hello2, _ := marshalEnvelope(TypeHello, HelloPayload{ClientID: "eve"})
	require.NoError(t, intruder.Write(ctx, websocket.MessageText, hello2))

	sawReject := false
	var closeErr error
	for {
		_, d, rerr := intruder.Read(ctx)
		if rerr != nil {
			closeErr = rerr
			break
		}
		if strings.Contains(string(d), `"room_password"`) {
			sawReject = true
		}
	}
	assert.True(t, sawReject, "expected room_password error frame")
	require.Error(t, closeErr)
	if st := websocket.CloseStatus(closeErr); st != websocket.StatusPolicyViolation {
		assert.Equal(t, websocket.StatusPolicyViolation, st, "close code must be 1008")
	}

	// Гость с верным паролем — заходит.
	guest, _, err := websocket.Dial(ctx, srv.url+"/room/locked", nil)
	require.NoError(t, err)
	defer guest.CloseNow()
	hello3, _ := marshalEnvelope(TypeHello, HelloPayload{ClientID: "bob", Password: "sekret"})
	require.NoError(t, guest.Write(ctx, websocket.MessageText, hello3))
	_, data, err = guest.Read(ctx)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"type":"welcome"`)
}

// waitAck читает сообщения до batchID=target; если попадается ops —
// игнорируем (это может быть эхо).
func waitAck(t *testing.T, c *wsClient, batchID string, d time.Duration) AckPayload {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		env := c.readEnvelope(time.Until(deadline))
		if env.Type == TypeAck {
			var ack AckPayload
			require.NoError(t, json.Unmarshal(env.Payload, &ack))
			require.Equal(t, batchID, ack.BatchID)
			return ack
		}
	}
	t.Fatal("ack not received in time")
	return AckPayload{}
}

// --- HTTP health endpoint (smoke) ---

func TestWS_HandlerIsRegistered(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.hs.URL + "/room/x")
	require.NoError(t, err)
	defer resp.Body.Close()
	// Не-WS GET на /room/{id}: websocket.Accept отвечает 426 Upgrade Required
	// (или 400 на старых версиях библиотеки). И то, и другое — «handler
	// зарегистрирован, но upgrade не выполнен», что и проверяется.
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusUpgradeRequired},
		resp.StatusCode, "expected 400 or 426 for non-WS GET")
}

// --- presence tests ---

func TestWS_PresenceBroadcastToOther(t *testing.T) {
	srv := newTestServer(t)
	a := dial(t, srv.url, "pr", "alice")
	b := dial(t, srv.url, "pr", "bob")
	a.readEnvelope(time.Second)
	b.readEnvelope(time.Second)

	a.write(TypePresence, presence.State{Cursor: &presence.Point{X: 42, Y: 24}, Tool: "rect"})

	env := readUntilType(t, b, TypePresence, 2*time.Second)
	var got PresenceBroadcastPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	require.Len(t, got.Entries, 1)
	assert.Equal(t, "alice", got.Entries[0].ClientID)
	require.NotNil(t, got.Entries[0].State.Cursor)
	assert.Equal(t, 42.0, got.Entries[0].State.Cursor.X)
	assert.Equal(t, "rect", got.Entries[0].State.Tool)
	assert.False(t, got.Entries[0].Removed)
}

func TestWS_PresenceSelfIsIncludedButFiltered(t *testing.T) {
	srv := newTestServer(t)
	a := dial(t, srv.url, "pr-self", "alice")
	a.readEnvelope(time.Second)

	a.write(TypePresence, presence.State{Cursor: &presence.Point{X: 1, Y: 2}})
	env := readUntilType(t, a, TypePresence, 2*time.Second)
	var got PresenceBroadcastPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	require.Len(t, got.Entries, 1)
	assert.Equal(t, "alice", got.Entries[0].ClientID)
}

func TestWS_PresenceRemovedOnDisconnect(t *testing.T) {
	srv := newTestServer(t)
	a := dial(t, srv.url, "pr-off", "alice")
	b := dial(t, srv.url, "pr-off", "bob")
	a.readEnvelope(time.Second)
	b.readEnvelope(time.Second)

	a.write(TypePresence, presence.State{Cursor: &presence.Point{X: 5, Y: 5}})
	readUntilType(t, b, TypePresence, 2*time.Second)

	a.conn.Close(websocket.StatusNormalClosure, "bye")

	env := readUntilType(t, b, TypePresence, 3*time.Second)
	var got PresenceBroadcastPayload
	require.NoError(t, json.Unmarshal(env.Payload, &got))
	require.NotEmpty(t, got.Entries)
	found := false
	for _, e := range got.Entries {
		if e.ClientID == "alice" && e.Removed {
			found = true
		}
	}
	assert.True(t, found, "expected removed=true for alice")
}

// readUntilType — читает кадры до первого совпадения с want.
func readUntilType(t *testing.T, c *wsClient, want string, d time.Duration) Envelope {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		env := c.readEnvelope(time.Until(deadline))
		if env.Type == want {
			return env
		}
	}
	t.Fatalf("frame %q not received within %s", want, d)
	return Envelope{}
}
