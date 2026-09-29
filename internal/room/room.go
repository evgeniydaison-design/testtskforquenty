package room

import (
	"context"
	"log/slog"
	"time"

	"board/internal/crdt"
	"board/internal/presence"
)

// Codec — сериализация исходящих сообщений. Интерфейс объявлен на стороне
// потребителя (комнаты): сам пакет room не знает ни про JSON, ни про
// WebSocket. Реализация живёт в internal/transport.
type Codec interface {
	MarshalSnapshot(seq uint64, elements []crdt.Element) ([]byte, error)
	MarshalOps(seq uint64, origin string, ops []crdt.Op) ([]byte, error)
	MarshalAck(batchID string, seq uint64) ([]byte, error)
	MarshalError(code, message string) ([]byte, error)
	MarshalPresence(entries []presence.Entry) ([]byte, error)
}

// Config — параметры актora.
type Config struct {
	// BufSize — ёмкость исходящего канала одного клиента. 256 сообщений
	// = примерно один RTT «слабы» при 30 Гц батчей; при переполнении
	// клиент выкидывается, чтобы не блокировать актора.
	BufSize int

	// CmdBuf — ёмкость канала команд. Маленькая: команды должны
	// обрабатываться быстро; большой буфер — признак, что актор тормозит.
	CmdBuf int

	// PresenceTick — период «склейки» presence-обновлений. Меньше 33 мс
	// — слишком часто для UI, больше 100 мс — заметна дёрганость.
	PresenceTick time.Duration

	// PresenceTimeout — если клиент молчит дольше этого времени, его
	// курсор удаляется. Нужен на случай «зависшего» TCP: соединение
	// живо, а мышь пользователь уже убрал.
	PresenceTimeout time.Duration

	// GCTick — как часто запускать сборку tombstone-ов. Час — безопасный
	// компромисс: doc.Live не должен «разбухать» на глазах, но и сама
	// операция O(N) не должна выполняться часто.
	GCTick time.Duration

	// TombstoneRetention — сколько хранить tombstone. Проще всего —
	// crdt.DefaultTombstoneRetention (30 дней).
	TombstoneRetention time.Duration

	// WalFlushTick — как часто дёргать Flush() у Store. 50 мс: потеря
	// последнего «маха» при краше — разумная цена за 20× ускорение
	// против «fsync на каждый op».
	WalFlushTick time.Duration

	// SnapshotEvery — через сколько применённых батчей писать полный
	// снапшот. 200 батчей ≈ 5–10 минут живой доски; recovery читает
	// максимум столько строк WAL.
	SnapshotEvery int

	// HistorySize — ёмкость in-memory history-кольца. Это НЕ per
	// размер (тот boundedWalFlushTick/SnapshotEvery), а «сколько
	// последних батчей отдавать на GET /history». Значение<=0
	// получает defaultHistorySize.
	HistorySize int
}

// DefaultConfig — безопасные значения по умолчанию.
func DefaultConfig() Config {
	return Config{
		BufSize:            256,
		CmdBuf:             64,
		PresenceTick:       50 * time.Millisecond,
		PresenceTimeout:    5 * time.Second,
		GCTick:             time.Hour,
		TombstoneRetention: crdt.DefaultTombstoneRetention,
		WalFlushTick:       50 * time.Millisecond,
		SnapshotEvery:      200,
		HistorySize:        defaultHistorySize,
	}
}

