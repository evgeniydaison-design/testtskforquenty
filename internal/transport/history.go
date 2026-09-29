package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"board/internal/hub"
	"board/internal/room"
)

// HistoryHandler — GET /rooms/{id}/history?from=N&to=M.
//
// Возвращает JSON-массив последних записей из in-memory history-кольца
// комнаты. Это «debug/audit» эндпоинт: UI на Этапе 5 его НЕ использует,
// но он полезен при разборе «почего правка пропала» и закладывается
// под Этап 6 (инкрементальный sync по lastSeq).
//
// Комната ищется через hub.Peek, а НЕ GetOrCreate: GET не должен
// порождать actora. Если комнаты нет — 404.
//
// Query-параметры:
//   - from — нижняя граница Seq (inclusive), 0 = без нижней;
//   - to   — верхняя граница Seq (inclusive), 0 = без верхней.
//
// Ответ: 200 { "room": "...", "current": <seq>, "records": [Record...] }
//
// Текущий seq — «r.current»: то, что реально лежит в кольце. Если
// from/to шире, чем содержимое, вернётся усечённый список.
type historyResponse struct {
	RoomID  string        `json:"room"`
	Current uint64        `json:"current"`
	Records []room.Record `json:"records"`
}

// historyTimeout — сколько ждём dispatch-a. Read-only операция,
// должна выполняться мгновенно. Если комната упёрлась в долгий
// handler (store-ошибка с ретраями и т.п.), мы не хотим вешать
// HTTP-req на минуты.
const historyTimeout = 2 * time.Second

func HistoryHandler(h *hub.Hub, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "room id required", http.StatusBadRequest)
			return
		}
		rr := h.Peek(id)
		if rr == nil {
			http.Error(w, "room not found", http.StatusNotFound)
			return
		}
		from, err := parseSeq(r.URL.Query().Get("from"))
		if err != nil {
			http.Error(w, "bad from: "+err.Error(), http.StatusBadRequest)
			return
		}
		to, err := parseSeq(r.URL.Query().Get("to"))
		if err != nil {
			http.Error(w, "bad to: "+err.Error(), http.StatusBadRequest)
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), historyTimeout)
		defer cancel()
		recs, err := rr.History(ctx, from, to)
		if err != nil {
			// ErrRoomStopped vs ctx deadline — оба «503»: комната
			// физически не может ответить в этом процессе.
			switch {
			case errors.Is(err, room.ErrRoomStopped):
				http.Error(w, "room stopped", http.StatusServiceUnavailable)
			case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
				http.Error(w, "timeout", http.StatusGatewayTimeout)
			default:
				log.Error("history: query failed", "room", id, "err", err)
				http.Error(w, "internal", http.StatusInternalServerError)
			}
			return
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		body := historyResponse{RoomID: id, Records: recs}
		if len(recs) > 0 {
			body.Current = recs[len(recs)-1].Seq
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			log.Error("history: encode failed", "room", id, "err", err)
		}
	})
}

// parseSeq — пустая строка = 0 (без границы). Остальное — uint64.
func parseSeq(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}
