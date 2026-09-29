package crdt

import (
	"sort"
)

// Doc — состояние документа: набор элементов + правила слияния.
//
// Инварианты:
//   - элементы хранятся по ID; нет «дубликатов ID»;
//   - tombstone (del=true) хранится, пока его не вытеснит более новый
//     del=false upsert; иначе запоздавший upsert «воскресит» удалённый;
//   - слияние детерминированно: любой порядок применения одного и того
//     же множества ops даёт одинаковое состояние (commutativity +
//     idempotence).
//
// Doc сам по себе NOT concurrency-safe. Внешний код (Room) вызывает его
// из одного актора. Для клиента используется такой же Doc — синхронно.
type Doc struct {
	elements map[string]*Element
}

// NewDoc — пустой документ.
func NewDoc() *Doc { return &Doc{elements: make(map[string]*Element)} }

// Len — число известных элементов (включая tombstones). Полезен для
// метрик и GC.
func (d *Doc) Len() int { return len(d.elements) }

// Live — число НЕ удалённых элементов.
func (d *Doc) Live() int {
	n := 0
	for _, e := range d.elements {
		if !e.Deleted() {
			n++
		}
	}
	return n
}

// Get — вернуть элемент по ID (включая tombstones).
func (d *Doc) Get(id string) (*Element, bool) {
	e, ok := d.elements[id]
	return e, ok
}

// Apply — применить Op. Возвращает true, если состояние изменилось.
//
// Логика:
//  1. Если элемента нет — создаём (только для upsert; delete на несуществующий
//     id = no-op, но tombstone всё равно кладём, чтобы запоздавший upsert
//     не создал элемент «с нуля» в обход порядка).
//  2. Для upsert перебираем Op.Props и делаем Set по каждому ключу;
//     ключ, где TS(op) <= текущего регистра, игнорируется (проиграл).
//  3. Для delete устанавливаем del=true с TS(op). Если существующий
//     регистр del имеет более новый TS, delete проигнорирован.
//
// Идемпотентность: повторный Apply того же Op возвращает false.
func (d *Doc) Apply(op Op) bool {
	el, exists := d.elements[op.ID]
	if !exists {
		// Для delete можно не создавать элемент? Нет: без tombstone
		// поздний upsert его «воскресит». Создаём tombstone-скелет.
		typ := op.Type
		if typ == "" {
			typ = TypeRect // дефолт; будет перезаписан реальным type,
			// когда прилетит запоздавший upsert с новым TS.
		}
		el = NewElement(op.ID, typ)
		d.elements[op.ID] = el
	} else if op.Type != "" && el.Type == "" {
		el.Type = op.Type
	}

	switch op.Kind {
	case OpUpsert:
		// «del» не должен приходить в Props upsert-а. Если пришёл —
		// игнорируем; delete идёт только через OpDelete.
		//
		// Возвращаем «хотя бы один Set прошёл». Это даёт идемпотентность:
		// повтор того же op (тот же TS) не меняет состояние → false.
		// Раньше тут стоял unconditional `return true` — из-за этого
		// TestDoc_Apply_LowerTSIgnored / TieBreakByNode / Idempotent
		// падали.
		changed := false
		for name, val := range op.Props {
			if name == PropDeleted {
				continue
			}
			if el.Set(name, val, op.TS) {
				changed = true
			}
		}
		// Если раньше элемент был удалён, а TS(op) строго больше TS
		// tombstone — «воскрешаем»: снимаем del.
		if el.Deleted() {
			if r, ok := el.Props[PropDeleted]; ok && !op.TS.After(r.TS) {
				// Delete всё ещё «новее» upsert — не воскресаем.
				return false
			}
			delete(el.Props, PropDeleted)
			return true
		}
		return changed
	case OpDelete:
		// Если уже удалено, но более «старым» delete — ignore.
		if r, ok := el.Props[PropDeleted]; ok && r.Value == true && !op.TS.After(r.TS) {
			return false
		}
		el.Set(PropDeleted, true, op.TS)
		return true
	}
	return false
}

// Snapshot возвращает живые (не tombstone) элементы, отсортированные по
// Rank. Это то, что уходит клиенту при join и то, что ложится в WAL
// (Этап 4).
func (d *Doc) Snapshot() []Element {
	out := make([]Element, 0, d.Live())
	for _, e := range d.elements {
		if e.Deleted() {
			continue
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		ri, _ := out[i].Get(PropRank)
		rj, _ := out[j].Get(PropRank)
		si, _ := ri.(string)
		sj, _ := rj.(string)
		// Пустой rank (не задан) = «в конец».
		if si == "" {
			si = "\xff"
		}
		if sj == "" {
			sj = "\xff"
		}
		if si != sj {
			return si < sj
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// All возвращает ВСЕ элементы, включая tombstones. Тесты и будущий
// «incremental sync» (Этап 4) работают через этот метод.
func (d *Doc) All() []Element {
	out := make([]Element, 0, len(d.elements))
	for _, e := range d.elements {
		out = append(out, *e)
	}
	return out
}

// Reset — полная замена состояния. Используется при загрузке из хранилища
// (Этап 4) и в тестах.
func (d *Doc) Reset(elements []Element) {
	d.elements = make(map[string]*Element, len(elements))
	for i := range elements {
		e := elements[i]
		cp := NewElement(e.ID, e.Type)
		for name, r := range e.Props {
			if r == nil {
				continue
			}
			cp.Props[name] = &Register{Value: r.Value, TS: r.TS}
		}
		d.elements[e.ID] = cp
	}
}
