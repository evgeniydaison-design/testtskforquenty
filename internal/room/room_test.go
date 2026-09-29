package room

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"

	"board/internal/crdt"
	"board/internal/presence"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// У тестов нет «фоновых» горутин, поэтому goleak строгий: если комната
	// утечёт после теста — мы это увидим.
	goleak.VerifyTestMain(m)
}

// --- fake codec ---

type fakeCodec struct{ fail error }

func (f fakeCodec) MarshalSnapshot(seq uint64, els []crdt.Element) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return []byte("snap@" + itoaU64(seq) + "#" + strconv.Itoa(len(els))), nil
}
func (f fakeCodec) MarshalOps(seq uint64, origin string, ops []crdt.Op) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return []byte("ops@" + itoaU64(seq) + "/" + origin + "#" + strconv.Itoa(len(ops))), nil
}
func (f fakeCodec) MarshalAck(batchID string, seq uint64) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	return []byte("ack:" + batchID + "@" + itoaU64(seq)), nil
}
func (f fakeCodec) MarshalError(code, msg string) ([]byte, error) { return []byte("err:" + code), nil }

// MarshalPresence возвращает «pr|<entries>|<n>», где каждая запись —
// <id>:<x>,<y>:<rm:1|0>. Это даёт читаемые assertion'ы в тестах.
func (f fakeCodec) MarshalPresence(entries []presence.Entry) ([]byte, error) {
	if f.fail != nil {
		return nil, f.fail
	}
	var sb strings.Builder
	sb.WriteString("pr|")
	for i, e := range entries {
		if i > 0 {
			sb.WriteByte(',')
		}
		x, y := 0.0, 0.0
		if e.State.Cursor != nil {
			x, y = e.State.Cursor.X, e.State.Cursor.Y
		}
		rm := "0"
		if e.Removed {
			rm = "1"
		}
		sb.WriteString(e.ClientID + ":" + strconv.FormatFloat(x, 'f', -1, 64) +
			"," + strconv.FormatFloat(y, 'f', -1, 64) + ":" + rm)
	}
	sb.WriteString("|" + strconv.Itoa(len(entries)))
	return []byte(sb.String()), nil
}

func itoaU64(v uint64) string { return strconv.FormatUint(v, 10) }

// --- helpers ---

func newRoom(t *testing.T, cfg Config, codec Codec) (*Room, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := New("test-room", codec, cfg, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	go r.Run(ctx)
	t.Cleanup(func() { cancel(); <-r.Done() })
	return r, cancel
}

// newFastPresenceRoom — комната с коротким тиком и таймаутом, чтобы
// тесты не спали по 5 секунд.
func newFastPresenceRoom(t *testing.T) *Room {
	t.Helper()
	cfg := Config{BufSize: 64, CmdBuf: 32, PresenceTick: 10 * time.Millisecond, PresenceTimeout: 100 * time.Millisecond}
	r, _ := newRoom(t, cfg, fakeCodec{})
	return r
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func up(id string, v uint64) crdt.Op {
	return crdt.Op{
		Kind: crdt.OpUpsert,
		ID:   id,
		TS:   crdt.HLC{Wall: int64(v * 1000), Logical: 0, Node: "whoever"},
		Type: crdt.TypeRect,
		Props: map[string]any{
			crdt.PropX: 10.0,
			crdt.PropY: 10.0,
			crdt.PropW: 10.0,
			crdt.PropH: 10.0,
		},
	}
}

func readOne(t *testing.T, cl *Client, timeout time.Duration) []byte {
	t.Helper()
	select {
	case b := <-cl.Send:
		return b
	case <-cl.Done:
		t.Fatalf("client %s closed while waiting for message", cl.ID)
		return nil
	case <-time.After(timeout):
		t.Fatalf("timeout waiting for message to %s", cl.ID)
		return nil
	}
}

// waitForPoll — повторяет predicate до дедлайна. Нужен потому, что presence
// приходит не моментально, а на ближайшем тике.
func waitForPoll(t *testing.T, timeout time.Duration, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pred() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("predicate did not become true within timeout")
}

// --- tests ---

func TestRoom_JoinDeliversSnapshot(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})

	// Сначала «наполним» документ без клиента — через ApplyOps «из ниоткуда».
	require.NoError(t, r.ApplyOps(context.Background(), "ghost", "b1",
		[]crdt.Op{up("a", 1), up("b", 2)}))

	cl := NewClient("c1", "Alice", "#ff0000", 8)
	require.NoError(t, r.Join(context.Background(), cl))

	// Пришло ровно одно сообщение — снапшот с двумя живыми элементами.
	msg := readOne(t, cl, time.Second)
	assert.Equal(t, "snap@1#2", string(msg))
}

