// Package crdt — модель данных и правила слияния.
//
// С Этапа 3 документ построен как «набор элементов, каждый элемент —
// набор LWW-регистров». Это даёт корректное параллельное редактирование
// РАЗНЫХ свойств одного элемента: «A двигает — Б красит» сохраняет и X/Y,
// и Stroke. Слияние по всему элементу (как в Этапе 1) теряло одно из
// изменений — здесь этого больше нет.
//
// Таймстемп — HLC (Wall, Logical, Node). Lamport+ClientID из Этапа 1
// заменён: Wall даёт связь с физическим временем (для «кто последний» в
// UI), Logical — под-мс разрешение при одинаковом Wall, Node — финальный
// tie-break, исключающий ничью.
package crdt

// ElementType — тип примитива на холсте.
type ElementType string

const (
	TypeRect    ElementType = "rect"
	TypeEllipse ElementType = "ellipse"
	TypeArrow   ElementType = "arrow"
	TypeLine    ElementType = "line"
	TypeText    ElementType = "text"
)

// PropName — имя свойства элемента. Ключ в Element.Props.
//
// Держим строки, а не enum: легко добавлять поля без перекомпиляции
// клиентов. Ошибочное имя — просто новая «колонка», ничего не ломает.
type PropName = string

const (
	PropX      = "x"
	PropY      = "y"
	PropW      = "w"
	PropH      = "h"
	PropAngle  = "angle"
	PropStroke = "stroke"
	PropFill   = "fill"
	PropText   = "text"
	PropPoints = "points"
	PropRank   = "rank" // fractional index для z-order
	PropDeleted = "del" // tombstone, value=true
)

// Element — «коробка» с идентификатором, типом и набором LWW-регистров.
//
// ID и Type иммутабельны: они не участвуют в слиянии. Если два клиента
// создали элементы с одним ID (коллизия UUID — редкость, но возможна при
// багах) — это разрешается на уровне Doc.Apply: первый выигрывает, Type
// берётся от победителя.
//
// Props — карта «propName → *Register». Nil-регистр не хранится; если
// значение сбросили, регистр остаётся, но Value=nil.
type Element struct {
	ID   string      `json:"id"`
	Type ElementType `json:"type"`
	Props map[PropName]*Register `json:"props"`
}

// NewElement создаёт элемент с одним регистром Type (value=type, ts=ts).
// Type — «псевдо-свойство», оно нужно для рендера, поэтому хранится
// отдельно от Props.
//
// Начальные Props пусты. Вызывающий код обязан сам вызвать Set/Apply для
// каждого свойства — иначе «первый» op случайно затирает неизвестные
// поля.
func NewElement(id string, typ ElementType) *Element {
	return &Element{
		ID:    id,
		Type:  typ,
		Props: make(map[PropName]*Register),
	}
}

// Set — применить значение к регистру. Возвращает true, если значение
// изменилось (то есть incoming старше текущего).
func (e *Element) Set(name PropName, value any, ts HLC) bool {
	r, ok := e.Props[name]
	if !ok {
		r = &Register{}
		e.Props[name] = r
	}
	return r.Apply(value, ts)
}

// Get — прочитать значение. ok=false, если свойства никогда не задавали.
func (e *Element) Get(name PropName) (any, bool) {
	r, ok := e.Props[name]
	if !ok {
		return nil, false
	}
	return r.Value, true
}

// TS — максимальный таймстемп среди регистров. Нужен для «последовательного
// snapshot» (Этап 4 — WAL) и для GC tombstone-ов.
func (e *Element) TS() HLC {
	var max HLC
	for _, r := range e.Props {
		if r.TS.After(max) {
			max = r.TS
		}
	}
	return max
}

// Deleted — tombstone. Присутствие del=true в любом регистре = элемент
// удалён. Отдельное поле не заводим: tombstone — это такое же свойство.
func (e *Element) Deleted() bool {
	r, ok := e.Props[PropDeleted]
	if !ok {
		return false
	}
	v, _ := r.Value.(bool)
	return v
}
