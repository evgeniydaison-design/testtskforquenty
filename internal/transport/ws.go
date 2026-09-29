package transport

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"nhooyr.io/websocket"

	"board/internal/hub"
	"board/internal/presence"
	"board/internal/room"
)

// Ограничения по умолчанию. В Этапе 8 их задаёт конфиг + rate limiter.
const (
	// 8 MiB: вставки картинок из буфера едут dataURL-ом прямо в ops —
	// скриншот среднего монитора в PNG легко превышает старые 1 MiB, и
	// вставка «молча» отбивалась разрывом соединения.
	DefaultMaxMessageBytes int64 = 8 << 20 // 8 MiB
	DefaultSendBuf                 = 256
	DefaultWriteTimeout            = 10 * time.Second
	DefaultPingInterval            = 30 * time.Second
)

// Handler — HTTP-хендлер, монтируется на /room/{id}.
//
// Зависимости внедряются через конструктор. Интерфейсов не вводим: hub.Hub
// конкретный, подмену делаем только в тестах (реальный in-process сервер).
type Handler struct {
	hub      *hub.Hub
	log      *slog.Logger
	serverID string

	sendBuf        int
	maxMessage     int64
	writeTimeout   time.Duration
	pingInterval   time.Duration
	allowAllOrigin bool
}

// Options — функциональные опции, чтобы main.go читался ровно.
type Option func(*Handler)

func WithSendBuf(n int) Option          { return func(h *Handler) { h.sendBuf = n } }
func WithMaxMessage(n int64) Option     { return func(h *Handler) { h.maxMessage = n } }
func WithWriteTimeout(d time.Duration) Option {
	return func(h *Handler) { h.writeTimeout = d }
}
func WithPingInterval(d time.Duration) Option {
	return func(h *Handler) { h.pingInterval = d }
}
func WithAllowAllOrigins(v bool) Option { return func(h *Handler) { h.allowAllOrigin = v } }

// NewHandler собирает обработчик. serverID — человекочитаемая метка
// процесса; клиент использует её для логов.
func NewHandler(h *hub.Hub, serverID string, log *slog.Logger, opts ...Option) *Handler {
	if log == nil {
		log = slog.Default()
	}
	handler := &Handler{
		hub:          h,
		log:          log,
		serverID:     serverID,
		sendBuf:      DefaultSendBuf,
		maxMessage:   DefaultMaxMessageBytes,
		writeTimeout: DefaultWriteTimeout,
		pingInterval: DefaultPingInterval,
	}
	for _, o := range opts {
		o(handler)
	}
	return handler
}

