import {
  Clock,
  LocalDoc,
  initialRank,
  rankBetween,
  readBool,
  readNum,
  readPoints,
  readStr,
} from './crdt';
import { ZERO_TS, UndoStack } from './history';
import {
  hashSeed,
  hachureFillRect,
  roughDoubleLine,
  roughEllipse,
  strokePts,
} from './rough';
import {
  catmullRomToBezier,
  emaPoint,
  hitTestBeziers,
  stableQuadraticPath,
  type Pt,
} from './smoothing';
import {
  Prop,
  type Element,
  type ElementType,
  type FillStyle,
  type Op,
  type StrokeStyle,
} from './protocol';
import type { PresenceTracker } from './presence';
import { Viewport } from './viewport';

export type Tool =
  | 'select'
  | 'rect'
  | 'ellipse'
  | 'diamond'
  | 'line'
  | 'arrow'
  | 'text'
  | 'erase';

export interface BoardOpts {
  doc: LocalDoc;
  clientID: string;
  // clientName — отображаемое имя; пишется в элементы как авторство
  // (Prop.AuthorName) и светится в hover-подсказке «кто сделал».
  clientName: string;
  sendOps: (ops: Op[]) => void;
  presence: PresenceTracker;
}

// 8 маркеров resize + 1 rotation. Порядок важен: hit-test идёт по
// этому же списку, чтобы «верхний» (ближайший к курсору) победил.
type Handle = 'nw' | 'n' | 'ne' | 'e' | 'se' | 's' | 'sw' | 'w' | 'rot';

const HANDLE_SIZE = 8; // экранных пикселей
const MIN_SIZE = 4; // минимальный w/h после resize

// Подписи для hover-подсказки «кто сделал» — по-человечески, на
// русском, чтобы тултип не выглядел отладочным выводом.
const TYPE_LABEL_RU: Record<string, string> = {
  rect: 'прямоугольник',
  ellipse: 'эллипс',
  diamond: 'ромб',
  line: 'линия',
  arrow: 'стрелка',
  text: 'текст',
  image: 'картинка',
};

export class Board {
  tool: Tool = 'rect';
  strokeColor = '#1e1e1e';
  fillColor = 'transparent';
  fillStyle: FillStyle = 'none';
  strokeWidth = 2;
  strokeStyle: StrokeStyle = 'solid';
  roughness = 1;
  opacity = 100;
  selection: string[] = [];

  readonly viewport = new Viewport();
  private readonly clock: Clock;
  private raf = 0;
  private presenceRunning = false;

  // Drag существующего элемента.
  private dragTarget: string | null = null;
  private dragOffsetX = 0;
  private dragOffsetY = 0;
  private dragLastEmitAt = 0;

  // Resize / rotate.
  private activeHandle: Handle | null = null;
  private resizeStart: { id: string; x: number; y: number; w: number; h: number } | null = null;
  private rotateStart: { id: string; cx: number; cy: number; startAngle: number } | null = null;

  // Marquee (рамка множественного выделения).
  private marqueeStart: Pt | null = null;
  private marqueeEnd: Pt | null = null;

  // Live-рисование штриха. drawingId — элемент, который рисуется
  // СЕЙЧАС (line/arrow). Штрих рендерим из livePts (полная частота
  // pointermove + EMA), а НЕ из doc — doc обновляется с троттлингом,
  // и хвост «дёргался» бы. stableQuadraticPath гарантирует: новые
  // точки только удлиняют хвост, уже нарисованное НЕ меняется.
  private drawingId: string | null = null;
  private livePts: Pt[] = [];
  private liveCursor: Pt | null = null;
  private liveStart: Pt | null = null;
  private liveShift = false;

  // Hover-attribution: наведение курсора на элемент показывает,
  // кто его создал (au/an пишутся один раз при создании). hoverId
  // пересчитывается в pointermove, тултип — DOM-узел над канвасом
  // (на канвасе он бы мигал из-за рендера и лез в zoom-координаты).
  private hoverId: string | null = null;
  private hoverTip: HTMLDivElement | null = null;

  // Inline-редактирование текста: textarea поверх позиции элемента.
  // Enter — перенос строки, Ctrl+Enter/blur — commit, Escape — отмена
  // (новый пустой элемент удаляется). prompt был «слишком строго»:
  // созданный текст нельзя было изменить.
  private editingId: string | null = null;
  private editIsNew = false;
  private editorEl: HTMLTextAreaElement | null = null;

  // Картинки: src (dataURL) -> загруженный Image. Кэш по src, поэтому
  // дубликаты и повторные документы не декодируют заново; onload
  // перерисовывает канвас, когда картинка дошла.
  private readonly imgCache = new Map<string, HTMLImageElement>();

  // Куда вставлять из буфера: туда, где был курсор мыши в мире; если
  // мышь не двигалась вовсе — центр вьюпорта (pasteOrigin).
  private cursorSeen = false;

  // Pan (пробел+drag или средняя кнопка).
  private panning = false;
  private panStartX = 0;
  private panStartY = 0;
  private spaceDown = false;

  // Undo/redo.
  private readonly history = new UndoStack();
  private pendingGesture: {
    inverse: Map<string, Op>;
    redo: Map<string, Op>;
  } | null = null;

  constructor(
    private readonly canvas: HTMLCanvasElement,
    private readonly opts: BoardOpts,
  ) {
    this.clock = new Clock(opts.clientID);
    this.bindPointer();
    this.bindKeyboard();
    this.resize();
    window.addEventListener('resize', () => this.resize());
    this.startPresenceLoop();
  }

  // --- public API ---

  setTool(t: Tool): void {
    this.tool = t;
    this.canvas.style.cursor = t === 'select' ? 'default' : t === 'erase' ? 'cell' : 'crosshair';
    this.opts.presence.recordCursor(this.lastCursorX, this.lastCursorY, this.tool, this.selection);
  }

  setStrokeColor(c: string): void { this.strokeColor = c; }
  setFillColor(c: string): void { this.fillColor = c; }
  setFillStyle(s: FillStyle): void { this.fillStyle = s; }
  setStrokeWidth(w: number): void { this.strokeWidth = w; }
  setStrokeStyle(s: StrokeStyle): void { this.strokeStyle = s; }
  setRoughness(r: number): void { this.roughness = r; }
  setOpacity(o: number): void { this.opacity = o; }

  loadSnapshot(elements: Element[], seq: number): void {
    this.opts.doc.loadSnapshot(elements);
    for (const e of elements) {
      for (const r of Object.values(e.props ?? {})) {
        this.clock.observe(r.ts);
      }
    }
    void seq;
    this.scheduleRender();
  }

  applyRemote(ops: Op[], seq: number, origin: string): void {
    for (const op of ops) {
      this.clock.observe(op.ts);
      if (origin === this.opts.clientID) continue;
      this.opts.doc.apply(op);
    }
    void seq;
    this.scheduleRender();
  }

  onAck(_batchId: string): void {}

  setColorForSelection(stroke: string, fill: string): void {
    if (this.selection.length === 0) return;
    const props: Record<string, unknown> = { [Prop.Stroke]: stroke };
    if (fill !== undefined) props[Prop.Fill] = fill;
    this.emitPartialMany(this.selection, props);
  }

  setStrokeWidthForSelection(w: number): void {
    if (this.selection.length === 0) return;
    this.emitPartialMany(this.selection, { [Prop.StrokeWidth]: w });
  }

  setStrokeStyleForSelection(s: StrokeStyle): void {
    if (this.selection.length === 0) return;
    this.emitPartialMany(this.selection, { [Prop.StrokeStyle]: s });
  }

  setFillStyleForSelection(s: FillStyle): void {
    if (this.selection.length === 0) return;
    this.emitPartialMany(this.selection, { [Prop.FillStyle]: s });
  }

  setRoughnessForSelection(r: number): void {
    if (this.selection.length === 0) return;
    this.emitPartialMany(this.selection, { [Prop.Roughness]: r });
  }

  clearSelection(): void { this.setSelection([]); }

  // selectAll — Ctrl+A, как в Excalidraw: выделить все живые элементы.
  selectAll(): void {
    this.setSelection(this.opts.doc.live().map((el) => el.id));
  }

  setSelection(ids: string[]): void {
    this.selection = ids;
    this.opts.presence.recordCursor(this.lastCursorX, this.lastCursorY, this.tool, this.selection);
    this.scheduleRender();
  }

  toggleSelection(id: string): void {
    const next = this.selection.slice();
    const i = next.indexOf(id);
    if (i >= 0) next.splice(i, 1);
    else next.push(id);
    this.setSelection(next);
  }

  bringForward(): void {
    if (this.selection.length === 0) return;
    this.beginGesture();
    for (const id of this.selection) this.moveRank(id, +1);
    this.endGesture();
  }
  sendBackward(): void {
    if (this.selection.length === 0) return;
    this.beginGesture();
    for (const id of this.selection) this.moveRank(id, -1);
    this.endGesture();
  }

