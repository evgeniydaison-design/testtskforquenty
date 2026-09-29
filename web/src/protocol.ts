// Mirror Go structs in internal/transport + internal/crdt + internal/presence.
// Держим типы «1-в-1» с сервером, чтобы не было сюрпризов на
// (de)serialization. Любая правка на сервере обязана отразиться здесь.

// 'image' — вставленные из буфера картинки: src = dataURL живёт в props,
// серверу всё равно что там — тип это просто строка (валидации нет).
export type ElementType = 'rect' | 'ellipse' | 'arrow' | 'line' | 'text' | 'diamond' | 'image';
export type OpKind = 'upsert' | 'delete';

// Стили — зеркало «магии» Excalidraw. Держим короткими строками,
// чтобы wire-формат не раздувался.
export type StrokeStyle = 'solid' | 'dashed' | 'dotted';
export type FillStyle = 'none' | 'solid' | 'hachure' | 'cross-hatch';

// HLC — triple (Wall, Logical, Node). Wire-имена совпадают с json:"..."
// в Go: w/l/n. Любое расхождение сломает roundtrip.
export interface HLC {
  w: number; // unix millis
  l: number; // uint32 logical counter
  n: string; // node id — clientId клиента или "server:<roomID>"
}

// Register — одна LWW-ячейка.
// Value — любой JSON-совместимый тип; конкретный ключ props определяет:
// «x/y/w/h/angle» = number, «stroke/fill/text/rank» = string,
// «points» = [number, number][], «del» = true.
export interface Register {
  v: unknown;
  ts: HLC;
}

// Element — id + type + карта свойств.
// Type иммутабиелен: второй upsert с другим type не переписывает его.
export interface Element {
  id: string;
  type: ElementType;
  props: Record<string, Register>;
}

// Op — частичное обновление. Props содержит ТОЛЬКО изменённые ключи.
// Это сердце «A двигает — Б красит»: два ops с пересекающимися id
// сливаются per-property, а неoverwrite целиком.
export interface Op {
  kind: OpKind;
  id: string;
  ts: HLC;
  type?: ElementType; // только при первичном upsert
  props?: Record<string, unknown>;
}

// Envelope — common frame.
export interface Envelope<T = unknown> {
  type: string;
  proto?: string;
  payload?: T;
}

// C→S payloads
export interface HelloPayload {
  clientId: string;
  name?: string;
  color?: string;
  // password — гейт комнаты: первый непустой при создании запирает её;
  // неверный => error-frame 'room_password' + close 1008 (без реконнекта).
  password?: string;
  lastSeq?: number;
}
export interface OpsPayload {
  batchId: string;
  ops: Op[];
}
export interface PingPayload {
  t: number;
}

// Presence — координаты в системе ДОКУМЕНТА, не канваса.
export interface PresencePoint {
  x: number;
  y: number;
}
export interface PresenceState {
  cursor?: PresencePoint | null;
  selection?: string[];
  tool?: string;
}
export interface PresenceEntry extends PresenceState {
  clientId: string;
  name: string;
  color: string;
  removed?: boolean;
}

// S→C payloads
export interface WelcomePayload {
  serverId: string;
  proto: string;
}
export interface SnapshotPayload {
  seq: number;
  elements: Element[];
}
export interface OpsBroadcastPayload {
  seq: number;
  origin: string;
  ops: Op[];
}
export interface AckPayload {
  batchId: string;
  seq: number;
}
export interface ErrorPayload {
  code: string;
  message: string;
}
export interface PongPayload {
  t: number;
}
export interface PresenceBroadcastPayload {
  entries: PresenceEntry[];
}

export const PROTO = 'v1';

// Константы имён свойств — зеркало Go PropX/PropY/…
// Держим lowercase, как в JSON.
export const Prop = {
  X: 'x',
  Y: 'y',
  W: 'w',
  H: 'h',
  Angle: 'angle',
  Stroke: 'stroke',
  Fill: 'fill',
  Text: 'text',
  Points: 'points',
  Rank: 'rank',
  Deleted: 'del',
  // --- Этап 5+ UI polish (Excalidraw-like) ---
  // Ширина штриха: 1 / 2 / 4 (тонкий/средний/жирный).
  StrokeWidth: 'sw',
  // Стиль штриха: 'solid' | 'dashed' | 'dotted'.
  StrokeStyle: 'ss',
  // Стиль заливки: 'none' | 'solid' | 'hachure' | 'cross-hatch'.
  FillStyle: 'fs',
  // «Рукописность»: 0 (чисто) / 1 (средне) / 2 (сильно).
  Roughness: 'ro',
  // Прозрачность 0..100 (в %).
  Opacity: 'op',
  // Для стрелки: id элемента-источника и получателя (binding).
  StartBind: 'sb',
  EndBind: 'eb',
  // --- Буфер обмена / авторство ---
  // Картинка: dataURL прямо в документе (LWW по свойству, как text).
  Src: 'src',
  // Кто создал элемент: clientId + отображаемое имя на момент создания.
  // Пишутся один раз в createElement; не перезаписываются (attribution
  // — не «последний редактор», а «автор»). Дубликат копирует оригинала.
  Author: 'au',
  AuthorName: 'an',
} as const;
export type PropName = (typeof Prop)[keyof typeof Prop];

