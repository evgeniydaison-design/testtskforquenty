// Package store — персистентность документа.
//
// Здесь живёт реализация интерфейсов room.Store/room.Loader. Сам пакет
// room ничего не знает про файлы и JSON — он работает через consumer-side
// интерфейс. Это даёт свободу менять backend (bbolt/Postgres) без
// правок в «горячем» коде комнаты.
package store

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"board/internal/room"
)

// FileStore — JSON-lines WAL + JSON-снимок на файловой системе.
//
// Почему файл, а не bbolt/sqlite:
//   - прозрачность: «cat wal.log» сразу показывает историю;
//   - никакой новой зависимости и схемы;
//   - append-only ложится на актор-модель комнаты — writes сериализованы;
//   - миграция на bbolt = меняем реализацию, интерфейс room.Store не
//     трогаем.
//
// Layout:
//
//	<root>/<hex(roomID)>/snapshot.json   — atomic rename через tmp
//	<root>/<hex(roomID)>/wal.log          — JSON-lines, append-only
//
// hex(roomID): защита от path traversal (клиент может прислать «../../»).
// hex читаем глазами в ls, не прячем в base32.
//
// Concurrency: все методы берут общий mutex. Write-path в комнате
// и так сериализован, так что lock почти не конкурентится. Для
// async-writer оптимизации (Этап 7) выделим per-room mutex.
type FileStore struct {
	root string

	mu      sync.Mutex
	writers map[string]*walWriter // key: hex(roomID)
}

type walWriter struct {
	f       *os.File
	w       *bufio.Writer
	lastSeq uint64 // защита от retry-дубликата
}

// New создаёт FileStore в каталоге root. Каталог создаётся, если его
// нет. Пустой root — ошибка: это защита от случайного «не указали -data».
func New(root string) (*FileStore, error) {
	if root == "" {
		return nil, errors.New("store: root required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("store: abs(%s): %w", root, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", abs, err)
	}
	return &FileStore{root: abs, writers: map[string]*walWriter{}}, nil
}

// List — известные персисту roomID. Возвращает исходные (unhex) id,
// отсортированные лексикографически — для стабильных логов и тестов.
func (fs *FileStore) List() ([]string, error) {
	entries, err := os.ReadDir(fs.root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: list %s: %w", fs.root, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id, err := unhex(e.Name())
		if err != nil {
			// Мусорный каталог — пропускаем; логирует вызывающий, если
			// захочет. Здесь важнее не упасть.
			continue
		}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

// Load читает снапшот (если есть) + «хвост» WAL, т.е. записи с
// Seq > snapshot.Seq.
//
// «Повреждённая» последняя линия (обрывок из-за краша посреди записи)
// трактуется как «дальше нет валидных данных» — цикл прерывается,
// всё, что до, уже валидно. Это стандартный WAL-семантика: потеря
// последнего fsync-интервала допустима, потеря середины — нет.
//
// Если файла нет, возвращаем *Loaded{} (не nil) — вызывающий может
// не проверять nil, и работать с пустым состоянием.
func (fs *FileStore) Load(roomID string) (*room.Loaded, error) {
	out := &room.Loaded{}
	dir := fs.dir(roomID)

	// Snapshot.
	if b, err := os.ReadFile(filepath.Join(dir, "snapshot.json")); err == nil {
		var s room.Snapshot
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, fmt.Errorf("store: parse snapshot %s: %w", roomID, err)
		}
		out.Snapshot = &s
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("store: read snapshot %s: %w", roomID, err)
	}

	// WAL tail.
	f, err := os.Open(filepath.Join(dir, "wal.log"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return nil, fmt.Errorf("store: open wal %s: %w", roomID, err)
	}
	defer f.Close()

	var minSeq uint64
	if out.Snapshot != nil {
		minSeq = out.Snapshot.Seq
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r room.Record
		if err := json.Unmarshal(line, &r); err != nil {
			// Хвост повреждён — стоп, остальное не читаем.
			break
		}
		if r.Seq <= minSeq {
			continue
		}
		out.Records = append(out.Records, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("store: scan wal %s: %w", roomID, err)
	}
	return out, nil
}

// Append пишет одну запись в WAL.
//
// Буферизованный writer, fsync НЕ на каждый вызов — это убило бы
// throughput. Комната периодически дергает Flush (см. cfg.WalFlushTick),
// и SaveSnapshot сам делает Flush перед записью.
//
// Идемпотентность: если r.Seq <= lastSeq, игнорируем. Это защита от
// retry-дубликата (клиент переслал батч после reconnect, ack потерялся).
func (fs *FileStore) Append(roomID string, r room.Record) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	w, err := fs.ensureWriterLocked(roomID)
	if err != nil {
		return err
	}
	if r.Seq <= w.lastSeq {
		return nil
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("store: marshal wal record: %w", err)
	}
	if _, err := w.w.Write(b); err != nil {
		return fmt.Errorf("store: wal write: %w", err)
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return fmt.Errorf("store: wal newline: %w", err)
	}
	w.lastSeq = r.Seq
	return nil
}

// Flush — fsync WAL-а для roomID. No-op, если writer'а нет.
func (fs *FileStore) Flush(roomID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.flushLocked(roomID)
}

func (fs *FileStore) flushLocked(roomID string) error {
	key := hexID(roomID)
	w, ok := fs.writers[key]
	if !ok {
		return nil
	}
	if err := w.w.Flush(); err != nil {
		return fmt.Errorf("store: flush wal %s: %w", roomID, err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("store: fsync wal %s: %w", roomID, err)
	}
	return nil
}

// SaveSnapshot пишет полный снимок doc + trunc'ит WAL.
//
// Sequence:
//  1. Flush WAL на диск — иначе reload увидит «рваный» хвост (снапшот
//     уже содержит ops, а WAL их не показал).
//  2. tmp-write + atomic rename snapshot.json.
//  3. Rewrite WAL, оставляя только Seq > snap.Seq (обычно пусто).
//
// Если шаг 3 упадёт, следующий Load отфильтрует «старые» records по
// minSeq, так что лишние строки безвредны.
func (fs *FileStore) SaveSnapshot(roomID string, s room.Snapshot) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	if err := fs.flushLocked(roomID); err != nil {
		return err
	}
	dir := fs.dir(roomID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("store: marshal snapshot: %w", err)
	}
	tmp := filepath.Join(dir, "snapshot.json.tmp")
	final := filepath.Join(dir, "snapshot.json")
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("store: write snapshot tmp: %w", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("store: rename snapshot: %w", err)
	}
	return fs.truncateWALLocked(roomID, s.Seq)
}

// truncateWALLocked переписывает WAL, оставляя только Seq > upto.
// Вызывается из SaveSnapshot; fs.mu уже удерживается.
func (fs *FileStore) truncateWALLocked(roomID string, upto uint64) error {
	key := hexID(roomID)
	dir := fs.dir(roomID)
	path := filepath.Join(dir, "wal.log")
	// Закрываем writer — дальше будем переписывать файл целиком.
	if w, ok := fs.writers[key]; ok {
		_ = w.w.Flush()
		_ = w.f.Close()
		delete(fs.writers, key)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("store: read wal for truncate: %w", err)
	}
	var keep []string
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		var probe struct {
			Seq uint64 `json:"seq"`
		}
		if json.Unmarshal([]byte(line), &probe) == nil && probe.Seq > upto {
			keep = append(keep, line)
		}
	}
	if len(keep) == 0 {
		// Пустой WAL: просто удаляем.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: remove wal: %w", err)
		}
		return nil
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(keep, "\n")+"\n"), 0o644); err != nil {
		return fmt.Errorf("store: write wal tmp: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("store: rename wal: %w", err)
	}
	return nil
}