  private moveRank(id: string, dir: 1 | -1): void {
    const list = this.opts.doc.live();
    const idx = list.findIndex((e) => e.id === id);
    if (idx < 0) return;
    const target = idx + dir;
    if (target < 0 || target >= list.length) return;
    let lo = '';
    let hi = '';
    if (dir > 0) {
      lo = readStr(list[idx + 1], Prop.Rank, '');
      hi = idx + 2 < list.length ? readStr(list[idx + 2], Prop.Rank, '') : '';
      if (lo === '') lo = initialRank();
    } else {
      const prev = list[idx - 1];
      hi = readStr(prev, Prop.Rank, '');
      lo = idx - 2 >= 0 ? readStr(list[idx - 2], Prop.Rank, '') : '';
      if (hi === '') hi = initialRank();
    }
    if (lo === hi) return;
    try {
      const nr = rankBetween(lo, hi);
      this.emitPartial(id, { [Prop.Rank]: nr });
    } catch {
      // Rebalance — future.
    }
  }

  deleteSelection(): void {
    if (this.selection.length === 0) return;
    const ids = this.selection.slice();
    this.setSelection([]);
    this.beginGesture();
    for (const id of ids) this.deleteElement(id);
    this.endGesture();
  }

  duplicateSelection(offset = 20): void {
    if (this.selection.length === 0) return;
    const newIds: string[] = [];
    this.beginGesture();
    for (const id of this.selection) {
      const el = this.opts.doc.get(id);
      if (!el || readBool(el, Prop.Deleted)) continue;
      const props: Record<string, unknown> = {};
      for (const [k, r] of Object.entries(el.props)) {
        if (k === Prop.Deleted || k === Prop.Rank) continue;
        props[k] = r.v;
      }
      props[Prop.X] = readNum(el, Prop.X, 0) + offset;
      props[Prop.Y] = readNum(el, Prop.Y, 0) + offset;
      const nid = crypto.randomUUID();
      this.createElement(nid, el.type, props);
      newIds.push(nid);
    }
    this.endGesture();
    this.setSelection(newIds);
  }

  // --- undo/redo ---

  undo(): boolean {
    const f = this.history.popUndo();
    if (!f || f.inverse.length === 0) return false;
    const ops = f.inverse.map((op) => ({ ...op, ts: this.clock.now() }));
    for (const op of ops) this.opts.doc.apply(op);
    this.opts.sendOps(ops);
    this.scheduleRender();
    return true;
  }

  redo(): boolean {
    const f = this.history.popRedo();
    if (!f || f.redo.length === 0) return false;
    const ops = f.redo.map((op) => ({ ...op, ts: this.clock.now() }));
    for (const op of ops) this.opts.doc.apply(op);
    this.opts.sendOps(ops);
    this.scheduleRender();
    return true;
  }

  canUndo(): boolean { return this.history.canUndo(); }
  canRedo(): boolean { return this.history.canRedo(); }
  historyDepth(): { undo: number; redo: number } { return this.history.depth(); }
  resetHistory(): void {
    this.history.clear();
    this.pendingGesture = null;
  }

  beginGesture(): void {
    if (!this.pendingGesture) {
      this.pendingGesture = { inverse: new Map(), redo: new Map() };
    }
  }
  endGesture(): void {
    const g = this.pendingGesture;
    if (!g) return;
    this.pendingGesture = null;
    const inverse = [...g.inverse.values()];
    const redo = [...g.redo.values()];
    if (inverse.length || redo.length) this.history.record({ inverse, redo });
  }

  // --- emit helpers ---

  createElement(
    id: string,
    type: ElementType,
    props: Record<string, unknown>,
  ): void {
    const ts = this.clock.now();
    const withDefaults: Record<string, unknown> = {
      [Prop.Rank]: this.nextTopRank(),
      [Prop.Angle]: 0,
      [Prop.StrokeWidth]: this.strokeWidth,
      [Prop.StrokeStyle]: this.strokeStyle,
      [Prop.FillStyle]: this.fillStyle,
      [Prop.Roughness]: this.roughness,
      [Prop.Opacity]: this.opacity,
      ...props,
    };
    const op: Op = { kind: 'upsert', id, ts, type, props: withDefaults };
    // Авторство: пишем ровно один раз, при создании. duplicateSelection
    // копирует props вместе с автором (копия «помнит» оригинала), а
    // последующие emitPartial эти ключи не трогают — значит hover
    // показывает СОЗДАТЕЛЯ, а не последнего редактора.
    if (withDefaults[Prop.Author] === undefined) {
      withDefaults[Prop.Author] = this.opts.clientID;
      withDefaults[Prop.AuthorName] = this.opts.clientName;
    }
    this.emitBatch([op]);
  }

  emitPartialMany(ids: string[], props: Record<string, unknown>): void {
    const ops: Op[] = [];
    for (const id of ids) {
      const ts = this.clock.now();
      ops.push({ kind: 'upsert', id, ts, props });
    }
    this.emitBatch(ops);
  }

  private emitPartial(id: string, props: Record<string, unknown>): void {
    const ts = this.clock.now();
    const op: Op = { kind: 'upsert', id, ts, props };
    this.emitBatch([op]);
  }

  private deleteElement(id: string): void {
    const cur = this.opts.doc.get(id);
    if (!cur) return;
    if (readBool(cur, Prop.Deleted)) return;
    const ts = this.clock.now();
    const op: Op = { kind: 'delete', id, ts };
    this.emitBatch([op]);
  }

  private emitBatch(ops: Op[]): void {
    if (ops.length === 0) return;
    type Before = { type: ElementType; props: Record<string, unknown> } | null;
    const before = new Map<string, Before>();
    for (const op of ops) {
      if (before.has(op.id)) continue;
      const el = this.opts.doc.get(op.id);
      if (!el) { before.set(op.id, null); continue; }
      if (readBool(el, Prop.Deleted)) { before.set(op.id, null); continue; }
      const props: Record<string, unknown> = {};
      for (const [k, r] of Object.entries(el.props)) {
        if (k === Prop.Deleted) continue;
        props[k] = r.v;
      }
      before.set(op.id, { type: el.type, props });
    }
    for (const op of ops) this.opts.doc.apply(op);
    this.opts.sendOps(ops);
    const { inverse, redo } = this.buildFrame(ops, before);
    this.commitFrame(inverse, redo);
    this.scheduleRender();
  }

  private buildFrame(
    ops: Op[],
    before: Map<string, { type: ElementType; props: Record<string, unknown> } | null>,
  ): { inverse: Op[]; redo: Op[] } {
    const inverse: Op[] = [];
    const redo: Op[] = [];
    for (const op of ops) {
      const b = before.get(op.id) ?? null;
      redo.push({
        kind: op.kind,
        id: op.id,
        ts: ZERO_TS,
        type: op.type,
        props: op.props ? { ...op.props } : undefined,
      });
      if (op.kind === 'delete') {
        if (b) {
          inverse.push({
            kind: 'upsert',
            id: op.id,
            ts: ZERO_TS,
            type: b.type,
            props: { ...b.props },
          });
        }
        continue;
      }
      if (!b) {
        inverse.push({ kind: 'delete', id: op.id, ts: ZERO_TS });
        continue;
      }
      const changedKeys = Object.keys(op.props ?? {});
      const partial: Record<string, unknown> = {};
      let any = false;
      for (const k of changedKeys) {
        if (k in b.props) { partial[k] = b.props[k]; any = true; }
      }
      if (any) {
        inverse.push({ kind: 'upsert', id: op.id, ts: ZERO_TS, props: partial });
      }
    }
    return { inverse, redo };
  }

  private commitFrame(inverse: Op[], redo: Op[]): void {
    if (!this.pendingGesture) {
      if (inverse.length || redo.length) this.history.record({ inverse, redo });
      return;
    }
    const g = this.pendingGesture;
    for (const op of inverse) if (!g.inverse.has(op.id)) g.inverse.set(op.id, op);
    for (const op of redo) {
      const ex = g.redo.get(op.id);
      if (!ex) { g.redo.set(op.id, op); continue; }
      const mergedProps: Record<string, unknown> = {
        ...(ex.props ?? {}),
        ...(op.props ?? {}),
      };
      g.redo.set(op.id, {
        kind: op.kind,
        id: op.id,
        ts: ZERO_TS,
        type: op.type ?? ex.type,
        props: mergedProps,
      });
    }
  }

  private nextTopRank(): string {
    const list = this.opts.doc.live();
    let max = '';
    for (const e of list) {
      const r = readStr(e, Prop.Rank, '');
      if (r > max) max = r;
    }
    if (max === '') return initialRank();
    try { return rankBetween(max, ''); } catch { return max; }
  }

  // --- pointer handling ---

  private lastCursorX = 0;
  private lastCursorY = 0;

  // Экранные координаты относительно canvas.
  private screenPos(e: PointerEvent | WheelEvent | MouseEvent): Pt {
    const r = this.canvas.getBoundingClientRect();
    return [e.clientX - r.left, e.clientY - r.top];
  }