// Room — горутина-актор.
//
// Инварианты:
//   - doc, clients, seqN изменЯются ТОЛЬКО внутри Run-цикла;
//   - никаких мьютексов: гонки физически невозможны;
//   - ни один handler не блокируется на внешней IO — исходящие сообщения
//     кладутся в буферизованный канал клиента, transport пишет в сокет;
//   - при переполнении буфера клиента он выкидывается (см. sendTo).
type Room struct {
	id    string
	cfg   Config
	log   *slog.Logger
	codec Codec

	doc     *crdt.Doc
	clients map[string]*Client

	// password — гейт доступа комнаты. Мутируется/читается только
	// внутри Run-цикла (инвариант актora сохранён). Правила:
	//   • пустая строка — комната открыта (кто зашёл, тот и рисует);
	//   • задаётся ПЕРВЫМ клиентом при создании комнаты
	//     (no clients && seqN==0), если он принёс непустой пароль;
	//   • дальше — только точное совпадение при Join.
	// Ограничение: пароль живёт в памяти; рестарт сервера его сбрасывает
	// (в WAL/снапшот не пишется — это транспортный, а не документный
	// слой). Этап 8: перенос в конфиг/хранилище метаданных комнат.
	password string

	// seqN — монотонный счётчик «трансляций» сервера. Отстаётся на 1 для
	// каждого обработанного ApplyOpsCmd. Нужен клиенту для инкрементального
	// sync и для сопоставления ack. На Этапе 3 его вытеснит вектор версий.
	seqN uint64

	// presence — эфемерное состояние по clientId. Не входит в doc,
	// не идёт в WAL (Этап 4). presenceDirty — множество id, у которых
	// было изменение с прошлого тика. Разделение dirty/entries нужно,
	// чтобы «склеить» несколько обновлений одного участника в один
	// broadcast.
	presence      map[string]*presence.Entry
	presenceDirty map[string]bool

	// clock — HLC сервера. Нужен, чтобы нормализовать входящие ops в
	// «серверную» временну́ю шкалу: даже если клиентские часы врут,
	// Observe поднимает Wall до максимума, и последующий broadcast
	// имеет TS >= локального. Это сохраняет каузальность.
	clock *crdt.Clock

	// store — персистентность doc-мутаций. Может быть nil — тогда
	// комната живёт «в памяти» (dev/recovery-тесты). Write-path:
	//   • на каждый применённый батч → Append(record);
	//   • каждые WalFlushTick → Flush (fsync батчем);
	//   • каждые SnapshotEvery батчей → SaveSnapshot;
	//   • при ctx.Done / явном останове → Flush + Close.
	store Store
	// batchesSinceSnap — счётчик для SnapshotEvery.
	batchesSinceSnap int

	// hist — in-memory ring buffer последних HistorySize батчей.
	// Нужен для GET /rooms/{id}/history и для будущего инкрементального
	// sync (Этап 6). В отличие от store, hist всегда существует
	// (history не требует fsync). Write-путь один: handleApplyOps.
	hist *historyRing

	cmd  chan Command
	done chan struct{}
}

// Option — функциональные опции New. Нужны, чтобы не плодить
// конструкторы «New / NewWithStore / NewWithStoreAndLoaded / ...».
type Option func(*Room)

// WithStore подключает персистентность. Если store==nil — ничего не
// делаем (уже дефолт).
func WithStore(s Store) Option {
	return func(r *Room) {
		if s != nil {
			r.store = s
		}
	}
}

// WithLoaded восстанавливает doc из состояния, которое hub прочитал
// из Store.Load. Вызывается ДО Run: гонки нет, потому что r.doc
// до Run-цикла не трогается.
//
// Snapshot грузится через doc.Reset (полная замена), затем Records
// применяются один за другим. Это даёт идемпотентный «replay»: любой
// порядок записей с тем же Seq приводит к тому же doc, потому что
// CRDT-операции уже нормализованы сервером (TS строго возрастающий).
func WithLoaded(l *Loaded) Option {
	return func(r *Room) {
		if l == nil {
			return
		}
		if l.Snapshot != nil {
			r.doc.Reset(l.Snapshot.Elements)
			r.seqN = l.Snapshot.Seq
		}
		for _, rec := range l.Records {
			for _, op := range rec.Ops {
				r.doc.Apply(op)
			}
			if rec.Seq > r.seqN {
				r.seqN = rec.Seq
			}
			// Поднимаем HLC сервера до max(Wall), чтобы следующий
			// Now() был строго больше восстановленных TS.
			for _, op := range rec.Ops {
				r.clock.Observe(op.TS)
			}
		}
	}
}

// New конструирует комнату, но НЕ запускает её. Run вызывается отдельно —
// обычно из hub, под ctx, которым управляется жизненный цикл сервера.
func New(id string, codec Codec, cfg Config, log *slog.Logger, opts ...Option) *Room {
	if cfg.BufSize <= 0 {
		cfg.BufSize = 256
	}
	if cfg.CmdBuf <= 0 {
		cfg.CmdBuf = 64
	}
	if log == nil {
		log = slog.Default()
	}
	r := &Room{
		id:            id,
		cfg:           cfg,
		log:           log.With("room", id),
		codec:         codec,
		doc:           crdt.NewDoc(),
		clients:       make(map[string]*Client),
		presence:      make(map[string]*presence.Entry),
		presenceDirty: make(map[string]bool),
		clock:         crdt.NewClock("server:" + id),
		cmd:           make(chan Command, cfg.CmdBuf),
		done:          make(chan struct{}),
	}
	for _, o := range opts {
		o(r)
	}
	if r.hist == nil {
		r.hist = newHistoryRing(cfg.HistorySize)
	}
	return r
}

