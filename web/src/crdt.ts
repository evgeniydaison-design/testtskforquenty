import {
  Prop,
  type Element,
  type ElementType,
  type HLC,
  type Op,
  type Register,
} from './protocol';

// --- HLC ---
//
// Зеркало internal/crdt/hlc.go. Держим идентичную формулу: если клиент
// и сервер сойдутся по алгоритму, «эхо» от сервера (после Observe+Now)
// гарантированно не затирает свежую локальную правку.
//
// Node — стабильный clientId. Для серверных broadcast-ов Node уже
// подменён на «server:<roomID>» — см. room.handleApplyOps.

export function hlcCompare(a: HLC, b: HLC): number {
  if (a.w !== b.w) return a.w < b.w ? -1 : 1;
  if (a.l !== b.l) return a.l < b.l ? -1 : 1;
  if (a.n === b.n) return 0;
  return a.n < b.n ? -1 : 1;
}

export function hlcAfter(a: HLC, b: HLC): boolean {
  return hlcCompare(a, b) > 0;
}

export function hlcIsZero(a: HLC): boolean {
  return a.w === 0 && a.l === 0 && a.n === '';
}

// Clock — локальное состояние HLC. Мьютекс не нужен: в браузере всё
// однопоточно в рамках microtask-очереди.
export class Clock {
  private wall = 0;
  private logical = 0;

  constructor(private readonly node: string) {}

  now(): HLC {
    const physical = Date.now();
    if (physical > this.wall) {
      this.wall = physical;
      this.logical = 0;
    } else {
      this.logical += 1;
    }
    return { w: this.wall, l: this.logical, n: this.node };
  }

  // observe — «узнать» о чужом времени. Формула Kulkarni: результат
  // строго больше и локального, и удалённого HLC.
  observe(remote: HLC): HLC {
    const physical = Date.now();
    let newWall = this.wall;
    if (remote.w > newWall) newWall = remote.w;
    if (physical > newWall) newWall = physical;
    let newLogical: number;
    if (newWall === this.wall && newWall === remote.w) {
      newLogical = Math.max(this.logical, remote.l) + 1;
    } else if (newWall === this.wall) {
      newLogical = this.logical + 1;
    } else if (newWall === remote.w) {
      newLogical = remote.l + 1;
    } else {
      newLogical = 0;
    }
    this.wall = newWall;
    this.logical = newLogical;
    return { w: this.wall, l: this.logical, n: this.node };
  }
}

// --- fractional index (LexoRank-lite) ---
//
// Зеркало internal/crdt/fractional.go. Вся арифметика — по ИНДЕКСАМ
// алфавита, не по байтам. Иначе «midpoint между '9' и 'a'» даёт '~'
// (вне алфавита), а «prepend перед 'i'» даёт «ii» (> «i»), а не «9».
const ALPHABET = '0123456789abcdefghijklmnopqrstuvwxyz';
const ALPHABET_LEN = ALPHABET.length; // 36
const MID_IDX = Math.floor(ALPHABET_LEN / 2); // 18 → 'i'
const MAX_RANK_LEN = 24;

function idxOf(c: string): number {
  return ALPHABET.indexOf(c);
}
function chrAt(i: number): string {
  return ALPHABET[i];
}

export function initialRank(): string {
  return chrAt(MID_IDX);
}

// before — строка R, строго меньше b.
function beforeStr(b: string): string {
  if (b === '') throw new Error('rank interval exhausted');
  if (b.length >= MAX_RANK_LEN) throw new Error('rank interval exhausted');
  const cb = idxOf(b[0]);
  if (cb < 0) throw new Error('rank interval exhausted');
  if (cb > 0) return chrAt(Math.floor(cb / 2));
  return '0' + beforeStr(b.slice(1));
}

// after — строка R, строго больше a.
function afterStr(a: string): string {
  if (a.length >= MAX_RANK_LEN) throw new Error('rank interval exhausted');
  return a + chrAt(MID_IDX);
}

function betweenStr(a: string, b: string): string {
  const n = Math.max(a.length, b.length);
  for (let i = 0; i < n; i++) {
    let ia: number, ib: number;
    if (i < a.length) {
      ia = idxOf(a[i]);
      if (ia < 0) throw new Error('rank interval exhausted');
    } else {
      ia = 0;
    }
    if (i < b.length) {
      ib = idxOf(b[i]);
      if (ib < 0) throw new Error('rank interval exhausted');
    } else {
      ib = ALPHABET_LEN;
    }
    if (ia === ib) continue;
    if (ib - ia > 1) {
      const mid = ia + Math.floor((ib - ia) / 2);
      return a.slice(0, i) + chrAt(mid);
    }
    // Зазора нет — «проваливаемся».
    if (i < a.length) {
      // a ещё не исчерпана: R = a[:i+1] + initialRank().
      const prefix = a.slice(0, i + 1);
      if (prefix.length + 1 >= MAX_RANK_LEN) throw new Error('rank interval exhausted');
      return prefix + initialRank();
    }
    // a исчерпана. R = a + before(b.slice(i)).
    if (a.length + 1 >= MAX_RANK_LEN) throw new Error('rank interval exhausted');
    return a + beforeStr(b.slice(i));
  }
  throw new Error('rank interval exhausted');
}