  // Мировые координаты (то, что кладём в документ).
  private worldPos(e: PointerEvent | WheelEvent | MouseEvent): Pt {
    const [sx, sy] = this.screenPos(e);
    return this.viewport.toWorld(sx, sy);
  }

  private bindPointer(): void {
    let drawing = false;
    let moved = false;
    let currentId = '';
    let startWX = 0;
    let startWY = 0;
    let lastEmitAt = 0;
    const rawPts: Pt[] = [];

    this.canvas.addEventListener('pointerdown', (e: PointerEvent) => {
      this.canvas.focus();
      // Клик «мимо» при открытом редакторе = зафиксировать правку:
      // blur textarea приходит позже pointerdown, иначе первый клик
      // ещё и создал бы лишнее выделение/фигуру поверх старого текста.
      if (this.editingId) this.commitTextEdit();
      if (this.hoverId) {
        this.hoverId = null;
        this.updateHoverTip();
      }
      // Средняя кнопка или Space → pan.
      if (e.button === 1 || (e.button === 0 && this.spaceDown)) {
        this.panning = true;
        const [sx, sy] = this.screenPos(e);
        this.panStartX = sx;
        this.panStartY = sy;
        this.canvas.setPointerCapture(e.pointerId);
        this.canvas.style.cursor = 'grabbing';
        e.preventDefault();
        return;
      }
      if (e.button !== 0) return;

      const [wx, wy] = this.worldPos(e);
      moved = false;

      // 1. Resize/rotate handles — имеют приоритет над всем остальным.
      if (this.tool === 'select' && this.selection.length === 1) {
        const h = this.hitHandle(wx, wy);
        if (h) {
          const el = this.opts.doc.get(this.selection[0]);
          if (el) {
            this.activeHandle = h;
            const bb = this.bboxOf(el);
            if (h === 'rot') {
              this.rotateStart = {
                id: el.id,
                cx: bb.x + bb.w / 2,
                cy: bb.y + bb.h / 2,
                startAngle: Math.atan2(wy - (bb.y + bb.h / 2), wx - (bb.x + bb.w / 2)) -
                  readNum(el, Prop.Angle, 0),
              };
            } else {
              this.resizeStart = { id: el.id, x: bb.x, y: bb.y, w: bb.w, h: bb.h };
            }
            this.canvas.setPointerCapture(e.pointerId);
            this.beginGesture();
            return;
          }
        }
      }

      // 2. Erase: клик = удалить верхний под курсором.
      if (this.tool === 'erase') {
        const hit = this.hitTestTop(wx, wy);
        if (hit) {
          this.setSelection([]);
          this.deleteElement(hit);
        }
        return;
      }

      // 3. Select: клик по элементу = выбрать и подготовить drag.
      if (this.tool === 'select') {
        let hit = this.hitTestTop(wx, wy);
        if (hit) {
          if (e.shiftKey) {
            this.toggleSelection(hit);
          } else if (!this.selection.includes(hit)) {
            this.setSelection([hit]);
          }
          if (e.altKey) {
            // Alt+drag = дубликат и перетаскивание копии (Excalidraw).
            this.duplicateSelection(0);
            if (this.selection.length > 0) hit = this.selection[0];
          }
          const el = this.opts.doc.get(hit);
          if (el) {
            this.dragTarget = hit;
            this.dragOffsetX = wx - readNum(el, Prop.X, 0);
            this.dragOffsetY = wy - readNum(el, Prop.Y, 0);
            this.canvas.setPointerCapture(e.pointerId);
            this.beginGesture();
          }
        } else {
          // Marquee.
          this.marqueeStart = [wx, wy];
          this.marqueeEnd = [wx, wy];
          if (!e.shiftKey) this.setSelection([]);
          this.canvas.setPointerCapture(e.pointerId);
        }
        return;
      }

      // 4. Text: клик = создать элемент и СРАЗУ начать inline-правка
      // (как в Excalidraw). Пустой текст по Escape — автоудаление.
      if (this.tool === 'text') {
        const id = crypto.randomUUID();
        this.createElement(id, 'text', {
          [Prop.X]: wx,
          [Prop.Y]: wy,
          [Prop.W]: 40,
          [Prop.H]: 24,
          [Prop.Stroke]: this.strokeColor,
          [Prop.Fill]: 'transparent',
          [Prop.Text]: '',
        });
        this.setSelection([id]);
        this.beginTextEdit(id, true);
        return;
      }

      // 5. Рисование rect/ellipse/diamond/line/arrow.
      this.canvas.setPointerCapture(e.pointerId);
      drawing = true;
      startWX = wx;
      startWY = wy;
      rawPts.length = 0;
      rawPts.push([0, 0]);
      currentId = crypto.randomUUID();
      this.beginGesture();
      this.emitDrawUpdate(currentId, startWX, startWY, wx, wy, rawPts.slice(), true, e.shiftKey);
      lastEmitAt = performance.now();
      if (this.tool === 'line' || this.tool === 'arrow') {
        this.drawingId = currentId;
        this.livePts = [[wx, wy]];
        this.liveStart = [wx, wy];
        this.liveCursor = null;
        this.liveShift = e.shiftKey;
      }
    });

    this.canvas.addEventListener('pointermove', (e: PointerEvent) => {
      const [sx, sy] = this.screenPos(e);
      const [wx, wy] = this.worldPos(e);
      this.lastCursorX = wx;
      this.lastCursorY = wy;
      this.cursorSeen = true;
      this.opts.presence.recordCursor(wx, wy, this.tool, this.selection);

      // Hover-attribution: считаем только при неподписанных кнопках —
      // во время drag/resize/рисования и так достаточно «шума».
      const idle = e.buttons === 0 && !this.editingId;
      const hid = idle ? this.hitTestTop(wx, wy) ?? null : null;
      if (hid !== this.hoverId) {
        this.hoverId = hid;
        this.updateHoverTip();
      }

      if (this.panning) {
        this.viewport.pan(sx - this.panStartX, sy - this.panStartY);
        this.panStartX = sx;
        this.panStartY = sy;
        this.scheduleRender();
        return;
      }

      // Marquee.
      if (this.marqueeStart) {
        this.marqueeEnd = [wx, wy];
        this.scheduleRender();
        return;
      }

      // Rotate.
      if (this.activeHandle === 'rot' && this.rotateStart) {
        const ang = Math.atan2(wy - this.rotateStart.cy, wx - this.rotateStart.cx) -
          this.rotateStart.startAngle;
        this.emitPartial(this.rotateStart.id, { [Prop.Angle]: ang });
        return;
      }

      // Resize.
      if (this.activeHandle && this.resizeStart) {
        this.applyResize(wx, wy);
        return;
      }

      // Drag существующего.
      if (this.dragTarget) {
        const now = performance.now();
        if (now - this.dragLastEmitAt < 33) return;
        this.dragLastEmitAt = now;
        moved = true;
        const nx = wx - this.dragOffsetX;
        const ny = wy - this.dragOffsetY;
        this.emitPartial(this.dragTarget, { [Prop.X]: nx, [Prop.Y]: ny });
        return;
      }

      // Erase by drag.
      if (this.tool === 'erase' && e.buttons === 1) {
        const hit = this.hitTestTop(wx, wy);
        if (hit) {
          this.deleteElement(hit);
          moved = true;
        }
        return;
      }

      if (!drawing) return;
      moved = true;
      if (this.tool === 'line' || this.tool === 'arrow') {
        // Полная частота pointermove: EMA-сглаживание + порог в
        // экранных пикселях (не зависимо от зума — при 4x мира
        // нужно 4 раза меньше точек).
        const prev = this.livePts.length > 0 ? this.livePts[this.livePts.length - 1] : null;
        const sm = emaPoint(prev, [wx, wy], 0.5);
        const minDist = 2 / this.viewport.scale;
        if (!prev || Math.hypot(sm[0] - prev[0], sm[1] - prev[1]) >= minDist) {
          this.livePts.push(sm);
        }
        this.liveCursor = [wx, wy];
        this.liveShift = e.shiftKey;
        this.scheduleRender();
      }
      const now = performance.now();
      if (now - lastEmitAt < 33) return;
      lastEmitAt = now;
      if (this.tool === 'line') {
        rawPts.length = 0;
        for (const p of this.livePts) rawPts.push([p[0] - startWX, p[1] - startWY]);
      }
      this.emitDrawUpdate(currentId, startWX, startWY, wx, wy, rawPts.slice(), false, e.shiftKey);
    });

    const finish = (e: PointerEvent) => {
      const [wx, wy] = this.worldPos(e);

      if (this.panning) {
        this.panning = false;
        this.canvas.releasePointerCapture?.(e.pointerId);
        this.canvas.style.cursor = this.tool === 'select' ? 'default' : 'crosshair';
        return;
      }

      if (this.marqueeStart && this.marqueeEnd) {
        const ids = this.hitTestRect(this.marqueeStart, this.marqueeEnd);
        this.setSelection(ids);
        this.marqueeStart = null;
        this.marqueeEnd = null;
        this.canvas.releasePointerCapture?.(e.pointerId);
        this.scheduleRender();
        return;
      }

      if (this.activeHandle) {
        this.activeHandle = null;
        this.resizeStart = null;
        this.rotateStart = null;
        this.endGesture();
        this.canvas.releasePointerCapture?.(e.pointerId);
        return;
      }

      if (this.dragTarget) {
        this.dragTarget = null;
        this.endGesture();
        this.canvas.releasePointerCapture?.(e.pointerId);
        return;
      }

      if (!drawing) return;
      if (this.tool === 'line') {
        rawPts.length = 0;
        for (const p of this.livePts) rawPts.push([p[0] - startWX, p[1] - startWY]);
        rawPts.push([wx - startWX, wy - startWY]);
      }
      // Финальный op — всегда.
      this.emitDrawUpdate(currentId, startWX, startWY, wx, wy, rawPts.slice(), false, e.shiftKey);
      // Крошечные «тычки» отбрасываем (Excalidraw тоже):
      const tiny = Math.hypot(wx - startWX, wy - startWY) < 3 / this.viewport.scale;
      if (tiny && (this.tool === 'arrow' || (this.tool === 'line' && rawPts.length < 2))) {
        this.deleteElement(currentId);
      }
      drawing = false;
      currentId = '';
      this.drawingId = null;
      this.livePts = [];
      this.liveCursor = null;
      this.liveStart = null;
      this.endGesture();
      this.canvas.releasePointerCapture?.(e.pointerId);

      if (!moved) {
        const hit = this.hitTestTop(wx, wy);
        this.setSelection(hit ? [hit] : []);
      }
    };
    this.canvas.addEventListener('pointerup', finish);
    this.canvas.addEventListener('pointercancel', finish);

    // Двойной клик по тексту = inline-редактирование (Excalidraw-style).
    // Одиночный клик по своему тексту при «select» остаётся drag'ом —
    // не конфликтуют: dblclick приходит после второго pointerup.
    this.canvas.addEventListener('dblclick', (e: MouseEvent) => {
      if (this.editingId) return;
      const [wx, wy] = this.worldPos(e);
      const hit = this.hitTestTop(wx, wy);
      if (!hit) return;
      const el = this.opts.doc.get(hit);
      if (!el || el.type !== 'text') return;
      this.setSelection([hit]);
      this.beginTextEdit(hit, false);
    });

    this.canvas.addEventListener('pointerleave', () => {
      this.opts.presence.recordCursor(-1, -1, this.tool, this.selection);
      if (this.hoverId) {
        this.hoverId = null;
        this.updateHoverTip();
      }
    });

    // Zoom: ctrl+wheel или просто wheel.
    this.canvas.addEventListener('wheel', (e: WheelEvent) => {
      e.preventDefault();
      const [sx, sy] = this.screenPos(e);
      if (e.ctrlKey || e.metaKey) {
        const factor = Math.exp(-e.deltaY * 0.0015);
        this.viewport.zoomAt(sx, sy, factor);
      } else {
        // Простое колесо = pan по вертикали, shift+колесо = горизонталь.
        if (e.shiftKey) this.viewport.pan(-e.deltaY, 0);
        else this.viewport.pan(0, -e.deltaY);
      }
      this.scheduleRender();
    }, { passive: false });
  }