func TestRoom_JoinRejectsDuplicate(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()

	require.NoError(t, r.Join(ctx, NewClient("dup", "one", "#111", 4)))
	err := r.Join(ctx, NewClient("dup", "two", "#222", 4))
	assert.ErrorIs(t, err, ErrDuplicateClient)
}

// Тесты пароль-гейта (модалка «добровольного входа» на фронте).

func TestRoom_CreatorSetsPasswordOnEmptyRoom(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()

	creator := NewClient("c1", "Alice", "#111", 4)
	creator.Password = "s3cr3t"
	require.NoError(t, r.Join(ctx, creator))

	// Без пароля — не пустят, и снапшот чужому не уйдёт.
	intruder := NewClient("c2", "Eve", "#222", 4)
	assert.ErrorIs(t, r.Join(ctx, intruder), ErrWrongPassword)
	assert.Empty(t, intruder.Send)

	// С верным — пустят.
	guest := NewClient("c3", "Bob", "#333", 4)
	guest.Password = "s3cr3t"
	require.NoError(t, r.Join(ctx, guest))
}

func TestRoom_PublicRoomStaysOpen(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()

	// Первый зашёл без пароля — комната открыта.
	require.NoError(t, r.Join(ctx, NewClient("c1", "Ann", "#111", 4)))
	// Второй — тоже без пароля, живой комнату никто не «запирает».
	late := NewClient("c2", "Ben", "#222", 4)
	late.Password = "try-lock"
	require.NoError(t, r.Join(ctx, late))
	// И полностью без пароля — пускаем.
	require.NoError(t, r.Join(ctx, NewClient("c3", "Cid", "#333", 4)))
}

func TestRoom_WrongPasswordNeverLocks(t *testing.T) {
	// Неверный пароль при запертой комнате НЕ должен «переписывать»
	// ничего в состоянии комнаты: reject — это read-only исход.
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()

	creator := NewClient("c1", "Ann", "#111", 4)
	creator.Password = "right"
	require.NoError(t, r.Join(ctx, creator))

	for _, bad := range []string{"", "wrong", "RIGHT"} {
		cl := NewClient("c-"+bad, "Eve", "#222", 4)
		cl.Password = bad
		assert.ErrorIs(t, r.Join(ctx, cl), ErrWrongPassword)
	}
	// Верный по-прежнему работает.
	ok := NewClient("c-ok", "Bob", "#333", 4)
	ok.Password = "right"
	require.NoError(t, r.Join(ctx, ok))
}

func TestRoom_BroadcastToOtherClientsAndAckToOrigin(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()

	origin := NewClient("origin", "O", "#111", 16)
	other := NewClient("other", "P", "#222", 16)
	require.NoError(t, r.Join(ctx, origin))
	require.NoError(t, r.Join(ctx, other))

	// Сбрасываем снапшоты.
	readOne(t, origin, time.Second)
	readOne(t, other, time.Second)

	require.NoError(t, r.ApplyOps(ctx, origin.ID, "b1", []crdt.Op{up("x", 1)}))

	// Origin получает И broadcast, И ack. Other получает только broadcast.
	// Порядок между broadcast/ack для origin не фиксирован — соберём оба.
	gotOrigin := drain(t, origin, 2, time.Second)
	gotOther := drain(t, other, 1, time.Second)

	assert.Equal(t, []string{"ops@1/origin#1", "ack:b1@1"}, gotOrigin)
	assert.Equal(t, []string{"ops@1/origin#1"}, gotOther)
}