// ServeHTTP — точка входа chi-маршрута /room/{id}.
//
// Жизненный цикл одного соединения:
//  1. accept → handshake (hello/welcome/join) синхронно в этом горуине;
//  2. spawn writer goroutine — она читает client.Send и пишет в сокет;
//  3. этот горуин становится reader'ом — читает сокете, шлёт команды в room;
//  4. когда reader выходит (клиент отключился, ошибка, bye), cancel writer;
//  5. defer Leave(client) — комната забудет клиента.
//
// Никаких мьютексов на уровне транспорта: всё взаимодействие через каналы.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomID := chi.URLParam(r, "id")
	if roomID == "" {
		http.Error(w, "missing room id", http.StatusBadRequest)
		return
	}

	// ctx привязан к запросу: когда http.Server.Shutdown завершит запрос,
	// ctx отменится и наши reader/writer выйдут.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	accept := &websocket.AcceptOptions{
		// В dev пропускаем любой origin (Vite :5173 → server :8080).
		// В prod конфигурируется отдельно (Этап 8 — AllowedOrigins).
		InsecureSkipVerify: h.allowAllOrigin,
	}
	conn, err := websocket.Accept(w, r, accept)
	if err != nil {
		h.log.Warn("ws accept failed", "err", err, "room", roomID)
		return
	}
	// CloseNow — гарантия, что горутина не зависнет на «холодном» сокете,
	// если writer/reader выйдут раньше, чем peer закроет соединение.
	defer conn.CloseNow()
	conn.SetReadLimit(h.maxMessage)

	cl, actor, err := h.handshake(ctx, conn, roomID)
	if err != nil {
		h.log.Info("handshake failed", "err", err, "room", roomID)
		// Close с StatusPolicyViolation: семантика «некорректное поведение клиента».
		_ = conn.Close(websocket.StatusPolicyViolation, "handshake failed")
		return
	}
	// При выходе из ServeHTTP оставляем комнату в обязательном порядке.
	// ctx у Leave — background: даже если основной ctx уже отменён,
	// команда должна дойти.
	defer func() {
		lctx, lcancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer lcancel()
		_ = actor.Leave(lctx, cl.ID)
	}()

	wctx, wcancel := context.WithCancel(ctx)
	werr := make(chan error, 1)
	go func() {
		werr <- h.writeLoop(wctx, conn, cl)
	}()

	// heartbeat — server-originated ping, чтобы не держать «мёртвые»
	// сокеты, которые клиент уже забыл. nhooyr сам отвечает pong'ом на
	// control-frame ping, но нам надо периодичность — отдельная горуина.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		h.heartbeat(wctx, conn)
	}()

	rerr := h.readLoop(ctx, conn, cl, actor)

	wcancel()
	<-werr
	<-hbDone

	closeCode := websocket.StatusNormalClosure
	reason := ""
	if rerr != nil && !isCleanExit(rerr) {
		closeCode = websocket.StatusGoingAway
		reason = "reader exit"
	}
	_ = conn.Close(closeCode, reason)
}

// handshake — разбор hello + welcome + join. Пишет напрямую в conn (до
// запуска writer-горутины), поэтому блокировки Send нет.
func (h *Handler) handshake(ctx context.Context, conn *websocket.Conn, roomID string) (*room.Client, *room.Room, error) {
	_, data, err := conn.Read(ctx)
	if err != nil {
		return nil, nil, fmtWrap("read hello", err)
	}
	env, err := decodeEnvelope(data)
	if err != nil {
		return nil, nil, err
	}
	if env.Type != TypeHello {
		return nil, nil, errors.New("transport: first frame must be hello, got " + env.Type)
	}
	if env.Proto != "" && env.Proto != ProtoV1 {
		return nil, nil, errors.New("transport: unsupported proto " + env.Proto)
	}
	var hp HelloPayload
	if err := decodePayload(env.Payload, &hp); err != nil {
		return nil, nil, err
	}
	if hp.ClientID == "" {
		return nil, nil, errors.New("transport: hello requires clientId")
	}

	// Welcome — сразу, чтобы клиент знал serverID до прихода снапшота.
	welcome, err := marshalEnvelope(TypeWelcome, WelcomePayload{
		ServerID: h.serverID, Proto: ProtoV1,
	})
	if err != nil {
		return nil, nil, err
	}
	if err := conn.Write(ctx, websocket.MessageText, welcome); err != nil {
		return nil, nil, fmtWrap("write welcome", err)
	}

	actor := h.hub.GetOrCreate(roomID)
	if actor == nil {
		return nil, nil, errors.New("transport: hub stopped")
	}
	cl := room.NewClient(hp.ClientID, hp.Name, hp.Color, h.sendBuf)
	cl.Password = hp.Password
	if err := actor.Join(ctx, cl); err != nil {
		// Пытаемся донести причину до клиента до закрытия. Для пароля —
		// отдельный код, чтобы UI показал именно поле пароля, а не
		// «комната занята».
		code := "join_failed"
		if errors.Is(err, room.ErrWrongPassword) {
			code = "room_password"
		}
		ebuf, _ := marshalEnvelope(TypeError, ErrorPayload{Code: code, Message: err.Error()})
		if len(ebuf) > 0 {
			_ = conn.Write(context.Background(), websocket.MessageText, ebuf)
		}
		cl.Close()
		return nil, nil, fmtWrap("join", err)
	}
	return cl, actor, nil
}