  private bindKeyboard(): void {
    window.addEventListener('keydown', (e: KeyboardEvent) => {
      if (e.key === ' ' && !this.spaceDown) {
        this.spaceDown = true;
        this.canvas.style.cursor = 'grab';
      }
    });
    window.addEventListener('keyup', (e: KeyboardEvent) => {
      if (e.key === ' ') {
        this.spaceDown = false;
        if (!this.panning) {
          this.canvas.style.cursor = this.tool === 'select' ? 'default' : 'crosshair';
        }
      }
    });
  }

  // emitDrawUpdate — общий для rect/ellipse/diamond/line/arrow.
  // shift: констрейны — у фигур «равносторонние» (1:1), у стрелки
  // «привязка угла к 15°» (как в Excalidraw).
  private emitDrawUpdate(
    id: string,
    originWX: number,
    originWY: number,
    curWX: number,
    curWY: number,
    points: Pt[],
    isNew: boolean,
    shift: boolean,
  ): void {
    if (this.tool === 'line' || this.tool === 'arrow') {
      // Стрелка — всегда два конца (прямая); line — живая полилиния.
      const pts: Pt[] = this.tool === 'arrow'
        ? [[0, 0], this.arrowEnd(originWX, originWY, curWX, curWY, shift)]
        : points;
      const xs = pts.map((p) => p[0]);
      const ys = pts.map((p) => p[1]);
      const minX = Math.min(...xs);
      const minY = Math.min(...ys);
      const x = originWX + minX;
      const y = originWY + minY;
      const w = Math.max(...xs) - minX;
      const h = Math.max(...ys) - minY;
      const rel: Pt[] = pts.map((p) => [p[0] - minX, p[1] - minY]);
      const typ: ElementType = this.tool === 'arrow' ? 'arrow' : 'line';
      const styleProps: Record<string, unknown> = {
        [Prop.Stroke]: this.strokeColor,
        [Prop.StrokeWidth]: this.strokeWidth,
        [Prop.StrokeStyle]: this.strokeStyle,
        [Prop.Roughness]: this.roughness,
        [Prop.Opacity]: this.opacity,
      };
      if (isNew) {
        this.createElement(id, typ, {
          [Prop.X]: x,
          [Prop.Y]: y,
          [Prop.W]: w,
          [Prop.H]: h,
          [Prop.Points]: rel,
          [Prop.Fill]: 'transparent',
          ...styleProps,
        });
      } else {
        this.emitPartial(id, {
          [Prop.X]: x,
          [Prop.Y]: y,
          [Prop.W]: w,
          [Prop.H]: h,
          [Prop.Points]: rel,
        });
      }
      return;
    }

    // rect / ellipse / diamond
    let endX = curWX;
    let endY = curWY;
    if (shift) {
      // Square / circle / diamond с равными сторонами.
      const dx = curWX - originWX;
      const dy = curWY - originWY;
      const m = Math.max(Math.abs(dx), Math.abs(dy));
      endX = originWX + (dx < 0 ? -m : m);
      endY = originWY + (dy < 0 ? -m : m);
    }
    const x = Math.min(originWX, endX);
    const y = Math.min(originWY, endY);
    const w = Math.abs(endX - originWX);
    const h = Math.abs(endY - originWY);
    const typ: ElementType = this.tool === 'ellipse' ? 'ellipse'
      : this.tool === 'diamond' ? 'diamond' : 'rect';
    if (isNew) {
      this.createElement(id, typ, {
        [Prop.X]: x,
        [Prop.Y]: y,
        [Prop.W]: w,
        [Prop.H]: h,
        [Prop.Stroke]: this.strokeColor,
        [Prop.StrokeWidth]: this.strokeWidth,
        [Prop.StrokeStyle]: this.strokeStyle,
        [Prop.Roughness]: this.roughness,
        [Prop.Opacity]: this.opacity,
        [Prop.Fill]: this.fillColor,
        [Prop.FillStyle]: this.fillStyle,
      });
    } else {
      this.emitPartial(id, { [Prop.X]: x, [Prop.Y]: y, [Prop.W]: w, [Prop.H]: h });
    }
  }

  // arrowEnd — относительные координаты конца стрелки; при shift угол
  // привязывается к шагу 15° — рисовать ровные стрелки можно.
  private arrowEnd(ox: number, oy: number, cx: number, cy: number, shift: boolean): Pt {
    let dx = cx - ox;
    let dy = cy - oy;
    if (shift) {
      const step = Math.PI / 12;
      const ang = Math.round(Math.atan2(dy, dx) / step) * step;
      const len = Math.hypot(dx, dy);
      dx = Math.cos(ang) * len;
      dy = Math.sin(ang) * len;
    }
    return [dx, dy];
  }

  // --- hit testing ---

  private hitTestTop(wx: number, wy: number): string | undefined {
    const list = this.opts.doc.live();
    for (let i = list.length - 1; i >= 0; i--) {
      if (this.hitTestElement(list[i], wx, wy)) return list[i].id;
    }
    return undefined;
  }

