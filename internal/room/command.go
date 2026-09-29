package room

import (
	"errors"
	"sync"

	"board/internal/crdt"
	"board/internal/presence"
)

// Ошибки, возвращаемые наружу через Reply-каналы и методы.
var (
	ErrRoomStopped     = errors.New("room: остановлена")
	ErrDuplicateClient = errors.New("room: клиент с таким ID уже подключён")
	ErrCodecFailure    = errors.New("room: сбой сериализации")
	// ErrWrongPassword — комната под паролем, а hello не принёс его или
	// принёс неверный. Транспорт превращает это в error-frame
	// "room_password" + close 1008 (policy violation), чтобы клиент
	// НЕ переподключался в цикле, а показал модалку.
	ErrWrongPassword = errors.New("room: неверный пароль комнаты")
)

// Command — закрытый набор сообщений, принимаемых комнатой. Любой внешний
// код общается с комнатой только через Submit(cmd). Закрытие по каналу
// — единственный способ мутации состояния комнаты.
type Command interface{ cmdMarker() }

// Client — «ручка» подключения внутри комнаты.
//
// Send — буферизованный канал исходящих байтов. Writer-горутинка транспорта
// читает из него и пишет в WebSocket. Переполнение = медленный клиент →
// комната закрывает Done и забывает клиента. Это ядро backpressure:
// никто не может «запереть» актора блокировкой на медленном читателе.
type Client struct {
	ID    string
	Name  string
	Color string // стабильный цвет для presence-рендера

	// Password — из hello; используется ТОЛЬКО в moment Join для
	// гейта доступа к комнате. Хранить его в комнате-клиенте дольше
	// незачем, но поле должно доехать до актора через JoinCmd.
	Password string

	Send chan []byte
	Done chan struct{}

	closeOnce sync.Once
}

// NewClient создаёт клиент с буфером buf. Color обязателен: presence
// палитра должна быть одинаковой на всех вкладках, иначе пользователи
// будут видеть разные цвета для одного и того же участника — но
// совпадающий цвет не гарантируется, поэтому передаём его явно через
// hello и храним в комнате.
func NewClient(id, name, color string, buf int) *Client {
	if buf <= 0 {
		buf = 256
	}
	if color == "" {
		color = "#6b7280"
	}
	return &Client{
		ID:    id,
		Name:  name,
		Color: color,
		Send:  make(chan []byte, buf),
		Done:  make(chan struct{}),
	}
}

// Close идемпотентно сигналит writer'у остановиться.
func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.Done) })
}

// JoinCmd — просим добавить клиента и прислать ему снапшот.
// Reply должен быть буферизован (capacity=1), иначе блокируем актора.
type JoinCmd struct {
	Client *Client
	Reply  chan error
}

// LeaveCmd — клиент ушёл. Идемпотентно: повторный Leave — no-op.
type LeaveCmd struct {
	ClientID string
}

// ApplyOpsCmd — батч операций от origin=From.
//
// Server не «доверяет» Version/ClientID в payload: они переиспользуются как
// есть, но нормализуются в Doc.Apply. Если ClientID внутри ops не совпадает
// с From — считаем From авторитетным (транспорт уже проверил авторизацию).
type ApplyOpsCmd struct {
	From    string
	BatchID string
	Ops     []crdt.Op
}

// PresenceCmd — обновление эфемерного состояния участника.
//
// Не попадает в doc, не идёт в журнал, не требует ack. Клиент может слать
// столько кадров, сколько хочет; комната склеивает их на ближайший тик.
type PresenceCmd struct {
	ClientID string
	State    presence.State
}

// HistoryCmd — запрос диапазона записей из history-кольца.
//
// From/To инclusive; To==0 снимает верхнюю границу. Reply буферизован
// (cap=1): отправитель обязан прочитать ровно один раз.
type HistoryCmd struct {
	From  uint64
	To    uint64
	Reply chan []Record
}

func (JoinCmd) cmdMarker()     {}
func (LeaveCmd) cmdMarker()     {}
func (ApplyOpsCmd) cmdMarker()  {}
func (PresenceCmd) cmdMarker()  {}
func (HistoryCmd) cmdMarker()   {}
