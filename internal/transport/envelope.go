// Package transport — внешний интерфейс сервера: HTTP + WebSocket +
// сериализация сообщений. Никакой бизнес-логики здесь нет; всё, что делает
// пакет — это «переговорщик» между байтами в сокете и командами room.
//
// Формат сообщений — компактный JSON (envelope с type/proto/payload).
// Обоснование выбора:
//   - отладка: в браузере DevTools видно кадры глазами, без декодера;
//   - скорость итерации на этапах 1–3, когда семантика ещё меняется;
//   - нативная поддержка в браузере, без wasm-декодера.
// Бинарный кодек (Protobuf/CBOR/custom) имеет смысл на Этапе 7 под нагрузкой.
// Переход будет непрозрачным для room: достаточно заменить реализацию
// room.Codec и readLoop/writeLoop.
package transport

import (
	"encoding/json"
	"fmt"

	"board/internal/crdt"
	"board/internal/presence"
)

// Версия протокола. Клиент обязан прислать hello с тем же proto, иначе
// сервер отклонит соединение. Маппинг «proto → реализация кодека» —
// точка расширения для будущих v2/v3.
const ProtoV1 = "v1"

// Identifier'ы типов сообщений. Держим строки, а не iota: клиенту
// проще отлаживать, и JSON остаётся самодокументируемым.
const (
	TypeHello    = "hello"
	TypeWelcome  = "welcome"
	TypeSnapshot = "snapshot"
	TypeOps      = "ops"
	TypeAck      = "ack"
	TypeError    = "error"
	TypePing     = "ping"
	TypePong     = "pong"
	TypeBye      = "bye"
	// TypePresence — эфемерные обновления. В обе стороны: клиент шлёт
	// presence.State, сервер отзывается presence.Payload (batch).
	TypePresence = "presence"
)

// Envelope — «конверт» поверх JSON. Payload — json.RawMessage, чтобы
// комната/транспорт могли декодировать его по типу позже. Это же поле
// в будущем станет бинарным блоком без изменения envelope.
type Envelope struct {
	Type    string          `json:"type"`
	Proto   string          `json:"proto,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// --- payloads «от клиента к серверу» ---

// HelloPayload — первое сообщение после upgrade. ServerID клиента —
// стабильный UUID, хранящийся в localStorage: он же используется как
// tie-break в LWW. Color — стабильный цвет участника (hex); если пустой,
// комната подставит дефолтный.
type HelloPayload struct {
	ClientID string `json:"clientId"`
	Name     string `json:"name,omitempty"`
	Color    string `json:"color,omitempty"`
	// Password — гейт комнаты: первый непустой при создании комнаты
	// «запирает» её; дальше без точного совпадения Join отбивается
	// ошибкой room_password (close 1008).
	Password string `json:"password,omitempty"`
	// LastSeq зарезервирован под инкрементальный join (Этап 6).
	LastSeq uint64 `json:"lastSeq,omitempty"`
}

// OpsPayload — батч операций от клиента. BatchID клиент генерирует сам
// (uuid), сервер возвращает его в ack — это позволяет клиенту
// коррелировать подтверждения и реализовать retry-политику.
type OpsPayload struct {
	BatchID string    `json:"batchId"`
	Ops     []crdt.Op `json:"ops"`
}

// PingPayload — прикладной ping. Не путать с WS-control ping (nhooyr
// шлёт их сам). Нужен для измерения RTT со стороны приложения.
type PingPayload struct {
	T int64 `json:"t"`
}

// PresenceUpdatePayload — C→S: эфемерное состояние одного участника.
// По сути это presence.State «закрученный» в envelope; отдельный тип
// вводим, чтобы имя было говорящим и чтобы в будущем иметь место
// добавить метаданные (seq, ts) без изменения presence.State.
type PresenceUpdatePayload = presence.State

// PresenceBroadcastPayload — S→C: batch обновлений presence.
// Один кадр может содержать сразу несколько участников (coalescing).
// Для «removed» у элемента стоит Removed=true; клиент обязан убрать
// курсор этого участника из своего локального реестра.
type PresenceBroadcastPayload struct {
	Entries []presence.Entry `json:"entries"`
}

// --- payloads «от сервера к клиенту» ---

type WelcomePayload struct {
	ServerID string `json:"serverId"`
	Proto    string `json:"proto"`
}

type SnapshotPayload struct {
	Seq      uint64         `json:"seq"`
	Elements []crdt.Element `json:"elements"`
}

type OpsBroadcastPayload struct {
	Seq    uint64    `json:"seq"`
	Origin string    `json:"origin"`
	Ops    []crdt.Op `json:"ops"`
}

type AckPayload struct {
	BatchID string `json:"batchId"`
	Seq     uint64 `json:"seq"`
}

type ErrorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PongPayload struct {
	T int64 `json:"t"`
}

// --- JSON codec (реализует room.Codec) ---

// JSONCodec —stateless. Одна инстанция разделяется всеми комнатами.
type JSONCodec struct{}

// NewJSONCodec — конструктор, чтобы main не импортировал тип напрямую.
func NewJSONCodec() *JSONCodec { return &JSONCodec{} }

func (JSONCodec) MarshalSnapshot(seq uint64, els []crdt.Element) ([]byte, error) {
	return marshalEnvelope(TypeSnapshot, SnapshotPayload{Seq: seq, Elements: els})
}

func (JSONCodec) MarshalOps(seq uint64, origin string, ops []crdt.Op) ([]byte, error) {
	return marshalEnvelope(TypeOps, OpsBroadcastPayload{Seq: seq, Origin: origin, Ops: ops})
}

func (JSONCodec) MarshalAck(batchID string, seq uint64) ([]byte, error) {
	return marshalEnvelope(TypeAck, AckPayload{BatchID: batchID, Seq: seq})
}

func (JSONCodec) MarshalError(code, message string) ([]byte, error) {
	return marshalEnvelope(TypeError, ErrorPayload{Code: code, Message: message})
}

func (JSONCodec) MarshalPresence(entries []presence.Entry) ([]byte, error) {
	return marshalEnvelope(TypePresence, PresenceBroadcastPayload{Entries: entries})
}

// marshalEnvelope кодирует payload и заворачивает в envelope.
// Два Marshal-вызова вместо «сделать сразу всё» — чтобы ошибка в одном
// шаге не терялась: мы знаем, какая именно стадия упала.
func marshalEnvelope(t string, payload any) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("transport: marshal %s payload: %w", t, err)
	}
	env := Envelope{Type: t, Proto: ProtoV1, Payload: body}
	out, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("transport: marshal %s envelope: %w", t, err)
	}
	return out, nil
}

// decodeEnvelope разбирает входящий кадр.
func decodeEnvelope(data []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return env, fmt.Errorf("transport: decode envelope: %w", err)
	}
	return env, nil
}

// decodePayload разбирает payload в указатель dst.
func decodePayload(raw json.RawMessage, dst any) error {
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("transport: decode payload: %w", err)
	}
	return nil
}