  private hitTestElement(el: Element, wx: number, wy: number): boolean {
    const ex = readNum(el, Prop.X, 0);
    const ey = readNum(el, Prop.Y, 0);
    const ew = readNum(el, Prop.W, 0);
    const eh = readNum(el, Prop.H, 0);
    const angle = readNum(el, Prop.Angle, 0);
    // Если повёрнут — переводим точку в локальную систему элемента.
    let lx = wx, ly = wy;
    if (angle !== 0) {
      const cx = ex + ew / 2, cy = ey + eh / 2;
      const c = Math.cos(-angle), s = Math.sin(-angle);
      const dx = wx - cx, dy = wy - cy;
      lx = cx + dx * c - dy * s;
      ly = cy + dx * s + dy * c;
    }
    if (el.type === 'rect' || el.type === 'text' || el.type === 'image' || el.type === 'ellipse' || el.type === 'diamond') {
      if (el.type === 'ellipse') {
        const rx = ew / 2, ry = eh / 2;
        const dx = (lx - (ex + rx)) / rx;
        const dy = (ly - (ey + ry)) / ry;
        return dx * dx + dy * dy <= 1.1;
      }
      if (el.type === 'diamond') {
        const rx = ew / 2, ry = eh / 2;
        const dx = Math.abs(lx - (ex + rx)) / rx;
        const dy = Math.abs(ly - (ey + ry)) / ry;
        return dx + dy <= 1.1;
      }
      return lx >= ex && lx <= ex + ew && ly >= ey && ly <= ey + eh;
    }
    if (el.type === 'line' || el.type === 'arrow') {
      const pts = readPoints(el);
      if (pts.length < 2) return false;
      const threshold = Math.max(8, readNum(el, Prop.StrokeWidth, 2) * 3) / this.viewport.scale;
      const segs = catmullRomToBezier(pts.map((p) => [ex + p[0], ey + p[1]] as Pt), 1);
      return hitTestBeziers(segs, wx, wy, threshold);
    }
    return false;
  }

  private hitTestRect(a: Pt, b: Pt): string[] {
    const x1 = Math.min(a[0], b[0]), x2 = Math.max(a[0], b[0]);
    const y1 = Math.min(a[1], b[1]), y2 = Math.max(a[1], b[1]);
    const out: string[] = [];
    for (const el of this.opts.doc.live()) {
      const bb = this.bboxOf(el);
      if (bb.x >= x1 && bb.x + bb.w <= x2 && bb.y >= y1 && bb.y + bb.h <= y2) {
        out.push(el.id);
      }
    }
    return out;
  }

  // Bounding box элемента в МИРОВЫХ координатах (без учёта angle).
  private bboxOf(el: Element): { x: number; y: number; w: number; h: number } {
    return {
      x: readNum(el, Prop.X, 0),
      y: readNum(el, Prop.Y, 0),
      w: readNum(el, Prop.W, 0),
      h: readNum(el, Prop.H, 0),
    };
  }

  // Hit-test resize/rotate handles. Работает в ЭКРАННЫХ координатах,
  // т.к. размер handle фиксирован в пикселях.
  private hitHandle(wx: number, wy: number): Handle | null {
    if (this.selection.length !== 1) return null;
    const el = this.opts.doc.get(this.selection[0]);
    if (!el) return null;
    const bb = this.bboxOf(el);
    const angle = readNum(el, Prop.Angle, 0);
    const cx = bb.x + bb.w / 2, cy = bb.y + bb.h / 2;
    const pts = this.handlePositions(bb, angle, cx, cy);
    const threshold = HANDLE_SIZE / this.viewport.scale;
    for (const [h, [hx, hy]] of Object.entries(pts)) {
      const dx = wx - hx, dy = wy - hy;
      if (dx * dx + dy * dy <= threshold * threshold) return h as Handle;
    }
    return null;
  }

  private handlePositions(
    bb: { x: number; y: number; w: number; h: number },
    angle: number,
    cx: number,
    cy: number,
  ): Record<Handle, Pt> {
    const raw: Record<Handle, Pt> = {
      nw: [bb.x, bb.y],
      n: [bb.x + bb.w / 2, bb.y],
      ne: [bb.x + bb.w, bb.y],
      e: [bb.x + bb.w, bb.y + bb.h / 2],
      se: [bb.x + bb.w, bb.y + bb.h],
      s: [bb.x + bb.w / 2, bb.y + bb.h],
      sw: [bb.x, bb.y + bb.h],
      w: [bb.x, bb.y + bb.h / 2],
      rot: [bb.x + bb.w / 2, bb.y - 24 / this.viewport.scale],
    };
    if (angle === 0) return raw;
    const c = Math.cos(angle), s = Math.sin(angle);
    const out = {} as Record<Handle, Pt>;
    for (const [k, [px, py]] of Object.entries(raw)) {
      const dx = px - cx, dy = py - cy;
      out[k as Handle] = [cx + dx * c - dy * s, cy + dx * s + dy * c];
    }
    return out;
  }

  private applyResize(wx: number, wy: number): void {
    if (!this.activeHandle || !this.resizeStart) return;
    const { id, x, y, w, h } = this.resizeStart;
    const el = this.opts.doc.get(id);
    if (!el) return;
    const angle = readNum(el, Prop.Angle, 0);
    const cx = x + w / 2, cy = y + h / 2;
    // Переводим точку в локальную систему (без rotation).
    let lx = wx, ly = wy;
    if (angle !== 0) {
      const c = Math.cos(-angle), s = Math.sin(-angle);
      const dx = wx - cx, dy = wy - cy;
      lx = cx + dx * c - dy * s;
      ly = cy + dx * s + dy * c;
    }
    let nx = x, ny = y, nw = w, nh = h;
    switch (this.activeHandle) {
      case 'e': nw = Math.max(MIN_SIZE, lx - x); break;
      case 'w': { const right = x + w; nx = Math.min(lx, right - MIN_SIZE); nw = right - nx; break; }
      case 's': nh = Math.max(MIN_SIZE, ly - y); break;
      case 'n': { const bottom = y + h; ny = Math.min(ly, bottom - MIN_SIZE); nh = bottom - ny; break; }
      case 'se': nw = Math.max(MIN_SIZE, lx - x); nh = Math.max(MIN_SIZE, ly - y); break;
      case 'sw': { const right = x + w; nx = Math.min(lx, right - MIN_SIZE); nw = right - nx; nh = Math.max(MIN_SIZE, ly - y); break; }
      case 'ne': nw = Math.max(MIN_SIZE, lx - x); { const bottom = y + h; ny = Math.min(ly, bottom - MIN_SIZE); nh = bottom - ny; break; }
      case 'nw': { const right = x + w, bottom = y + h; nx = Math.min(lx, right - MIN_SIZE); ny = Math.min(ly, bottom - MIN_SIZE); nw = right - nx; nh = bottom - ny; break; }
    }
    // Для line/arrow ресайз = масштабирование points.
    if (el.type === 'line' || el.type === 'arrow') {
      const pts = readPoints(el);
      const sx = w > 0 ? nw / w : 1;
      const sy = h > 0 ? nh / h : 1;
      const scaled: Pt[] = pts.map((p) => [p[0] * sx, p[1] * sy]);
      this.emitPartial(id, { [Prop.X]: nx, [Prop.Y]: ny, [Prop.W]: nw, [Prop.H]: nh, [Prop.Points]: scaled });
      return;
    }
    this.emitPartial(id, { [Prop.X]: nx, [Prop.Y]: ny, [Prop.W]: nw, [Prop.H]: nh });
  }

  // --- rendering ---

  private resize(): void {
    const dpr = window.devicePixelRatio || 1;
    const r = this.canvas.getBoundingClientRect();
    this.canvas.width = Math.max(1, Math.floor(r.width * dpr));
    this.canvas.height = Math.max(1, Math.floor(r.height * dpr));
    const ctx = this.canvas.getContext('2d');
    ctx?.setTransform(dpr, 0, 0, dpr, 0, 0);
    this.scheduleRender();
  }

  private scheduleRender(): void {
    if (this.raf !== 0) return;
    this.raf = requestAnimationFrame(() => {
      this.raf = 0;
      this.render();
    });
  }