// readLoop — читает кадры из сокета, разбирает envelope, шлёт команды
// в комнату. Все ошибки декодирования — «soft», соединение живёт дальше;
// только сетевые/ctx-ошибки завершают цикл.
func (h *Handler) readLoop(ctx context.Context, conn *websocket.Conn, cl *room.Client, actor *room.Room) error {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		env, err := decodeEnvelope(data)
		if err != nil {
			h.pushError(cl, "bad_frame", err.Error())
			continue
		}
		switch env.Type {
		case TypeOps:
			var p OpsPayload
			if err := decodePayload(env.Payload, &p); err != nil {
				h.pushError(cl, "bad_ops", err.Error())
				continue
			}
			if p.BatchID == "" {
				h.pushError(cl, "bad_ops", "batchId required")
				continue
			}
			if err := actor.ApplyOps(ctx, cl.ID, p.BatchID, p.Ops); err != nil {
				return fmtWrap("apply ops", err)
			}
		case TypePing:
			var p PingPayload
			_ = decodePayload(env.Payload, &p)
			pong, _ := marshalEnvelope(TypePong, PongPayload{T: p.T})
			h.tryPush(cl, pong)
		case TypeBye:
			return io.EOF
		case TypePresence:
			var st presence.State
			if err := decodePayload(env.Payload, &st); err != nil {
				h.pushError(cl, "bad_presence", err.Error())
				continue
			}
			// Presence — fire-and-forget. Ошибку доставки не считаем
			// критичной: «потерянный» кадр = моргнувший курсор.
			if err := actor.SubmitPresence(ctx, cl.ID, st); err != nil {
				return fmtWrap("submit presence", err)
			}
		case TypeHello:
			// Повторный hello: на Этапе 1 не поддерживаем переподключение
			// в рамках одного сокета — клиенту проще закрыться и открыться заново.
			h.pushError(cl, "unexpected_hello", "hello already processed")
		default:
			h.pushError(cl, "unknown_type", env.Type)
		}
	}
}

// writeLoop — единственный потребитель cl.Send для этого соединения.
// Пишет с ограничением по времени, чтобы зависший TCP не прижал writer.
func (h *Handler) writeLoop(ctx context.Context, conn *websocket.Conn, cl *room.Client) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cl.Done:
			return nil
		case payload := <-cl.Send:
			wctx, cancel := context.WithTimeout(ctx, h.writeTimeout)
			err := conn.Write(wctx, websocket.MessageText, payload)
			cancel()
			if err != nil {
				return err
			}
		}
	}
}

// heartbeat — периодические control-ping от сервера. nhooyr сам обрабатывает
// pong-и, нам достаточно просто дёргать Ping(). Когда соединение умрёт,
// ping вернёт ошибку, и мы выйдем; readLoop тоже выйдет следом.
func (h *Handler) heartbeat(ctx context.Context, conn *websocket.Conn) {
	if h.pingInterval <= 0 {
		return
	}
	t := time.NewTicker(h.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pctx, cancel := context.WithTimeout(ctx, h.writeTimeout)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}

func (h *Handler) pushError(cl *room.Client, code, msg string) {
	b, err := marshalEnvelope(TypeError, ErrorPayload{Code: code, Message: msg})
	if err != nil {
		h.log.Error("marshal error frame", "err", err)
		return
	}
	h.tryPush(cl, b)
}

// tryPush — non-blocking запись в Send. Если буфер полон, клиент уже
// «на грани» — комната выкинет его на ближайшем broadcast. Не роняем
// соединение здесь: решение за актором.
func (h *Handler) tryPush(cl *room.Client, b []byte) {
	select {
	case cl.Send <- b:
	default:
	}
}

func isCleanExit(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return true
	}
	var cerr websocket.CloseError
	if errors.As(err, &cerr) {
		switch cerr.Code {
		case websocket.StatusNormalClosure, websocket.StatusGoingAway:
			return true
		}
	}
	return false
}

// fmtWrap — короткий helper, чтобы не засорять импорты fmt во всех хелперах.
func fmtWrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return errors.New("transport: " + what + ": " + err.Error())
}