// ID — идентификатор комнаты (для логов и hub).
func (r *Room) ID() string { return r.id }

// Done — канал, закрывающийся, когда актор завершился. Полезен для
// hub-выгрузки и graceful shutdown.
func (r *Room) Done() <-chan struct{} { return r.done }

// Seq — текущий «номер трансляции». Для тестов и отладочных эндпоинтов.
func (r *Room) Seq() uint64 {
	// Чтение извне — race по факту. Но поле пишется только в Run-цикле,
	// а читается из тестов, которые синхронизированы по времени.
	// Для реального внешнего доступа нужен отдельной cmd. Оставим так,
	// в проде (main.go) мы Seq() не зовём.
	return r.seqN
}

// Doc — прямой доступ к документу. Только для тестов; в проде не трогаем.
func (r *Room) Doc() *crdt.Doc { return r.doc }

// Run — главный цикл. Возвращается при ctx.Done() или после явной остановки.
func (r *Room) Run(ctx context.Context) {
	defer close(r.done)
	// Таймер presence — отдельный, с малым периодом. Если PresenceTick<=0,
	// presence-тикер не участвует в select (nil-channel «вечный block»).
	var tickCh <-chan time.Time
	if r.cfg.PresenceTick > 0 {
		t := time.NewTicker(r.cfg.PresenceTick)
		defer t.Stop()
		tickCh = t.C
	}
	var gcCh <-chan time.Time
	if r.cfg.GCTick > 0 {
		g := time.NewTicker(r.cfg.GCTick)
		defer g.Stop()
		gcCh = g.C
	}
	// walCh — периодический fsync WAL. nil, если store не подключён
	// или WalFlushTick<=0.
	var walCh <-chan time.Time
	if r.store != nil && r.cfg.WalFlushTick > 0 {
		wf := time.NewTicker(r.cfg.WalFlushTick)
		defer wf.Stop()
		walCh = wf.C
	}
	for {
		select {
		case <-ctx.Done():
			r.shutdownAll()
			return
		case cmd := <-r.cmd:
			// ВАЖНО: dispatch никогда не блокируется. Все исходящие каналы
			// клиентские — non-blocking; Reply-каналы буферизованы (cap=1).
			r.dispatch(cmd)
		case <-tickCh:
			r.flushPresence()
		case <-gcCh:
			r.runGC()
		case <-walCh:
			r.flushWAL()
		}
	}
}

// dispatch — единственный место, где мутируется состояние комнаты.
func (r *Room) dispatch(cmd Command) {
	switch c := cmd.(type) {
	case JoinCmd:
		r.handleJoin(c)
	case LeaveCmd:
		r.handleLeave(c)
	case ApplyOpsCmd:
		r.handleApplyOps(c)
	case PresenceCmd:
		r.handlePresence(c)
	case HistoryCmd:
		r.handleHistory(c)
	}
}

// --- Public API: тонкие обёртки над Submit, чтобы не плодить select в
//     каждом месте вызова. Все они уважают ctx и завершение комнаты. ---