  private startPresenceLoop(): void {
    if (this.presenceRunning) return;
    this.presenceRunning = true;
    const loop = () => {
      this.opts.presence.step(0.35);
      this.render();
      requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  }

  private render(): void {
    const ctx = this.canvas.getContext('2d');
    if (!ctx) return;
    const dpr = window.devicePixelRatio || 1;
    const w = this.canvas.width / dpr;
    const h = this.canvas.height / dpr;
    // Фон — сетка точек как в Excalidraw.
    ctx.fillStyle = '#f5f5f5';
    ctx.fillRect(0, 0, w, h);
    this.renderGrid(ctx, w, h);

    ctx.save();
    ctx.translate(this.viewport.x, this.viewport.y);
    ctx.scale(this.viewport.scale, this.viewport.scale);

    for (const el of this.opts.doc.live()) {
      // Штрих, который рисуется СЕЙЧАС, рендерим из livePts (ниже),
      // а не из doc — иначе хвост «дёргается» из-за троттлинга ops.
      if (el.id === this.drawingId && (el.type === 'line' || el.type === 'arrow')) continue;
      this.renderElement(ctx, el);
    }
    this.renderLiveStroke(ctx);
    // Hover-подсветка — под выделением, чтобы не «перекрикивать» синее.
    if (this.hoverId && !this.selection.includes(this.hoverId)) {
      const hel = this.opts.doc.get(this.hoverId);
      if (hel) this.renderHover(ctx, hel);
    }
    // Выделение — поверх.
    for (const id of this.selection) {
      const el = this.opts.doc.get(id);
      if (el) this.renderSelection(ctx, el);
    }
    ctx.restore();

    this.renderMarquee(ctx);
    this.renderSelections(ctx);
    this.renderCursors(ctx);
    // DOM-оверлеи (редактор/тултип) синхронизируем с вьюпортом каждый
    // кадр: pan/zoom могут произойти «мимо» наших обработчиков.
    if (this.editingId) this.positionEditor();
    if (this.hoverId) this.positionHoverTip();
  }

  private renderGrid(ctx: CanvasRenderingContext2D, w: number, h: number): void {
    const gap = 24 * this.viewport.scale;
    if (gap < 8) return; // при сильном зуме-аут сетка «мылит» — прячем
    ctx.save();
    ctx.fillStyle = '#d4d4d4';
    const ox = this.viewport.x % gap;
    const oy = this.viewport.y % gap;
    for (let x = ox; x < w; x += gap) {
      for (let y = oy; y < h; y += gap) {
        ctx.beginPath();
        ctx.arc(x, y, 1, 0, Math.PI * 2);
        ctx.fill();
      }
    }
    ctx.restore();
  }

  private renderElement(ctx: CanvasRenderingContext2D, el: Element): void {
    const x = readNum(el, Prop.X, 0);
    const y = readNum(el, Prop.Y, 0);
    const ew = readNum(el, Prop.W, 0);
    const eh = readNum(el, Prop.H, 0);
    const angle = readNum(el, Prop.Angle, 0);
    const stroke = readStr(el, Prop.Stroke, '#1e1e1e');
    const fill = readStr(el, Prop.Fill, 'transparent');
    const sw = readNum(el, Prop.StrokeWidth, 2);
    const ss = readStr(el, Prop.StrokeStyle, 'solid') as StrokeStyle;
    const fs = readStr(el, Prop.FillStyle, 'none') as FillStyle;
    const ro = readNum(el, Prop.Roughness, 1);
    const op = readNum(el, Prop.Opacity, 100);

    ctx.save();
    ctx.globalAlpha = Math.max(0.05, Math.min(1, op / 100));
    if (angle !== 0) {
      const cx = x + ew / 2, cy = y + eh / 2;
      ctx.translate(cx, cy);
      ctx.rotate(angle);
      ctx.translate(-cx, -cy);
    }
    ctx.strokeStyle = stroke;
    ctx.lineWidth = sw;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    if (ss === 'dashed') ctx.setLineDash([sw * 4, sw * 3]);
    else if (ss === 'dotted') ctx.setLineDash([0.1, sw * 2.5]);
    else ctx.setLineDash([]);

    const seed = hashSeed(el.id);

    switch (el.type) {
      case 'rect': this.renderRect(ctx, x, y, ew, eh, seed, ro, sw, fill, fs); break;
      case 'ellipse': this.renderEllipse(ctx, x, y, ew, eh, seed, ro, sw, fill, fs); break;
      case 'diamond': this.renderDiamond(ctx, x, y, ew, eh, seed, ro, sw, fill, fs); break;
      case 'line': this.renderLine(ctx, x, y, readPoints(el)); break;
      case 'arrow': this.renderArrow(ctx, x, y, readPoints(el), seed, ro, sw); break;
      case 'text': this.renderText(ctx, x, y, readStr(el, Prop.Text, ''), stroke); break;
      case 'image': this.renderImage(ctx, x, y, ew, eh, readStr(el, Prop.Src, '')); break;
    }
    ctx.restore();
  }

  private renderRect(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, w: number, h: number,
    seed: number, ro: number, sw: number,
    fill: string, fs: FillStyle,
  ): void {
    if (fs === 'solid' && fill !== 'transparent') {
      ctx.fillStyle = fill;
      ctx.fillRect(x, y, w, h);
    } else if ((fs === 'hachure' || fs === 'cross-hatch') && fill !== 'transparent') {
      const gap = 5;
      const strokes = hachureFillRect(x, y, w, h, gap, Math.PI / 4, seed);
      ctx.save();
      ctx.strokeStyle = fill;
      ctx.lineWidth = 1;
      for (const s of strokes) strokePts(ctx, s);
      if (fs === 'cross-hatch') {
        const strokes2 = hachureFillRect(x, y, w, h, gap, -Math.PI / 4, seed ^ 0xabcd);
        for (const s of strokes2) strokePts(ctx, s);
      }
      ctx.restore();
    }
    if (ro === 0) {
      ctx.strokeRect(x, y, w, h);
      return;
    }
    // Rough: 4 стороны, каждая двойная.
    const corners: [Pt, Pt][] = [
      [[x, y], [x + w, y]],
      [[x + w, y], [x + w, y + h]],
      [[x + w, y + h], [x, y + h]],
      [[x, y + h], [x, y]],
    ];
    for (let i = 0; i < corners.length; i++) {
      const pair = roughDoubleLine(corners[i][0], corners[i][1], (seed + i * 0x1234567) >>> 0, ro, sw);
      for (const pts of pair) strokePts(ctx, pts);
    }
  }

  private renderEllipse(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, w: number, h: number,
    seed: number, ro: number, sw: number,
    fill: string, fs: FillStyle,
  ): void {
    const cx = x + w / 2, cy = y + h / 2;
    const rx = w / 2, ry = h / 2;
    if (fs === 'solid' && fill !== 'transparent') {
      ctx.fillStyle = fill;
      ctx.beginPath();
      ctx.ellipse(cx, cy, Math.abs(rx), Math.abs(ry), 0, 0, Math.PI * 2);
      ctx.fill();
    }
    if (ro === 0) {
      ctx.beginPath();
      ctx.ellipse(cx, cy, Math.abs(rx), Math.abs(ry), 0, 0, Math.PI * 2);
      ctx.stroke();
      return;
    }
    const loops = roughEllipse(cx, cy, Math.abs(rx), Math.abs(ry), seed, ro, sw);
    for (const pts of loops) strokePts(ctx, pts);
  }

  private renderDiamond(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, w: number, h: number,
    seed: number, ro: number, sw: number,
    fill: string, fs: FillStyle,
  ): void {
    const top: Pt = [x + w / 2, y];
    const right: Pt = [x + w, y + h / 2];
    const bottom: Pt = [x + w / 2, y + h];
    const left: Pt = [x, y + h / 2];
    if (fs === 'solid' && fill !== 'transparent') {
      ctx.fillStyle = fill;
      ctx.beginPath();
      ctx.moveTo(top[0], top[1]);
      ctx.lineTo(right[0], right[1]);
      ctx.lineTo(bottom[0], bottom[1]);
      ctx.lineTo(left[0], left[1]);
      ctx.closePath();
      ctx.fill();
    }
    const edges: [Pt, Pt][] = [[top, right], [right, bottom], [bottom, left], [left, top]];
    for (let i = 0; i < edges.length; i++) {
      if (ro === 0) {
        ctx.beginPath();
        ctx.moveTo(edges[i][0][0], edges[i][0][1]);
        ctx.lineTo(edges[i][1][0], edges[i][1][1]);
        ctx.stroke();
      } else {
        const pair = roughDoubleLine(edges[i][0], edges[i][1], (seed + i * 0x1234567) >>> 0, ro, sw);
        for (const pts of pair) strokePts(ctx, pts);
      }
    }
  }

  // renderLiveStroke — штрих/стрелка в процессе рисования.
  // Рисуем из livePts (полная частота) + provisional-хвост до
  // текущего курсора. Предыдущие сегменты при этом НЕ меняются.
  private renderLiveStroke(ctx: CanvasRenderingContext2D): void {
    if (!this.drawingId) return;
    if (this.tool !== 'line' && this.tool !== 'arrow') return;
    ctx.save();
    ctx.strokeStyle = this.strokeColor;
    ctx.lineWidth = this.strokeWidth;
    ctx.lineCap = 'round';
    ctx.lineJoin = 'round';
    if (this.tool === 'line') {
      const pts = this.liveCursor && this.livePts.length > 0
        ? [...this.livePts, this.liveCursor]
        : this.livePts;
      const path = stableQuadraticPath(pts);
      if (path) ctx.stroke(path);
    } else if (this.liveStart) {
      const cur = this.liveCursor ?? this.liveStart;
      const rel = this.arrowEnd(this.liveStart[0], this.liveStart[1], cur[0], cur[1], this.liveShift);
      const end: Pt = [this.liveStart[0] + rel[0], this.liveStart[1] + rel[1]];
      const seed = hashSeed(this.drawingId);
      const a: Pt = [this.liveStart[0], this.liveStart[1]];
      if (this.roughness === 0) {
        ctx.beginPath();
        ctx.moveTo(a[0], a[1]);
        ctx.lineTo(end[0], end[1]);
        ctx.stroke();
      } else {
        const pair = roughDoubleLine(a, end, seed, this.roughness, this.strokeWidth);
        for (const p of pair) strokePts(ctx, p);
      }
      this.renderArrowHead(ctx, a, end, seed, this.roughness, this.strokeWidth);
    }
    ctx.restore();
  }

  // renderLine — inked freehand: ОДИН гладкий штрих без «призрака».
  // Тот самый баг «у линии есть тень» — это был roughCurve с
  // полупрозрачным ghost-проходом; Excalidraw рисует freedraw одной
  // линией. Точки уже EMA-сглажены при вводе, поэтому здесь только
  // midpoint-quadratic — и он детерминирован по Points (стабильно
  // между кадрами и между клиентами).
  private renderLine(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, pts: Pt[],
  ): void {
    if (pts.length < 2) return;
    const world = pts.map((p) => [x + p[0], y + p[1]] as Pt);
    const path = stableQuadraticPath(world);
    if (path) ctx.stroke(path);
  }

  private renderArrow(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, pts: Pt[],
    seed: number, ro: number, sw: number,
  ): void {
    if (pts.length < 2) return;
    const a: Pt = [x + pts[0][0], y + pts[0][1]];
    const last = pts[pts.length - 1];
    const b: Pt = [x + last[0], y + last[1]];
    // Стрелка — прямая (двойной rough-штрих, как стороны rect).
    if (ro === 0) {
      ctx.beginPath();
      ctx.moveTo(a[0], a[1]);
      ctx.lineTo(b[0], b[1]);
      ctx.stroke();
    } else {
      const pair = roughDoubleLine(a, b, seed, ro, sw);
      for (const p of pair) strokePts(ctx, p);
    }
    this.renderArrowHead(ctx, a, b, seed, ro, sw);
  }

  // renderArrowHead — наконечник: два «пера» под ±0.4 рад от конца.
  private renderArrowHead(
    ctx: CanvasRenderingContext2D,
    a: Pt, b: Pt, seed: number, ro: number, sw: number,
  ): void {
    const dx = b[0] - a[0], dy = b[1] - a[1];
    const ang = Math.atan2(dy, dx);
    const headLen = Math.max(10, sw * 5);
    const a1 = ang + Math.PI - 0.4;
    const a2 = ang + Math.PI + 0.4;
    const t1: Pt = [b[0] + Math.cos(a1) * headLen, b[1] + Math.sin(a1) * headLen];
    const t2: Pt = [b[0] + Math.cos(a2) * headLen, b[1] + Math.sin(a2) * headLen];
    if (ro === 0) {
      ctx.beginPath();
      ctx.moveTo(t1[0], t1[1]);
      ctx.lineTo(b[0], b[1]);
      ctx.lineTo(t2[0], t2[1]);
      ctx.stroke();
    } else {
      const p1 = roughDoubleLine(t1, b, seed ^ 0x1111, ro, sw);
      const p2 = roughDoubleLine(t2, b, seed ^ 0x2222, ro, sw);
      for (const p of p1) strokePts(ctx, p);
      for (const p of p2) strokePts(ctx, p);
    }
  }

  private renderText(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, text: string, color: string,
  ): void {
    ctx.font = '16px -apple-system, "Segoe UI", system-ui, sans-serif';
    ctx.textBaseline = 'top';
    ctx.fillStyle = color;
    const lines = text.split('\n');
    for (let i = 0; i < lines.length; i++) {
      ctx.fillText(lines[i], x, y + i * 20);
    }
  }

  private renderSelection(ctx: CanvasRenderingContext2D, el: Element): void {
    const bb = this.bboxOf(el);
    const angle = readNum(el, Prop.Angle, 0);
    ctx.save();
    if (angle !== 0) {
      const cx = bb.x + bb.w / 2, cy = bb.y + bb.h / 2;
      ctx.translate(cx, cy);
      ctx.rotate(angle);
      ctx.translate(-cx, -cy);
    }
    ctx.strokeStyle = '#4263eb';
    ctx.lineWidth = 1.5 / this.viewport.scale;
    ctx.setLineDash([]);
    ctx.strokeRect(bb.x - 2 / this.viewport.scale, bb.y - 2 / this.viewport.scale,
      bb.w + 4 / this.viewport.scale, bb.h + 4 / this.viewport.scale);
    // 8 resize handles + 1 rotation.
    const cx = bb.x + bb.w / 2, cy = bb.y + bb.h / 2;
    const pts = this.handlePositions(bb, 0, cx, cy);
    const hs = HANDLE_SIZE / this.viewport.scale;
    for (const [h, [hx, hy]] of Object.entries(pts)) {
      ctx.fillStyle = h === 'rot' ? '#4263eb' : '#ffffff';
      ctx.strokeStyle = '#4263eb';
      ctx.lineWidth = 1.5 / this.viewport.scale;
      ctx.beginPath();
      ctx.rect(hx - hs / 2, hy - hs / 2, hs, hs);
      ctx.fill();
      ctx.stroke();
    }
    ctx.restore();
  }

  private renderMarquee(ctx: CanvasRenderingContext2D): void {
    if (!this.marqueeStart || !this.marqueeEnd) return;
    const [s1x, s1y] = this.viewport.toScreen(this.marqueeStart[0], this.marqueeStart[1]);
    const [s2x, s2y] = this.viewport.toScreen(this.marqueeEnd[0], this.marqueeEnd[1]);
    ctx.save();
    ctx.strokeStyle = '#4263eb';
    ctx.fillStyle = 'rgba(66, 99, 235, 0.08)';
    ctx.lineWidth = 1;
    ctx.setLineDash([4, 3]);
    const x = Math.min(s1x, s2x), y = Math.min(s1y, s2y);
    const w = Math.abs(s2x - s1x), h = Math.abs(s2y - s1y);
    ctx.fillRect(x, y, w, h);
    ctx.strokeRect(x, y, w, h);
    ctx.restore();
  }

  private renderSelections(ctx: CanvasRenderingContext2D): void {
    ctx.save();
    ctx.setLineDash([6, 4]);
    ctx.lineWidth = 2;
    for (const remote of this.opts.presence.list()) {
      if (!remote.selection.length) continue;
      for (const id of remote.selection) {
        const el = this.opts.doc.get(id);
        if (!el || readBool(el, Prop.Deleted)) continue;
        const [sx, sy] = this.viewport.toScreen(
          readNum(el, Prop.X, 0),
          readNum(el, Prop.Y, 0),
        );
        const w = readNum(el, Prop.W, 0) * this.viewport.scale;
        const h = readNum(el, Prop.H, 0) * this.viewport.scale;
        ctx.strokeStyle = remote.color;
        ctx.strokeRect(sx - 4, sy - 4, w + 8, h + 8);
      }
    }
    ctx.restore();
  }

  private renderCursors(ctx: CanvasRenderingContext2D): void {
    for (const r of this.opts.presence.list()) {
      if (!r.hasCursor) continue;
      const [x, y] = this.viewport.toScreen(r.renderX, r.renderY);
      ctx.save();
      ctx.fillStyle = r.color;
      ctx.strokeStyle = '#fff';
      ctx.lineWidth = 1.5;
      ctx.beginPath();
      ctx.moveTo(x, y);
      ctx.lineTo(x + 12, y + 5);
      ctx.lineTo(x + 5, y + 6);
      ctx.lineTo(x + 4, y + 14);
      ctx.closePath();
      ctx.fill();
      ctx.stroke();
      const label = r.name + (r.tool ? ' · ' + r.tool : '');
      ctx.font = '12px system-ui, sans-serif';
      const textW = ctx.measureText(label).width;
      ctx.fillStyle = r.color;
      ctx.beginPath();
      ctx.roundRect(x + 14, y + 12, textW + 10, 18, 6);
      ctx.fill();
      ctx.fillStyle = '#fff';
      ctx.fillText(label, x + 19, y + 25);
      ctx.restore();
    }
  }

  // --- inline text editing ---

  beginTextEdit(id: string, isNew: boolean): void {
    const el = this.opts.doc.get(id);
    if (!el || el.type !== 'text') return;
    this.editingId = id;
    this.editIsNew = isNew;
    const ta = document.createElement('textarea');
    ta.className = 'text-editor';
    ta.spellcheck = false;
    ta.value = readStr(el, Prop.Text, '');
    ta.style.color = readStr(el, Prop.Stroke, '#1e1e1e');
    document.body.appendChild(ta);
    this.editorEl = ta;
    this.positionEditor();

    ta.addEventListener('keydown', (e: KeyboardEvent) => {
      // Не даём глобальным хоткеям (v/r/t/…) сработать во время печати.
      e.stopPropagation();
      if (e.key === 'Escape') {
        e.preventDefault();
        this.cancelTextEdit();
      } else if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) {
        e.preventDefault();
        ta.blur(); // коммит через blur-обработчик
      }
    });
    ta.addEventListener('input', () => this.autogrowEditor(ta));
    ta.addEventListener('blur', () => this.commitTextEdit());

    ta.focus();
    if (!isNew) ta.setSelectionRange(ta.value.length, ta.value.length);
    this.autogrowEditor(ta);
    this.scheduleRender();
  }