func TestRoom_IgnoreNoOpApplyStillAck(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()
	o := NewClient("o", "O", "#111", 8)
	require.NoError(t, r.Join(ctx, o))
	readOne(t, o, time.Second)

	// Отправим ops с возрастающим TS (чтобы второй не проиграл первому).
	require.NoError(t, r.ApplyOps(ctx, o.ID, "b1", []crdt.Op{up("z", 5)}))
	drain(t, o, 2, time.Second) // ops + ack
	// Тот же TS — сервер нормализует его через clock.Observe/Now, но
	// doc уже содержит этот «ключ», повтор идемпотентен. Проверяем,
	// что ack приходит даже при applied=[] (нет новых применённых ops).
	require.NoError(t, r.ApplyOps(ctx, o.ID, "b2", []crdt.Op{up("z", 5)}))
	// applied=1, потому что серверный TS стал строго больше прежнего.
	msgs := drain(t, o, 2, time.Second)
	require.Len(t, msgs, 2)
	assert.Contains(t, msgs[0], "ops@")
	assert.Contains(t, msgs[1], "ack:b2@")
}

func TestRoom_SlowClientIsKicked(t *testing.T) {
	r, _ := newRoom(t, Config{BufSize: 2, CmdBuf: 8, PresenceTick: 50 * time.Millisecond, PresenceTimeout: 5 * time.Second}, fakeCodec{})
	ctx := context.Background()

	slow := NewClient("slow", "S", "#111", 2)
	fast := NewClient("fast", "F", "#222", 64)
	require.NoError(t, r.Join(ctx, slow))
	require.NoError(t, r.Join(ctx, fast))
	// Снапшоты — 0 элементов, но они занимают по слоту в буфере.
	readOne(t, slow, time.Second)
	readOne(t, fast, time.Second)

	// Шлём 5 батчей подряд; slow буферит 2, остальные три должны привести к
	// выкидыванию. После выкидывания slow.Send закрыт через Done.
	for i := 1; i <= 5; i++ {
		require.NoError(t, r.ApplyOps(ctx, fast.ID, "b"+strconv.Itoa(i),
			[]crdt.Op{up("e"+strconv.Itoa(i), uint64(i))}))
		// Дайте актору обработать.
		time.Sleep(10 * time.Millisecond)
	}

	// Быстрый должен получить ВСЕ 5 батчей.
	msgs := drain(t, fast, 10, time.Second) // 5 ops + 5 acks
	assert.Len(t, msgs, 10)

	// Медленный получил максимум 2 сообщения (буфер), потом Done закрыт.
	select {
	case <-slow.Done:
		// expected
	default:
		t.Fatal("slow client must be closed by now")
	}
}

func TestRoom_LeaveIsIdempotent(t *testing.T) {
	r, _ := newRoom(t, DefaultConfig(), fakeCodec{})
	ctx := context.Background()
	cl := NewClient("c", "C", "#111", 4)
	require.NoError(t, r.Join(ctx, cl))
	require.NoError(t, r.Leave(ctx, cl.ID))
	require.NoError(t, r.Leave(ctx, cl.ID)) // второй — no-op
	// Ждём, пока актор обработает обе команды: Join синхронно упирается
	// в Reply, поэтому до него пройдут все ранее поставленные Leave.
	sync := NewClient("sync", "s", "#222", 1)
	require.NoError(t, r.Join(ctx, sync))
	select {
	case <-cl.Done:
	default:
		t.Fatal("Done must be closed after Leave")
	}
}