// Join добавляет клиента и гарантирует, что снапшот уже лежит в его Send.
func (r *Room) Join(ctx context.Context, cl *Client) error {
	reply := make(chan error, 1)
	select {
	case r.cmd <- JoinCmd{Client: cl, Reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return ErrRoomStopped
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return ErrRoomStopped
	}
}

// Leave — асинхронный «покинуть». Не ждём подтверждения: идемпотентно.
func (r *Room) Leave(ctx context.Context, clientID string) error {
	select {
	case r.cmd <- LeaveCmd{ClientID: clientID}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return ErrRoomStopped
	}
}

// ApplyOps — асинхронная передача батча. Transport не ждёт ack-а: ack
// придёт через Send-канал того же клиента.
func (r *Room) ApplyOps(ctx context.Context, from, batchID string, ops []crdt.Op) error {
	select {
	case r.cmd <- ApplyOpsCmd{From: from, BatchID: batchID, Ops: ops}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return ErrRoomStopped
	}
}

// SubmitPresence — обновление эфемерного состояния участника.
// Fire-and-forget: ack не шлём, потерянные кадры не критичны.
func (r *Room) SubmitPresence(ctx context.Context, clientID string, st presence.State) error {
	select {
	case r.cmd <- PresenceCmd{ClientID: clientID, State: st}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return ErrRoomStopped
	}
}

// History — синхронный запрос диапазона записей из history-кольца.
// Возвращает копию (срез новых Record-ов); вызывающий волен
// сериализовать его без риска data race.
//
// Относится к диспатчеру так же, как Join: через cmd-канал +
// буферизованный reply. Блокируется до dispatch-а или ctx/done.
func (r *Room) History(ctx context.Context, from, to uint64) ([]Record, error) {
	reply := make(chan []Record, 1)
	select {
	case r.cmd <- HistoryCmd{From: from, To: to, Reply: reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, ErrRoomStopped
	}
	select {
	case recs := <-reply:
		return recs, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.done:
		return nil, ErrRoomStopped
	}
}

// --- handlers ---

func (r *Room) handleJoin(c JoinCmd) {
	cl := c.Client
	// Парольный гейт — ДО регистрации клиента и ДО снапшота:
	// чужой не должен увидеть ни doc, ни presence-лист.
	if r.password != "" && cl.Password != r.password {
		c.Reply <- ErrWrongPassword
		return
	}
	if _, exists := r.clients[cl.ID]; exists {
		c.Reply <- ErrDuplicateClient
		return
	}
	// Создатель комнаты ставит замок: только на пустой ещё не живой
	// комнате, иначе случайный опоздавший не может «захватить» доступ.
	if r.password == "" && len(r.clients) == 0 && r.seqN == 0 && cl.Password != "" {
		r.password = cl.Password
	}
	r.clients[cl.ID] = cl

	// Presence-запись создаём сразу: другие участники увидят новичка
	// даже до того, как он пришлёт первый presence-кадр.
	now := time.Now()
	r.presence[cl.ID] = &presence.Entry{
		ClientID: cl.ID,
		Name:     cl.Name,
		Color:    cl.Color,
		LastSeen: now,
	}

	// Порядок отправки новичку: snapshot → presence-sync. Так клиент
	// сначала отрисует документ, потом накидает чужие курсоры; обратный
	// порядок давал бы «мерцающие» призраки на пустом холсте.
	snap := r.doc.Snapshot()
	payload, err := r.codec.MarshalSnapshot(r.seqN, snap)
	if err != nil {
		// Codec упал — не держим клиента, пусть транспорт закроется.
		r.log.Error("codec: snapshot failed", "err", err, "client", cl.ID)
		delete(r.clients, cl.ID)
		delete(r.presence, cl.ID)
		c.Reply <- ErrCodecFailure
		return
	}
	// Для свежего клиента буфер Send пуст, non-blocking send обязан пройти.
	// Всё равно делаем select — на случай, если транспорт уже закрыл Done.
	select {
	case cl.Send <- payload:
	default:
		delete(r.clients, cl.ID)
		delete(r.presence, cl.ID)
		c.Reply <- ErrRoomStopped
		return
	}

	// Presence-sync: шлём только «живые» записи — у которых уже есть
	// курсор или инструмент. Zero-state (кто зашёл и ничего не делал)
	// не отправляем: presence — это про курсоры, а не про «кто онлайн»;
	// online-список — отдельная задача (свг. Этап 2.5/roster).
	if len(r.presence) > 1 {
		entries := make([]presence.Entry, 0, len(r.presence)-1)
		for id, e := range r.presence {
			if id == cl.ID || e.Removed {
				continue
			}
			if e.State.Cursor == nil && e.State.Tool == "" && len(e.State.Selection) == 0 {
				continue
			}
			entries = append(entries, *e)
		}
		if len(entries) > 0 {
			if b, err := r.codec.MarshalPresence(entries); err == nil {
				select {
				case cl.Send <- b:
				default:
				}
			}
		}
	}
	c.Reply <- nil
}

func (r *Room) handleLeave(c LeaveCmd) {
	if cl, ok := r.clients[c.ClientID]; ok {
		cl.Close()
		delete(r.clients, c.ClientID)
	}
	// Presence-запись помечаем removed, чистим на ближайшем тике.
	// Убираем из map только ПОСЛЕ рассылки removed-флага — иначе
	// другие участники никогда не получат сигнал «сотри курсор».
	if e, ok := r.presence[c.ClientID]; ok && !e.Removed {
		e.Removed = true
		r.presenceDirty[c.ClientID] = true
	}
}

func (r *Room) handleApplyOps(c ApplyOpsCmd) {
	applied := make([]crdt.Op, 0, len(c.Ops))
	for _, op := range c.Ops {
		// Нормализуем TS: сервер вызывает Observe, чтобы его локальные
		// часы «догнали» удалённые. Затем Now() даёт новый TS, строго
		// больший и клиентского, и локального. Это нужно на случай
		// кривых часов клиента: без Observe его ops могли бы всегда
		// проигрывать, и наоборот.
		r.clock.Observe(op.TS)
		op.TS = r.clock.Now()
		// Origin берём из From — даже если в op.TS.Node другой текст,
		// авторитетом является From. Изменяем node в TS на серверный,
		// чтобы другие клиенты видели «это прошло через сервер».
		if op.TS.Node == "" {
			op.TS.Node = "server:" + r.id
		}
		if r.doc.Apply(op) {
			applied = append(applied, op)
		}
	}
	r.seqN++

	// History-кольцо: пишем всегда, независимо от store. Это
	// дешво (O(1) push в массив фикс. размера) и даёт
	// наблюдаемость «что происходило на доске» даже в dev-режиме
	// с -data="".
	if len(applied) > 0 {
		r.hist.push(Record{
			Seq:    r.seqN,
			Origin: c.From,
			Ops:    applied,
			At:     time.Now().UnixMilli(),
		})
	}

	// Персистентность: пишем WAL-запись ПЕРЕД broadcast. Иначе есть
	// окно, когда клиент уже увидел op, а сервер умер, не записав,
	// и после recovery op «исчезнет» — это ломает monotonicity для
	// пользователя. Ошибка store логируется, но не откатывает doc:
	// «документ живой, персист отстанет» лучше, чем «ops потеряны».
	if r.store != nil && len(applied) > 0 {
		rec := Record{
			Seq:    r.seqN,
			Origin: c.From,
			Ops:    applied,
			At:     time.Now().UnixMilli(),
		}
		if err := r.store.Append(r.id, rec); err != nil {
			r.log.Error("store: append failed", "err", err, "seq", rec.Seq)
		}
		r.batchesSinceSnap++
		if r.cfg.SnapshotEvery > 0 && r.batchesSinceSnap >= r.cfg.SnapshotEvery {
			r.batchesSinceSnap = 0
			snap := Snapshot{
				Seq:      r.seqN,
				Elements: r.doc.Snapshot(),
				SavedAt:  time.Now().UnixMilli(),
			}
			if err := r.store.SaveSnapshot(r.id, snap); err != nil {
				r.log.Error("store: snapshot failed", "err", err, "seq", snap.Seq)
			}
		}
	}

	// Пустой applied всё равно требует ack: клиент ждёт подтверждения
	// batchID, иначе не поймёт, что батч «переварен».
	if len(applied) > 0 {
		payload, err := r.codec.MarshalOps(r.seqN, c.From, applied)
		if err != nil {
			r.log.Error("codec: ops failed", "err", err)
		} else {
			r.broadcast(payload)
		}
	}

	if cl, ok := r.clients[c.From]; ok {
		ack, err := r.codec.MarshalAck(c.BatchID, r.seqN)
		if err == nil {
			r.sendTo(cl, ack)
		}
	}
}

// --- internals ---

// handleHistory — читает диапазон из history-кольца и кладёт в
// Reply. Никакой мутации — это «read-only» команда, но она всё
// равно идёт через dispatch: иначе нам пришлось бы городить
// мьютекс на r.hist, ломая инвариант актora.
func (r *Room) handleHistory(c HistoryCmd) {
	c.Reply <- r.hist.rangeBetween(c.From, c.To)
}

// broadcast — неблокирующая рассылка. Снимок списка клиентов делаем заранее:
// sendTo может удалить клиента из map, а мутация map во время итерации
// в Go не гарантирует порядок и может пропустить элементы.
func (r *Room) broadcast(payload []byte) {
	list := make([]*Client, 0, len(r.clients))
	for _, cl := range r.clients {
		list = append(list, cl)
	}
	for _, cl := range list {
		r.sendTo(cl, payload)
	}
}

// sendTo — сердце backpressure. Либо сообщение попало в буфер, либо
// клиент выкидывается. Никакой блокировки, никаких «подождём».
func (r *Room) sendTo(cl *Client, payload []byte) {
	select {
	case cl.Send <- payload:
		return
	case <-cl.Done:
		// Уже выкинут ранее.
		return
	default:
		r.log.Warn("slow client kicked", "client", cl.ID)
		cl.Close()
		delete(r.clients, cl.ID)
	}
}

func (r *Room) shutdownAll() {
	for _, cl := range r.clients {
		cl.Close()
	}
	// map не обнуляем: dispatch уже завершился, и на Room больше никто
	// не должен ссылаться. Утечки нет — hub выбросит комнату из своей
	// таблицы после WaitGroup.
	//
	// Персист: финальный flush + close. Иначе последние WalFlushTick-
	// интервалы «останутся» в bufio-буфере и потеряются при exit.
	if r.store != nil {
		if err := r.store.Flush(r.id); err != nil {
			r.log.Error("store: shutdown flush failed", "err", err)
		}
		if err := r.store.Close(r.id); err != nil {
			r.log.Error("store: shutdown close failed", "err", err)
		}
	}
}

// flushWAL — периодический fsync WAL-а. Дёргается из Run-цикла с
// периодом WalFlushTick. Если store nil — no-op (тикер не заведён).
func (r *Room) flushWAL() {
	if r.store == nil {
		return
	}
	if err := r.store.Flush(r.id); err != nil {
		r.log.Error("store: flush failed", "err", err)
	}
}

// --- presence handlers ---

// handlePresence — обновляет запись в карте presence и помечает её dirty.
// Никакой рассылки здесь: flushPresence сделает это на ближайшем тике,
// «склеив» несколько кадров одного клиента в один broadcast.
func (r *Room) handlePresence(c PresenceCmd) {
	// Обновлять presence можно только для подключённого клиента.
	if _, ok := r.clients[c.ClientID]; !ok {
		return
	}
	entry, ok := r.presence[c.ClientID]
	if !ok {
		// Могло не быть, если Join ещё не обработан, а presence уже летит.
		// Создаём запись «на лету».
		cl := r.clients[c.ClientID]
		entry = &presence.Entry{ClientID: cl.ID, Name: cl.Name, Color: cl.Color}
		r.presence[c.ClientID] = entry
	}
	entry.State = c.State
	entry.LastSeen = time.Now()
	entry.Removed = false
	r.presenceDirty[c.ClientID] = true
}

// flushPresence — «сердце» coalescing. Вызывается с периодом cfg.PresenceTick.
//
// Три вещи за проход:
//   1. evict: помечаем removed те, кто молчит дольше PresenceTimeout;
//   2. собираем entries по presenceDirty и кодируем одним batch;
//   3. из map выкидываем только тех, у кого Removed=true — так другие
//      получают финальный «removed»-кадр, а призрачные записи не копятся.
func (r *Room) flushPresence() {
	now := time.Now()
	if r.cfg.PresenceTimeout > 0 {
		for id, e := range r.presence {
			if e.Removed {
				continue
			}
			if now.Sub(e.LastSeen) > r.cfg.PresenceTimeout {
				e.Removed = true
				r.presenceDirty[id] = true
			}
		}
	}
	if len(r.presenceDirty) == 0 {
		return
	}
	entries := make([]presence.Entry, 0, len(r.presenceDirty))
	for id := range r.presenceDirty {
		e, ok := r.presence[id]
		if !ok {
			continue
		}
		entries = append(entries, *e)
		if e.Removed {
			delete(r.presence, id)
		}
	}
	r.presenceDirty = make(map[string]bool)
	if len(entries) == 0 {
		return
	}
	payload, err := r.codec.MarshalPresence(entries)
	if err != nil {
		r.log.Error("codec: presence failed", "err", err)
		return
	}
	r.broadcast(payload)
}

// runGC — уборка tombstone-ов. Вызывается с периодом cfg.GCTick.
// Логируем сколько выкинули — полезно при отладке «почего doc растёт».
func (r *Room) runGC() {
	if r.cfg.TombstoneRetention <= 0 {
		return
	}
	cutoff := time.Now().Add(-r.cfg.TombstoneRetention)
	if n := r.doc.SweepTombstones(cutoff); n > 0 {
		r.log.Info("gc: tombstones swept", "removed", n, "live", r.doc.Live(), "total", r.doc.Len())
	}
}
