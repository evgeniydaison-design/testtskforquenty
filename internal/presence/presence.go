// Package presence — эфемерное состояние участников: курсор, выделение,
// активный инструмент.
//
// Ключевое отличие от crdt.Op: presence НЕ входит в CRDT-журнал, НЕ пишется
// в WAL и НЕ влияет на документ. Пропущенный или «склеенный» кадр не портит
// историю — просто чужой курсор на мгновение «подвиснет». Поэтому presence
// проходит отдельным каналом и живёт в отдельной карте комнаты.
//
// Сами типы — чистые данные. Никакой логики слияния: последнее обновление
// от конкретного clientId всегда побеждает (последовательная обработка
// гарантирует это внутри Run-цикла комнаты).
package presence

import "time"

// Point — координата курсора в системе координат ДОКУМЕНТА, а не canvas.
// Клиент сам конвертирует через viewport (scale + translate). Это важно:
// если два пользователя смотрят на разные области, сервер не обязан
// «пересчитывать» их в одну.
type Point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

// State — то, что клиент шлёт про себя одним presence-кадром.
//
// Все поля — опциональные. `cursor` может быть nil, когда пользователь
// увёл мышь за пределы окна. `selection` пуст, когда ничего не выбрано.
type State struct {
	Cursor    *Point   `json:"cursor,omitempty"`
	Selection []string `json:"selection,omitempty"`
	Tool      string   `json:"tool,omitempty"`
}

// Entry — запись серверного реестра presence. Отправляется клиенту как
// часть batch-сообщения TypePresence.
//
// Removed=true означает «сотри этот курсор». Удаление из карты происходит
// ПОСЛЕ рассылки (см. Room.flushPresence), иначе другие участники никогда
// не узнают, что надо стереть призрачный курсор.
type Entry struct {
	ClientID string  `json:"clientId"`
	Name     string  `json:"name"`
	Color    string  `json:"color"`
	State    State   `json:"state"`
	Removed  bool    `json:"removed,omitempty"`

	// LastSeen — серверная метка. json:"-": клиенту она ни к чему, а без
	// этого мы бы тянули время через сеть, что добавило бы шум в диффы.
	LastSeen time.Time `json:"-"`
}