  // positionEditor — textarea в экранных координатах элемента. Шрифт
  // масштабируется вместе с зумом, чтобы правка выглядела 1-в-1 с
  // рендером документа.
  private positionEditor(): void {
    if (!this.editorEl || !this.editingId) return;
    const el = this.opts.doc.get(this.editingId);
    if (!el) { this.destroyEditor(); return; }
    const r = this.canvas.getBoundingClientRect();
    const [sx, sy] = this.viewport.toScreen(
      readNum(el, Prop.X, 0), readNum(el, Prop.Y, 0),
    );
    const sc = this.viewport.scale;
    const ta = this.editorEl;
    ta.style.left = (r.left + sx) + 'px';
    ta.style.top = (r.top + sy) + 'px';
    ta.style.fontSize = (16 * sc) + 'px';
    ta.style.lineHeight = (20 * sc) + 'px';
    ta.style.minWidth = (60 * sc) + 'px';
    this.autogrowEditor(ta);
  }

  private autogrowEditor(ta: HTMLTextAreaElement): void {
    ta.style.height = 'auto';
    ta.style.height = ta.scrollHeight + 'px';
  }

  private destroyEditor(): void {
    if (this.editorEl) {
      this.editorEl.remove();
      this.editorEl = null;
    }
    this.editingId = null;
    this.editIsNew = false;
  }