// Close освобождает ресурсы, относящиеся к roomID. Идемпотентно.
func (fs *FileStore) Close(roomID string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	key := hexID(roomID)
	w, ok := fs.writers[key]
	if !ok {
		return nil
	}
	var firstErr error
	if err := w.w.Flush(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("store: flush on close %s: %w", roomID, err)
	}
	if err := w.f.Close(); err != nil && firstErr == nil {
		firstErr = fmt.Errorf("store: close file %s: %w", roomID, err)
	}
	delete(fs.writers, key)
	return firstErr
}

// CloseAll освобождает всё. Используется на shutdown сервера.
func (fs *FileStore) CloseAll() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	var firstErr error
	for key, w := range fs.writers {
		if err := w.w.Flush(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("store: flush all %s: %w", key, err)
		}
		if err := w.f.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("store: close all %s: %w", key, err)
		}
	}
	fs.writers = map[string]*walWriter{}
	return firstErr
}

// --- internals ---

func (fs *FileStore) dir(roomID string) string {
	return filepath.Join(fs.root, hexID(roomID))
}

func (fs *FileStore) ensureWriterLocked(roomID string) (*walWriter, error) {
	key := hexID(roomID)
	if w, ok := fs.writers[key]; ok {
		return w, nil
	}
	dir := fs.dir(roomID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, "wal.log")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("store: open wal %s: %w", roomID, err)
	}
	// lastSeq = максимум уже записанного, иначе первый append после
	// restart'а ошибочно сойдёт за retry-дубликат.
	var last uint64
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if line == "" {
				continue
			}
			var probe struct {
				Seq uint64 `json:"seq"`
			}
			if json.Unmarshal([]byte(line), &probe) == nil && probe.Seq > last {
				last = probe.Seq
			}
		}
	}
	w := &walWriter{
		f:       f,
		w:       bufio.NewWriterSize(f, 32*1024),
		lastSeq: last,
	}
	fs.writers[key] = w
	return w, nil
}

// hexID кодирует roomID для файловой системы. hex обратим и безопасен
// к любому вводу клиента (включая «/», «..», «\0»).
func hexID(s string) string {
	return hex.EncodeToString([]byte(s))
}

func unhex(s string) (string, error) {
	b, err := hex.DecodeString(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
