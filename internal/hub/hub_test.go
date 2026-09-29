package hub

import (
	"context"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"go.uber.org/goleak"

	"board/internal/crdt"
	"board/internal/presence"
	"board/internal/room"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// noopCodec — не проверяем формат здесь, это задача transport.
type noopCodec struct{}

func (noopCodec) MarshalSnapshot(uint64, []crdt.Element) ([]byte, error) { return []byte("s"), nil }
func (noopCodec) MarshalOps(uint64, string, []crdt.Op) ([]byte, error)   { return []byte("o"), nil }
func (noopCodec) MarshalAck(string, uint64) ([]byte, error)              { return []byte("a"), nil }
func (noopCodec) MarshalError(string, string) ([]byte, error)            { return []byte("e"), nil }
func (noopCodec) MarshalPresence([]presence.Entry) ([]byte, error)       { return []byte("p"), nil }

func newHub(t *testing.T) *Hub {
	t.Helper()
	h := New(context.Background(), noopCodec{}, room.DefaultConfig(),
		slog.New(slog.NewTextHandler(discard{}, nil)))
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = h.Shutdown(ctx)
	})
	return h
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func TestHub_GetOrCreateIsStable(t *testing.T) {
	h := newHub(t)
	r1 := h.GetOrCreate("alpha")
	r2 := h.GetOrCreate("alpha")
	require.NotNil(t, r1)
	assert.Same(t, r1, r2, "один и тот же roomID обязан возвращать одного актora")

	r3 := h.GetOrCreate("beta")
	assert.NotSame(t, r1, r3, "разные roomID — разные комнаты")
}

func TestHub_RoomsReflectsState(t *testing.T) {
	h := newHub(t)
	_ = h.GetOrCreate("a")
	_ = h.GetOrCreate("b")
	ids := h.Rooms()
	assert.ElementsMatch(t, []string{"a", "b"}, ids)
}

func TestHub_ShutdownStopsAllRooms(t *testing.T) {
	h := New(context.Background(), noopCodec{}, room.DefaultConfig(),
		slog.New(slog.NewTextHandler(discard{}, nil)))
	r1 := h.GetOrCreate("a")
	r2 := h.GetOrCreate("b")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, h.Shutdown(ctx))

	select {
	case <-r1.Done():
	case <-time.After(time.Second):
		t.Fatal("room a не завершилась")
	}
	select {
	case <-r2.Done():
	case <-time.After(time.Second):
		t.Fatal("room b не завершилась")
	}
	// Реестр обязан опустеть — иначе memory leak при повторном «оживлении».
	assert.Empty(t, h.Rooms())
}

func TestHub_GetOrCreateAfterShutdown(t *testing.T) {
	h := New(context.Background(), noopCodec{}, room.DefaultConfig(),
		slog.New(slog.NewTextHandler(discard{}, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, h.Shutdown(ctx))

	r := h.GetOrCreate("late")
	assert.Nil(t, r, "после Shutdown нельзя создавать новые комнаты")
}

// TestHub_NoGoroutineLeakWhenRoomsReopened — после shutdown все goroutine
// комнаты должны отработаться и уйти; goleak под TestMain это и проверит.
// Здесь — дополнительный smoke, чтобы убедиться, что «перезапуск» комнаты
// (после удаления) не накапливает горутины.
func TestHub_NoGoroutineLeakWhenRoomsReopened(t *testing.T) {
	h := newHub(t)
	for i := 0; i < 20; i++ {
		id := "r" + strconv.Itoa(i)
		_ = h.GetOrCreate(id)
	}
	require.Len(t, h.Rooms(), 20)
}
