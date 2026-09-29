package transport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
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
)

// historyServer — hub + chi-mux, раздающий /room/{id} (WS) и
// /rooms/{id}/history (HTTP GET). Возвращает ws-URL и http-URL.
type historyServer struct {
	hs     *httptest.Server
	h      *hub.Hub
	wsURL  string
	cancel context.CancelFunc
}

func newHistoryServer(t *testing.T) *historyServer {
	t.Helper()
	log := slog.New(slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx, cancel := context.WithCancel(context.Background())
	codec := NewJSONCodec()
	cfg := room.DefaultConfig()
	cfg.WalFlushTick = 10 * time.Millisecond
	h := hub.New(ctx, codec, cfg, log)
	wsH := NewHandler(h, "srv", log, WithAllowAllOrigins(true), WithPingInterval(time.Hour))
	mux := chi.NewMux()
	mux.Handle("/room/{id}", wsH)
	mux.Handle("/rooms/{id}/history", HistoryHandler(h, log))
	hs := httptest.NewServer(mux)
	s := &historyServer{
		hs:     hs,
		h:      h,
		wsURL:  "ws" + strings.TrimPrefix(hs.URL, "http"),
		cancel: cancel,
	}
	t.Cleanup(func() {
		hs.Close()
		sctx, scancel := context.WithTimeout(context.Background(), time.Second)
		defer scancel()
		_ = h.Shutdown(sctx)
		s.cancel()
	})
	return s
}

// httpGet — короткий хелпер для проверки эндпоинта.
func httpGet(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec // тест-локальный httptest
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, body
}

func TestHistoryEndpoint_ReturnsAppliedBatches(t *testing.T) {
	s := newHistoryServer(t)

	a := dial(t, s.wsURL, "hdemo", "alice")
	a.readEnvelope(time.Second) // snapshot
	a.write(TypeOps, OpsPayload{BatchID: "b1", Ops: []crdt.Op{
		rectOp("e1", "alice", 1000, 10, 20, 30, 40),
	}})
	waitAck(t, a, "b1", 2*time.Second)
	a.write(TypeOps, OpsPayload{BatchID: "b2", Ops: []crdt.Op{
		rectOp("e2", "alice", 2000, 1, 2, 3, 4),
	}})
	waitAck(t, a, "b2", 2*time.Second)

	code, body := httpGet(t, s.hs.URL+"/rooms/hdemo/history")
	require.Equal(t, http.StatusOK, code, string(body))
	var hr struct {
		Room    string `json:"room"`
		Current uint64 `json:"current"`
		Records []struct {
			Seq    uint64 `json:"seq"`
			Origin string `json:"origin"`
			Ops    []struct {
				ID   string `json:"id"`
				Kind string `json:"kind"`
			} `json:"ops"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal(body, &hr))
	assert.Equal(t, "hdemo", hr.Room)
	assert.EqualValues(t, 2, hr.Current)
	require.Len(t, hr.Records, 2)
	assert.EqualValues(t, 1, hr.Records[0].Seq)
	assert.Equal(t, "alice", hr.Records[0].Origin)
	assert.Equal(t, "e1", hr.Records[0].Ops[0].ID)
	assert.Equal(t, "e2", hr.Records[1].Ops[0].ID)
}

func TestHistoryEndpoint_RangeFilter(t *testing.T) {
	s := newHistoryServer(t)

	a := dial(t, s.wsURL, "hrange", "alice")
	a.readEnvelope(time.Second)
	for i := 1; i <= 5; i++ {
		id := "e" + string(rune('0'+i))
		a.write(TypeOps, OpsPayload{BatchID: id, Ops: []crdt.Op{
			rectOp(id, "alice", int64(1000*i), 1, 1, 1, 1),
		}})
		waitAck(t, a, id, 2*time.Second)
	}

	// from=2&to=4 → seq 2, 3, 4.
	code, body := httpGet(t, s.hs.URL+"/rooms/hrange/history?from=2&to=4")
	require.Equal(t, http.StatusOK, code, string(body))
	var hr struct {
		Records []struct {
			Seq uint64 `json:"seq"`
		} `json:"records"`
	}
	require.NoError(t, json.Unmarshal(body, &hr))
	require.Len(t, hr.Records, 3)
	assert.EqualValues(t, 2, hr.Records[0].Seq)
	assert.EqualValues(t, 4, hr.Records[2].Seq)

	// from=10 → пусто.
	code, body = httpGet(t, s.hs.URL+"/rooms/hrange/history?from=10")
	require.Equal(t, http.StatusOK, code)
	hr.Records = nil
	require.NoError(t, json.Unmarshal(body, &hr))
	assert.Empty(t, hr.Records)
}

func TestHistoryEndpoint_UnknownRoom404(t *testing.T) {
	s := newHistoryServer(t)
	code, _ := httpGet(t, s.hs.URL+"/rooms/nonexistent/history")
	assert.Equal(t, http.StatusNotFound, code,
		"unknown room → 404, и НЕ должно создать комнату через Peek")
	// Убедимся, что hub.Rooms() не пополнился.
	_, body := httpGet(t, s.hs.URL+"/rooms/nonexistent/history")
	_ = body
	assert.NotContains(t, s.h.Rooms(), "nonexistent")
}

func TestHistoryEndpoint_BadQuery(t *testing.T) {
	s := newHistoryServer(t)
	a := dial(t, s.wsURL, "hbad", "alice")
	a.readEnvelope(time.Second)
	a.write(TypeOps, OpsPayload{BatchID: "b", Ops: []crdt.Op{
		rectOp("e", "alice", 1000, 1, 1, 1, 1),
	}})
	waitAck(t, a, "b", 2*time.Second)

	code, body := httpGet(t, s.hs.URL+"/rooms/hbad/history?from=abc")
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, string(body), "bad from")
}