export function rankBetween(a: string, b: string): string {
  if (a === '' && b === '') return initialRank();
  if (a === '') return beforeStr(b);
  if (b === '') return afterStr(a);
  return betweenStr(a, b);
}

// --- LocalDoc ---
//
// Клиентская копия документа. Тот же алгоритм, что в crdt.Doc на сервере:
// per-property LWW + tombstone «del». Никакой сетевой логики — ops
// прилетают из main.ts, расходятся по применению.
//
// Важно: apply() НЕ создаёт новые элементы из «пустяка» для delete,
// если у нас уже есть live-версия. Но tombstone-скелет кладём всегда —
// иначе запоздавший upsert «воскресит» удалённый, что расходится с
// сервером.

export class LocalDoc {
  private elements = new Map<string, Element>();

  apply(op: Op): boolean {
    let el = this.elements.get(op.id);
    if (!el) {
      const typ: ElementType = op.type ?? 'rect';
      el = { id: op.id, type: typ, props: {} };
      this.elements.set(op.id, el);
    } else if (op.type && !el.type) {
      el.type = op.type;
    }

    if (op.kind === 'upsert') {
      const props = op.props ?? {};
      for (const [name, value] of Object.entries(props)) {
        // «del» не должен ехать в upsert-props. Если прилетел — игнорируем.
        if (name === Prop.Deleted) continue;
        setRegister(el, name, value, op.ts);
      }
      // Resurrect: если раньше был tombstone и TS(op) строго больше него —
      // снимаем del. Иначе delete всё ещё «новее», и мы не трогаем состояние.
      const del = el.props[Prop.Deleted];
      if (del) {
        if (!hlcAfter(op.ts, del.ts)) return false;
        delete el.props[Prop.Deleted];
        return true;
      }
      return true;
    }

    if (op.kind === 'delete') {
      const del = el.props[Prop.Deleted];
      if (del && del.v === true && !hlcAfter(op.ts, del.ts)) return false;
      setRegister(el, Prop.Deleted, true, op.ts);
      return true;
    }
    return false;
  }

  loadSnapshot(elements: Element[]): void {
    this.elements.clear();
    for (const e of elements) {
      // Держим копию: чужой snapshot может быть «переиспользован».
      const props: Record<string, Register> = {};
      for (const [k, r] of Object.entries(e.props ?? {})) {
        props[k] = { v: r.v, ts: r.ts };
      }
      this.elements.set(e.id, { id: e.id, type: e.type, props });
    }
  }

  // live — элементы в z-order (по rank). Пустой rank = «в конец», как
  // на сервере. Это то, что рисует board.render.
  live(): Element[] {
    const out: Element[] = [];
    for (const e of this.elements.values()) {
      const del = e.props[Prop.Deleted];
      if (del && del.v === true) continue;
      out.push(e);
    }
    out.sort((a, b) => {
      const ra = (a.props[Prop.Rank]?.v as string) || '\uffff';
      const rb = (b.props[Prop.Rank]?.v as string) || '\uffff';
      if (ra !== rb) return ra < rb ? -1 : 1;
      return a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
    });
    return out;
  }

  get(id: string): Element | undefined {
    return this.elements.get(id);
  }
}

function setRegister(el: Element, name: string, value: unknown, ts: HLC): void {
  const cur = el.props[name];
  if (!cur) {
    el.props[name] = { v: value, ts };
    return;
  }
  if (!hlcIsZero(cur.ts) && !hlcAfter(ts, cur.ts)) return;
  cur.v = value;
  cur.ts = ts;
}

// --- value readers ---
// Читаем с приведением типа: JSON roundtrip даёт float64 для чисел,
// string для строк, массив для points. Если свойства нет — fallback.

export function readNum(el: Element, name: string, dflt = 0): number {
  const r = el.props[name];
  if (!r) return dflt;
  const v = r.v;
  return typeof v === 'number' ? v : dflt;
}

export function readStr(el: Element, name: string, dflt = ''): string {
  const r = el.props[name];
  if (!r) return dflt;
  const v = r.v;
  return typeof v === 'string' ? v : dflt;
}

export function readBool(el: Element, name: string): boolean {
  const r = el.props[name];
  if (!r) return false;
  return r.v === true;
}

export function readPoints(el: Element): [number, number][] {
  const r = el.props[Prop.Points];
  if (!r) return [];
  const v = r.v;
  if (!Array.isArray(v)) return [];
  const out: [number, number][] = [];
  for (const p of v) {
    if (Array.isArray(p) && p.length >= 2) {
      const x = typeof p[0] === 'number' ? p[0] : 0;
      const y = typeof p[1] === 'number' ? p[1] : 0;
      out.push([x, y]);
    }
  }
  return out;
}
