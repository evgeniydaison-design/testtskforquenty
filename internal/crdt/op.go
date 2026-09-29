package crdt

// OpKind — различает «создать/обновить» и «удалить».
type OpKind string

const (
	OpUpsert OpKind = "upsert"
	OpDelete OpKind = "delete"
)

// Op — атомарное изменение, которое клиент шлёт на сервер.
//
// Partial-update: Props содержит только изменённые ключи. Это и ядро
// «per-property LWW» — слияние двух ops с одинаковым ID, но разными
// ключами, даёт объединение, а не overwrite.
//
// TS — HLC. На сервере он нормализуется: сервер вызывает Clock.Observe,
// результат идёт во все исходящие ops этого батча. Клиент не обязан
// синхронизировать часы с сервером, но сервер «сводит» их к общей
// временной шкале.
//
// Type — заполнен только при первичном upsert (создании). Повторный
// upsert может не слать type — Doc применит из существующего элемента.
type Op struct {
	Kind  OpKind           `json:"kind"`
	ID    string           `json:"id"`
	TS    HLC              `json:"ts"`
	Type  ElementType      `json:"type,omitempty"`
	Props map[PropName]any `json:"props,omitempty"`
}

// NewUpsert — convenience для клиента: собрать upsert из карты свойств.
func NewUpsert(id string, typ ElementType, ts HLC, props map[PropName]any) Op {
	return Op{Kind: OpUpsert, ID: id, TS: ts, Type: typ, Props: props}
}

// NewDelete — tombstone для id с таймстемпом ts.
func NewDelete(id string, ts HLC) Op {
	return Op{Kind: OpDelete, ID: id, TS: ts}
}
