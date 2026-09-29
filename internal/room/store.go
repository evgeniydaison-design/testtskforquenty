package room

import "board/internal/crdt"

// defaultHistorySize — ёмкость in-memory history-кольца, если
// Config.HistorySize<=0. 1000 батчей ≈ полчаса активной доски на
// 30Hz-троттлинге — больше не нужно для «audit log» и reconnect
// backfill-а в пределах Этапа 5.
const defaultHistorySize = 1000

// Snapshot — снимок doc + seqN на момент записи.
//
// Имя «Snapshot», а не «State», чтобы не путать с Loaded (состояние,
// которое Load вернул на старте) и с wire-форматом SnapshotPayload
// (он же, но без SavedAt).
//
// SavedAt — unix ms. Нужен для отладки и для логов «как давно мы не
// писали полный снапшот». В слиянии НЕ участвует.
type Snapshot struct {
	Seq      uint64         `json:"seq"`
	Elements []crdt.Element `json:"elements"`
	SavedAt  int64          `json:"savedAt"`
}

// Record — одна запись WAL: «в этот seqApply пришёл вот такой батч».
//
// Origin — clientId автора. Нужен для корректного эхо-фильтра при
// восстановлении клиента: если после рестарта сервер шлёт «backfill»,
// клиент должен суметь отличить «это моё же, не применять повторно».
//
// Ops — уже нормализованные сервером TS (после Observe+Now). Именно
// они ушли в broadcast; пишем ровно то, что увидели клиенты, чтобы
// recovery давал идентичный документ.
type Record struct {
	Seq    uint64    `json:"seq"`
	Origin string    `json:"origin,omitempty"`
	Ops    []crdt.Op `json:"ops"`
	At     int64     `json:"at"` // unix ms для отладки
}

// Loaded — состояние, которое hub восстановил из персиста перед
// запуском комнаты. Может быть нулевым (комната впервые).
type Loaded struct {
	Snapshot *Snapshot
	Records  []Record // с Seq > Snapshot.Seq, отсортированы по Seq
}

// Empty — convenience для «первый запуск комнаты».
func (l *Loaded) IsEmpty() bool {
	return l == nil || (l.Snapshot == nil && len(l.Records) == 0)
}

// Store — то, чем комната пишет doc-мутации во внешний персист.
//
// Интерфейс объявлен на стороне комнаты: она не знает про файл, bbolt
// или Postgres. Реализация обязана:
//   - Append: сохранять record так, чтобы он выжил после SIGKILL
//     (fsync — внутреннее дело реализации; комната даёт ей до
//     WalFlushTick на batch fsync);
//   - SaveSnapshot: атомарно (tmp+rename) писать полный снимок doc;
//   - Close: сбросить буферы и освободить ресурсы для roomID.
//
// Методы вызываются синхронно из Run-цикла комнаты. Реализация обязана
// быть быстрой; «долгая» IO — это сигнал, что пора в асинхронный writer
// (Этап 7 perf-work).
type Store interface {
	Append(roomID string, r Record) error
	SaveSnapshot(roomID string, s Snapshot) error
	Flush(roomID string) error
	Close(roomID string) error
}

// Loader — то, чем hub читает персист на старте.
//
// Вынесен в отдельный интерфейс, чтобы «горячий» Store не содержал
// «холодный» Load: это разные ответственности, и реализация может
// жить в разных процессах (например, sidecar-агент, читающий WAL).
type Loader interface {
	Load(roomID string) (*Loaded, error)
	List() ([]string, error)
}

// NopStore — заглушка «нет персиста». Используется в unit-тестах и
// когда запускаем сервер с -data="" (dev-режим).
type NopStore struct{}

func (NopStore) Append(string, Record) error           { return nil }
func (NopStore) SaveSnapshot(string, Snapshot) error   { return nil }
func (NopStore) Flush(string) error                    { return nil }
func (NopStore) Close(string) error                    { return nil }
func (NopStore) Load(string) (*Loaded, error)          { return &Loaded{}, nil }
func (NopStore) List() ([]string, error)               { return nil, nil }
