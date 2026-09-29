package crdt

// Register — LWW-ячейка: значение + таймстемп последнего обновления.
//
// Value — any; в JSON-потоке используется конкретный тип (float64, string,
// []*Point, bool). Тип значения определяется по ключу props: «x» — float64,
// «stroke» — string, «points» — [][2]float64, «del» — true/nil.
//
// TS — HLC. Zero = «нет значения, ещё не писали».
type Register struct {
	Value any `json:"v"`
	TS    HLC `json:"ts"`
}

// Apply объединяет incoming-обновку в существующий регистр.
// true — если значение изменилось (то есть incoming строго старше).
//
// Идемпотентность: повторный apply того же HLC не меняет состояние.
// Это важно для реконнекта: клиент может прислать тот же батч, и
// сервер не должен «дёргивать» snapshot hash.
func (r *Register) Apply(value any, ts HLC) bool {
	if !r.TS.IsZero() && !ts.After(r.TS) {
		return false
	}
	r.Value = value
	r.TS = ts
	return true
}
