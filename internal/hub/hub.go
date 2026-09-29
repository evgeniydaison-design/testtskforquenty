// Package hub — менеджер комнат.
//
// Ответственность Этапа 4: ленивое создание с восстановлением из
// персиста, хранение, graceful shutdown. Выгрузка неактивных комнат
// по таймеру появится позже (Этап 7 — под нагрузкой).
package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"board/internal/room"
)

// Hub — потокобезопасный реестр активных комнат.
//
// Комната создаётся в момент первого Join и живёт до shutdown сервера
// (или до явной выгрузки в Этапе 7). Одна и та же roomID всегда
// возвращает одного и того же актora — это фундаментальная инвариант
// для CRDT-слияния: два «параллельных» акта на одном doc = рассинхрон.
//
// На Этапе 4 hub умеет восстанавливать doc при создании комнаты:
// если подключён Loader, он вызывается ровно один раз per-room, до
// запуска Run-цикла.
type Hub struct {
	// ctx задаёт время жизни всех горутин-комнат. Отменяется в Shutdown.
	ctx    context.Context
	cancel context.CancelFunc

	cfg    room.Config
	codec  room.Codec
	store  room.Store   // опционально; nil = «только память»
	loader room.Loader // опционально; nil = не восстанавливаем
	log    *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room.Room
	wg    sync.WaitGroup
}

// Option — функциональные опции New.
type Option func(*Hub)

// WithStore подключает персистентность для новых комнат. Loader —
// «то же самое» в файловом случае, но в общем виде это разные
// интерфейсы; вызывающий может дать разные реализации (например,
// read-only реплика с другого узла).
func WithStore(s room.Store, l room.Loader) Option {
	return func(h *Hub) {
		h.store = s
		h.loader = l
	}
}

// New создаёт hub. Вызывающий обязан когда-нибудь позвать Shutdown.
func New(parent context.Context, codec room.Codec, cfg room.Config, log *slog.Logger, opts ...Option) *Hub {
	ctx, cancel := context.WithCancel(parent)
	if log == nil {
		log = slog.Default()
	}
	h := &Hub{
		ctx:    ctx,
		cancel: cancel,
		cfg:    cfg,
		codec:  codec,
		log:    log,
		rooms:  make(map[string]*room.Room),
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// ErrHubStopped возвращается, если hub уже закрыт.
var ErrHubStopped = errors.New("hub: остановлен")

// GetOrCreate возвращает комнату по ID, создавая её при необходимости.
// Если подключён Loader, состояние читается синхронно до запуска
// Run-цикла: любой внешний наблюдатель, увидевший комнату через
// этот метод, гарантированно увидит doc с уже восстановленным содержимым.
//
// Возвращает nil, если hub остановлен.
func (h *Hub) GetOrCreate(id string) *room.Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	select {
	case <-h.ctx.Done():
		return nil
	default:
	}
	if r, ok := h.rooms[id]; ok {
		return r
	}

	// Восстановление. Ошибка loader не фатальна: «пустая комната» лучше,
	// чем «сервер не стартует из-за битого WAL-а на диске». Логируем и
	// идём дальше с Loaded==nil.
	var loaded *room.Loaded
	if h.loader != nil {
		l, err := h.loader.Load(id)
		if err != nil {
			h.log.Error("hub: loader failed, starting empty", "room", id, "err", err)
		} else {
			loaded = l
		}
	}

	opts := []room.Option{}
	if h.store != nil {
		opts = append(opts, room.WithStore(h.store))
	}
	if loaded != nil {
		opts = append(opts, room.WithLoaded(loaded))
	}
	r := room.New(id, h.codec, h.cfg, h.log, opts...)

	h.rooms[id] = r
	h.wg.Add(1)
	// Горутина комнаты. Когда Run выйдет (ctx cancel или явный стоп),
	// убираем запись из map — иначе утечка при выгрузке неактивных.
	go func() {
		defer h.wg.Done()
		r.Run(h.ctx)
		h.mu.Lock()
		if cur, ok := h.rooms[id]; ok && cur == r {
			delete(h.rooms, id)
		}
		h.mu.Unlock()
	}()
	return r
}

// Rooms — снимок текущих АКТИВНЫХ комнат. Для метрик (Этап 7) и логов.
// В Этапе 4 добавили KnownRooms — он включает и «на диске, но ещё не
// открытые», что полезно для debug-эндпоинта /rooms.
func (h *Hub) Rooms() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.rooms))
	for id := range h.rooms {
		out = append(out, id)
	}
	return out
}

// Peek — существующая комната по id, БЕЗ создания новой.
// Возвращает nil, если комнаты нет в памяти. Нужен HTTP-эндпоинтам
// (/rooms/{id}/history), где «создать комнату ради GET-а» —
// неприемлемый сайд-эффект.
func (h *Hub) Peek(id string) *room.Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.rooms[id]
}

// KnownRooms — активные ∪ известные loader-у. Возвращает отсортированный
// union; если loader nil — тоже самое, что Rooms(). Активные идут
// первыми (стабильный порядок для UI).
func (h *Hub) KnownRooms() []string {
	active := h.Rooms()
	if h.loader == nil {
		return active
	}
	persisted, err := h.loader.List()
	if err != nil {
		// Не роняем вызывающего: просто «не дополняем».
		return active
	}
	seen := make(map[string]bool, len(active)+len(persisted))
	out := make([]string, 0, len(active)+len(persisted))
	for _, id := range active {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, id := range persisted {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// Shutdown отключает все комнаты и ждёт завершения их горутин.
// Возвращает ошибку контекста, если ожидание превысило ctx.
func (h *Hub) Shutdown(ctx context.Context) error {
	h.cancel()
	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
