package crdt

import (
	"strings"
	"sync"
	"time"
)

// HLC — Hybrid Logical Clock (Kulkarni et al., 2014).
//
// Triple (Wall, Logical, Node):
//   - Wall — unix millis, максимум локальных часов и полученного Wall;
//   - Logical — «подмиллисекундный» счётчик, растёт когда Wall не изменился;
//   - Node — стабильный идентификатор клиента; нужен как финальный
//     tie-break, чтобы сравнение двух HLC всегда давало строгий порядок.
//
// Свойства:
//  1. Монотонность: два Now() одного узла никогда не дадут одинаковый
//     (Wall, Logical, Node).
//  2. Каузальность: если событие A causally предшествует B (A пришло в
//     сообщении, породившем B), то A < B в HLC-порядке.
//  3. Физическая близость: HLC.Wall не отстаёт от физических часов больше,
//     чем на «clock drift» (обычно <500мс в пределах одного ДЦ).
//
// HLC использует «безопасный» таймстемп — тот же алгоритм, что в CockroachDB
// и MongoDB. Полная корректность требует NTP; при расхождении часов больше
// 60 секунд узел с отстающими часами будет «проигрывать» по Wall всем.
// Митигируем: (а) логируем явный drift, (б) на Этапе 7 добавим мониторинг.
type HLC struct {
	Wall    int64  `json:"w"` // unix milliseconds
	Logical uint32 `json:"l"`
	Node    string `json:"n"`
}

// Compare возвращает -1/0/1. Порядок лексикографический: сначала Wall,
// потом Logical, потом Node.
//
// Почему Node — строка, а не int? Стабильный UUID/hex — стандартный
// идентификатор клиента в localStorage; приведение к int требует хэша
// и не даёт строгого порядка. Lexicographic sort по UUID достаточен.
func (a HLC) Compare(b HLC) int {
	switch {
	case a.Wall < b.Wall:
		return -1
	case a.Wall > b.Wall:
		return 1
	}
	switch {
	case a.Logical < b.Logical:
		return -1
	case a.Logical > b.Logical:
		return 1
	}
	return strings.Compare(a.Node, b.Node)
}

// After — convenience. a после b, то есть a старше.
func (a HLC) After(b HLC) bool { return a.Compare(b) > 0 }

// Zero — «начальный» HLC. При сравнении с любым реальным проигрывает.
func (a HLC) IsZero() bool { return a.Wall == 0 && a.Logical == 0 && a.Node == "" }

// Physical — «примерное» время события для UI.
func (a HLC) Physical() time.Time { return time.UnixMilli(a.Wall) }

// Clock — локальное состояние HLC. Небезопасна для конкурентного
// использования: в комнате актор вызывает её из Run-цикла, на клиенте —
// синхронно из rAF. Синхронизация на уровне структуры «дёшева» и
// предотвращает сюрпризы при переносе в другие места.
type Clock struct {
	mu     sync.Mutex
	node   string
	wall   int64
	logical uint32
}

// NewClock создаёт часы для узла node. Node должен быть непустым —
// иначе tie-break ломается, и два разных клиента могут выдать одинаковый
// HLC.
func NewClock(node string) *Clock {
	return &Clock{node: node}
}

// Node — идентификатор узла. Стабильный на всё время жизни Clock.
func (c *Clock) Node() string { return c.node }

// Now — продвинуть часы и вернуть новый timestamp.
//
// Логика: если физические часы шагнули вперёд относительно локального
// wall — сбрасываем logical на 0. Иначе инкрементируем logical; wall не
// меняется. Это даёт монотонность даже если системные часы «прыгают».
func (c *Clock) Now() HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	physical := time.Now().UnixMilli()
	if physical > c.wall {
		c.wall = physical
		c.logical = 0
	} else {
		c.logical++
	}
	return HLC{Wall: c.wall, Logical: c.logical, Node: c.node}
}

// Observe — продвинуть часы на основании полученного remote HLC.
// Вызывается на сервере, когда пришёл Op от клиента, и на клиенте, когда
// пришёл broadcast. Без Observe часы узла не «узнают» о росте времени
// у других, и каузальность может сломаться.
//
// Формула:
//
//	newWall = max(localWall, remoteWall, physical)
//	if newWall == localWall == remoteWall: logical = max(localL, remoteL) + 1
//	elif newWall == localWall:              logical = localL + 1
//	elif newWall == remoteWall:             logical = remoteL + 1
//	else:                                    logical = 0
//
// Это гарантирует: результат строго больше и local, и remote.
func (c *Clock) Observe(remote HLC) HLC {
	c.mu.Lock()
	defer c.mu.Unlock()
	physical := time.Now().UnixMilli()
	newWall := c.wall
	if remote.Wall > newWall {
		newWall = remote.Wall
	}
	if physical > newWall {
		newWall = physical
	}
	var newLogical uint32
	switch {
	case newWall == c.wall && newWall == remote.Wall:
		if c.logical > remote.Logical {
			newLogical = c.logical + 1
		} else {
			newLogical = remote.Logical + 1
		}
	case newWall == c.wall:
		newLogical = c.logical + 1
	case newWall == remote.Wall:
		newLogical = remote.Logical + 1
	default:
		newLogical = 0
	}
	c.wall = newWall
	c.logical = newLogical
	return HLC{Wall: c.wall, Logical: c.logical, Node: c.node}
}