  private cancelTextEdit(): void {
    if (!this.editingId) return;
    if (this.editIsNew) this.deleteElement(this.editingId);
    this.destroyEditor();
    this.canvas.focus();
    this.scheduleRender();
  }

  commitTextEdit(): void {
    if (!this.editingId) return;
    const id = this.editingId;
    const ta = this.editorEl;
    this.destroyEditor();
    const text = ta ? ta.value : '';
    const el = this.opts.doc.get(id);
    if (!el) { this.scheduleRender(); return; }
    if (text.trim() === '') {
      // Пустой текст не храним (Excalidraw так же): новый — откат,
      // старый — удаление (undo вернёт через историю ops).
      if (!readBool(el, Prop.Deleted)) this.deleteElement(id);
      this.setSelection([]);
    } else {
      const size = this.measureTextSize(text);
      this.emitPartial(id, { [Prop.Text]: text, [Prop.W]: size.w, [Prop.H]: size.h });
    }
    this.scheduleRender();
  }

  // measureTextSize — тот же шрифт, что в renderText: bbox в доке
  // совпадает с картинкой, иначе выделение/hover «не попадают».
  private measureTextSize(text: string): { w: number; h: number } {
    const ctx = this.canvas.getContext('2d');
    if (!ctx) return { w: Math.max(40, text.length * 8), h: 24 };
    ctx.save();
    ctx.font = '16px -apple-system, "Segoe UI", system-ui, sans-serif';
    let maxW = 0;
    const lines = text.split('\n');
    for (const ln of lines) maxW = Math.max(maxW, ctx.measureText(ln).width);
    ctx.restore();
    return {
      w: Math.max(40, Math.ceil(maxW) + 4),
      h: Math.max(24, lines.length * 20 + 4),
    };
  }

  // --- hover attribution («кто сделал запись») ---

  private ensureHoverTip(): HTMLDivElement {
    if (!this.hoverTip) {
      const d = document.createElement('div');
      d.className = 'hover-tip';
      d.style.display = 'none';
      document.body.appendChild(d);
      this.hoverTip = d;
    }
    return this.hoverTip;
  }

  // updateHoverTip — au/an едут в CRDT-доке вместе с элементом, поэтому
  // авторство видно и для чужих (remote) фигур, и после перезагрузки.
  private updateHoverTip(): void {
    const tip = this.ensureHoverTip();
    if (!this.hoverId) { tip.style.display = 'none'; return; }
    const el = this.opts.doc.get(this.hoverId);
    if (!el || readBool(el, Prop.Deleted)) {
      this.hoverId = null;
      tip.style.display = 'none';
      return;
    }
    const name = readStr(el, Prop.AuthorName, '')
      || readStr(el, Prop.Author, '')
      || 'неизвестно';
    const kind = TYPE_LABEL_RU[el.type] ?? el.type;
    tip.textContent = `${kind} · автор: ${name}`;
    tip.style.display = 'block';
    this.positionHoverTip();
  }

  private positionHoverTip(): void {
    if (!this.hoverTip || !this.hoverId) return;
    const el = this.opts.doc.get(this.hoverId);
    if (!el) {
      this.hoverTip.style.display = 'none';
      this.hoverId = null;
      return;
    }
    const r = this.canvas.getBoundingClientRect();
    const bb = this.bboxOf(el);
    const [sx, sy] = this.viewport.toScreen(bb.x, bb.y);
    this.hoverTip.style.left = Math.max(4, r.left + sx) + 'px';
    this.hoverTip.style.top = Math.max(r.top + 4, r.top + sy - 30) + 'px';
  }

  // renderHover — лёгкая подсветка элемента под курсором: видно, К
  // КАКОЙ именно фигуре относится подсказка.
  private renderHover(ctx: CanvasRenderingContext2D, el: Element): void {
    const bb = this.bboxOf(el);
    const s = 3 / this.viewport.scale;
    ctx.save();
    ctx.strokeStyle = 'rgba(66, 99, 235, 0.45)';
    ctx.lineWidth = 1.5 / this.viewport.scale;
    ctx.setLineDash([]);
    ctx.strokeRect(bb.x - s, bb.y - s, bb.w + s * 2, bb.h + s * 2);
    ctx.restore();
  }

  // --- images + clipboard ---

  // renderImage — вставленная картинка. src хранится прямо в элементе:
  // для CRDT это обычная строковая LWW-ячейка, серверу «всё равно»,
  // что там внутри. Пока decode не готов — пунктирная заглушка, чтобы
  // первый кадр не мигал пустотой.
  private renderImage(
    ctx: CanvasRenderingContext2D,
    x: number, y: number, w: number, h: number, src: string,
  ): void {
    const img = this.loadImage(src);
    if (!img) {
      ctx.save();
      ctx.fillStyle = 'rgba(0, 0, 0, 0.05)';
      ctx.fillRect(x, y, w, h);
      ctx.strokeStyle = '#adb5bd';
      ctx.lineWidth = 1 / this.viewport.scale;
      ctx.setLineDash([6 / this.viewport.scale, 4 / this.viewport.scale]);
      ctx.strokeRect(x, y, w, h);
      ctx.restore();
      return;
    }
    ctx.drawImage(img, x, y, w, h);
  }

  private loadImage(src: string): HTMLImageElement | undefined {
    if (!src) return undefined;
    let img = this.imgCache.get(src);
    if (!img) {
      img = new Image();
      img.onload = () => this.scheduleRender();
      img.onerror = () => { this.imgCache.delete(src); };
      img.src = src;
      this.imgCache.set(src, img);
    }
    return img.complete && img.naturalWidth > 0 ? img : undefined;
  }

  // pasteOrigin — курсор мыши (в мире), если его двигали; иначе центр
  // вьюпорта. Вставка «под мышкой» — как в Excalidraw.
  private pasteOrigin(): Pt {
    if (this.cursorSeen) return [this.lastCursorX, this.lastCursorY];
    const r = this.canvas.getBoundingClientRect();
    return this.viewport.toWorld(r.width / 2, r.height / 2);
  }

  pasteText(text: string): void {
    if (!text) return;
    const [x, y] = this.pasteOrigin();
    const size = this.measureTextSize(text);
    const id = crypto.randomUUID();
    this.createElement(id, 'text', {
      [Prop.X]: x,
      [Prop.Y]: y,
      [Prop.W]: size.w,
      [Prop.H]: size.h,
      [Prop.Stroke]: this.strokeColor,
      [Prop.Fill]: 'transparent',
      [Prop.Text]: text,
    });
    this.setSelection([id]);
  }

  pasteImage(src: string, natW: number, natH: number): void {
    if (!src || natW <= 0 || natH <= 0) return;
    const [x, y] = this.pasteOrigin();
    // Ограничиваем только отображаемый размер (320 world-px по длинной
    // стороне) — разрешение остаётся исходное, resize-ручки его
    // меняют как у любой фигуры.
    const k = Math.min(1, 320 / Math.max(natW, natH));
    const w = Math.max(16, Math.round(natW * k));
    const h = Math.max(16, Math.round(natH * k));
    const id = crypto.randomUUID();
    this.loadImage(src); // начнём decode раньше первого рендера
    this.createElement(id, 'image', {
      [Prop.X]: x,
      [Prop.Y]: y,
      [Prop.W]: w,
      [Prop.H]: h,
      [Prop.Src]: src,
    });
    this.setSelection([id]);
  }
}
