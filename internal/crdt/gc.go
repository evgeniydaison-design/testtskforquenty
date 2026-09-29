package crdt

import "time"

// DefaultTombstoneRetention — 30 дней. Это компромисс:
//   - слишком короткий: офлайн-клиент, вернувшийся через неделю, увидит
//     «воскресшие» элементы, потому что его запоздавший upsert пройдёт;
//   - слишком длинный: память растёт, snapshot «раздут».
//
// 30 дней покрывает разумные сценарии «забыл телефон в ящике». После —
// GC вычищает. Полноценный sync (Этап 4) использует HLC.Wall для
// «клиент слишком старый, полный re-download», поэтому retention GC —
// просто гигиена памяти.
const DefaultTombstoneRetention = 30 * 24 * time.Hour

// SweepTombstones удаляет tombstone-элементы, у которых del.TSWall строго
// раньше cutoff. Возвращает количество удалённых.
//
// Почему НЕ «самый поздний del» в элементе: если у элемента del=true с
// старым TS, но другие регистры обновлены позже, он не должен быть
// «воскресшим» — это невалидное состояние. Такой случай treated как «нет,
// рановато» и остаётся до следующего sweep.
func (d *Doc) SweepTombstones(cutoff time.Time) int {
	cutoffWall := cutoff.UnixMilli()
	removed := 0
	for id, e := range d.elements {
		if !e.Deleted() {
			continue
		}
		r, ok := e.Props[PropDeleted]
		if !ok {
			continue
		}
		if r.TS.Wall >= cutoffWall {
			continue
		}
		delete(d.elements, id)
		removed++
	}
	return removed
}

// OldestTombstoneWall — для диагностики/метрик: какой самый старый
// tombstone ещё жив. Возвращает 0, если tombstone-ов нет.
func (d *Doc) OldestTombstoneWall() int64 {
	var oldest int64
	for _, e := range d.elements {
		if !e.Deleted() {
			continue
		}
		r, ok := e.Props[PropDeleted]
		if !ok {
			continue
		}
		if oldest == 0 || r.TS.Wall < oldest {
			oldest = r.TS.Wall
		}
	}
	return oldest
}
