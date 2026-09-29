package room

// История применённых батчей.
//
// Это НЕ «журнал для репликации» (для этого есть WAL в store.go).
// History — компактный in-memory «ring buffer» последних N батчей,
// доступный по seq-диапазону. Нужен для:
//   1. HTTP-эндпоинта /rooms/{id}/history — debug и UI-«audit log»;
//   2. инкрементального sync в будущем (Этап 6: client reconnect c
//      lastSeq → server отдаёт records, которые ещё не «выпарены»);
//   3. undo/redo на клиенте — сам undo НЕ ходит на сервер за историей
//      (per-client stack живёт в браузере), но server-сторона
//      должна уметь «показать что произошло», если пользователь
//      жалуеться на «мои правки пропали».
//
// Ring buffer, а не слайс: push O(1), память ограничена сверху,
// нет «распухания между GC-циклами». Переполнение — самые старые
// записи перезатираются; current() всегда даёт максимальный seq
// среди ХРАНИМЫХ.

// historyRing — кольцевой буфер фиксированного размера.
//
// head — индекс СЛЕДУЮЩЕЙ записи (самая старая, если буфер полный).
// size — сколько реально занято (0..cap). При переполнении head
// сдвигается, effectively выталкивая самый старый элемент.
type historyRing struct {
	buf  []Record
	head int
	size int
	cap  int
}

func newHistoryRing(n int) *historyRing {
	if n <= 0 {
		n = defaultHistorySize
	}
	return &historyRing{buf: make([]Record, n), cap: n}
}

// push кладёт запись в хвост. Если буфер заполнен — вытесняет
// самый старый. Seq новой записи ДОЛЖЕН быть строго больше current()
// (комната вызывает push уже после r.seqN++, инвариант выполняется
// автоматически). Никакой проверки на дубликаты здесь нет: вызывающий
// отвечает за monotonicity.
func (h *historyRing) push(r Record) {
	if h.size < h.cap {
		// Ещё есть свободные слоты: пишем в «конец».
		idx := (h.head + h.size) % h.cap
		h.buf[idx] = r
		h.size++
		return
	}
	// Полный: перезаписываем голову и сдвигаем её.
	h.buf[h.head] = r
	h.head = (h.head + 1) % h.cap
}

// current — seq последней записи, или 0, если пусто.
func (h *historyRing) current() uint64 {
	if h.size == 0 {
		return 0
	}
	return h.buf[(h.head+h.size-1)%h.cap].Seq
}

// all — снимок всех записей в порядке возрастания Seq. Выделение
// нового слайса: вызывающий может свободно отдавать результат наружу
// (например, в HTTP- handler), mutate-ить buf дальше безопасно.
func (h *historyRing) all() []Record {
	out := make([]Record, 0, h.size)
	for i := 0; i < h.size; i++ {
		out = append(out, h.buf[(h.head+i)%h.cap])
	}
	return out
}

// rangeBetween — записи с from <= Seq <= to. to==0 снимает верхнюю
// границу. from==0 — нижнюю. Результат отсортирован по Seq ↑.
//
// Линейный проход O(size). ПриHistorySize=1000 это ~1000 сравнений;
// для HTTP-debug эндпоинта более чем. Бинарный поиск по «head» —
// излишество: seq-и тут «почти последовательные» (plus-1 на батч),
// поэтому средняя длина диапазона мала.
func (h *historyRing) rangeBetween(from, to uint64) []Record {
	out := make([]Record, 0, h.size)
	for i := 0; i < h.size; i++ {
		r := h.buf[(h.head+i)%h.cap]
		if from > 0 && r.Seq < from {
			continue
		}
		if to > 0 && r.Seq > to {
			continue
		}
		out = append(out, r)
	}
	return out
}