func TestRoom_ShutdownClosesAllClients(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := New("x", fakeCodec{}, DefaultConfig(), slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	go r.Run(ctx)

	c1 := NewClient("1", "one", "#111", 8)
	c2 := NewClient("2", "two", "#222", 8)
	require.NoError(t, r.Join(ctx, c1))
	require.NoError(t, r.Join(ctx, c2))
	// Сбросим снапшоты, чтобы каналы были свободны.
	readOne(t, c1, time.Second)
	readOne(t, c2, time.Second)

	cancel()
	<-r.Done()

	select {
	case <-c1.Done:
	case <-time.After(time.Second):
		t.Fatal("c1 not closed on shutdown")
	}
	select {
	case <-c2.Done:
	case <-time.After(time.Second):
		t.Fatal("c2 not closed on shutdown")
	}
}

func TestRoom_SubmitAfterStopped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := New("x", fakeCodec{}, DefaultConfig(), slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	go r.Run(ctx)
	cancel()
	<-r.Done()

	err := r.Join(ctx, NewClient("c", "c", "#111", 4))
	assert.True(t, errors.Is(err, ErrRoomStopped) || errors.Is(err, context.Canceled),
		"expected ErrRoomStopped/Canceled, got %v", err)
}

// --- presence tests ---

// Новый клиент при join получает snapshot И (если в комнате уже кто-то есть)
// presence-sync. Проверяем порядок: snapshot, затем presence.
func TestRoom_JoinSendsExistingPresenceToNewcomer(t *testing.T) {
	r := newFastPresenceRoom(t)
	ctx := context.Background()

	a := NewClient("a", "Alice", "#aaa", 16)
	require.NoError(t, r.Join(ctx, a))
	readOne(t, a, time.Second) // snapshot
	// Alice шлёт курсор, ждём, пока тик его «выплюнет».
	require.NoError(t, r.SubmitPresence(ctx, a.ID, presence.State{Cursor: &presence.Point{X: 10, Y: 20}}))
	waitForPoll(t, time.Second, func() bool {
		// У a пришло как минимум ещё presence-сообщение (self-broadcast).
		return tryRead(a) != nil
	})

	b := NewClient("b", "Bob", "#bbb", 16)
	require.NoError(t, r.Join(ctx, b))
	snap := readOne(t, b, time.Second)
	assert.True(t, strings.HasPrefix(string(snap), "snap@"), "expected snapshot, got %s", snap)
	pmsg := readOne(t, b, time.Second)
	assert.Contains(t, string(pmsg), "pr|a:10,20:0|1", "expected presence-sync with alice")
}

// Coalescing: 3 кадра подряд от одного клиента = один broadcast с последним
// состоянием. Это ядро экономии трафика на Этапе 2.
func TestRoom_PresenceCoalescing(t *testing.T) {
	cfg := Config{BufSize: 64, CmdBuf: 32, PresenceTick: 100 * time.Millisecond, PresenceTimeout: 5 * time.Second}
	r, _ := newRoom(t, cfg, fakeCodec{})
	ctx := context.Background()

	a := NewClient("a", "A", "#aaa", 64)
	b := NewClient("b", "B", "#bbb", 64)
	require.NoError(t, r.Join(ctx, a))
	require.NoError(t, r.Join(ctx, b))
	readOne(t, a, time.Second) // snapshot
	readOne(t, b, time.Second) // snapshot
	// b мог получить ещё и presence-sync (a ещё не шлёт курсор) — но entries
	// пустой быть не должен, т.к. Alice без курсора.
	drain(t, a, 0, 10*time.Millisecond)
	drain(t, b, 0, 10*time.Millisecond)

	// 3 кадра подряд за одну «эпоху» тикера.
	for i := 1; i <= 3; i++ {
		require.NoError(t, r.SubmitPresence(ctx, a.ID, presence.State{Cursor: &presence.Point{X: float64(i), Y: 0}}))
	}

	msgs := drain(t, b, 1, time.Second)
	require.Len(t, msgs, 1, "expected exactly one broadcast")
	assert.Equal(t, "pr|a:3,0:0|1", string(msgs[0]), "last update wins, previous ones merged")
}

// Leave → removed-broadcast на следующем тике.
func TestRoom_LeaveBroadcastsPresenceRemoved(t *testing.T) {
	r := newFastPresenceRoom(t)
	ctx := context.Background()

	a := NewClient("a", "A", "#aaa", 16)
	b := NewClient("b", "B", "#bbb", 16)
	require.NoError(t, r.Join(ctx, a))
	require.NoError(t, r.Join(ctx, b))
	readOne(t, a, time.Second)
	readOne(t, b, time.Second)
	// drain any sync frames
	drain(t, a, 4, 50*time.Millisecond)
	drain(t, b, 4, 50*time.Millisecond)

	require.NoError(t, r.SubmitPresence(ctx, a.ID, presence.State{Cursor: &presence.Point{X: 5, Y: 5}}))
	// дождаться presence-рассылки на b (это же подтверждает, что тик работает)
	waitForPoll(t, time.Second, func() bool { return tryRead(b) != nil })

	require.NoError(t, r.Leave(ctx, a.ID))
	// Теперь ждём removed-broadcast.
	var got string
	waitForPoll(t, time.Second, func() bool {
		b := tryRead(b)
		if b == nil {
			return false
		}
		got = string(b)
		return strings.Contains(got, ":1") && strings.Contains(got, "a:")
	})
	assert.Contains(t, got, "a:5,5:1", "expected removed:true for alice (last cursor preserved), got "+got)
}

// PresenceTimeout должен автоматически evict «замолчавшего» участника.
func TestRoom_PresenceStaleEviction(t *testing.T) {
	cfg := Config{BufSize: 64, CmdBuf: 32, PresenceTick: 10 * time.Millisecond, PresenceTimeout: 50 * time.Millisecond}
	r, _ := newRoom(t, cfg, fakeCodec{})
	ctx := context.Background()

	a := NewClient("a", "A", "#aaa", 64)
	b := NewClient("b", "B", "#bbb", 64)
	require.NoError(t, r.Join(ctx, a))
	require.NoError(t, r.Join(ctx, b))
	readOne(t, a, time.Second)
	readOne(t, b, time.Second)
	drain(t, a, 4, 50*time.Millisecond)
	drain(t, b, 4, 50*time.Millisecond)

	require.NoError(t, r.SubmitPresence(ctx, a.ID, presence.State{Cursor: &presence.Point{X: 1, Y: 2}}))
	// дождаться первой presence-рассылки
	waitForPoll(t, time.Second, func() bool { return tryRead(b) != nil })
	// ждём stale-евикшн: последний LastSeen «a» — момент SubmitPresence.
	// через 50мс + 10мс тик должен появиться removed.
	var got string
	waitForPoll(t, time.Second, func() bool {
		m := tryRead(b)
		if m == nil {
			return false
		}
		got = string(m)
		return strings.Contains(got, "a:1,2:1")
	})
	assert.Contains(t, got, "a:1,2:1", "stale entry must be broadcast as removed")
}

// Presence от неизвестного клиента (уже ушёл) игнорируется, но не валит комнату.
func TestRoom_PresenceForUnknownClientIgnored(t *testing.T) {
	r := newFastPresenceRoom(t)
	ctx := context.Background()
	require.NoError(t, r.SubmitPresence(ctx, "ghost", presence.State{Cursor: &presence.Point{X: 1, Y: 1}}))
	// Sync-команда: Join нового клиента пройдёт только если актор жив.
	sync := NewClient("sync", "s", "#111", 4)
	require.NoError(t, r.Join(ctx, sync))
}

// --- benchmark: hot path ---

func BenchmarkRoomApplyOps(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("bench", fakeCodec{}, DefaultConfig(), slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	go r.Run(ctx)
	cl := NewClient("c", "c", "#111", 1024)
	_ = r.Join(ctx, cl)
	// drain
	go func() {
		for {
			select {
			case <-cl.Send:
			case <-cl.Done:
				return
			}
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.ApplyOps(ctx, cl.ID, "b", []crdt.Op{up("e", uint64(i+1))})
	}
	b.StopTimer()
	cancel()
	<-r.Done()
}

// BenchmarkRoomPresence — «хот-путь» coalescing: сколько presence-кадров/сек
//ROOM переваривает при тике 50 мс.
func BenchmarkRoomPresence(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("bench-pr", fakeCodec{}, Config{
		BufSize: 4096, CmdBuf: 4096,
		PresenceTick: 50 * time.Millisecond, PresenceTimeout: time.Hour,
		GCTick: 0,
	}, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	go r.Run(ctx)
	cl := NewClient("c", "c", "#111", 4096)
	_ = r.Join(ctx, cl)
	go func() {
		for {
			select {
			case <-cl.Send:
			case <-cl.Done:
				return
			}
		}
	}()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = r.SubmitPresence(ctx, cl.ID, presence.State{Cursor: &presence.Point{X: float64(i % 1000), Y: float64(i % 777)}})
	}
	b.StopTimer()
	cancel()
	<-r.Done()
}

// --- util: drain ---

func drain(t *testing.T, cl *Client, want int, timeout time.Duration) []string {
	t.Helper()
	deadline := time.After(timeout)
	out := make([]string, 0, want)
	for len(out) < want {
		select {
		case b := <-cl.Send:
			out = append(out, string(b))
		case <-cl.Done:
			return out
		case <-deadline:
			return out
		}
	}
	return out
}

// tryRead — неблокирующий read из Send. Возвращает nil, если пусто.
func tryRead(cl *Client) []byte {
	select {
	case b := <-cl.Send:
		return b
	default:
		return nil
	}
}
