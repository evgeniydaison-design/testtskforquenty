import type { PresenceEntry, PresenceState } from './protocol';

// PresenceTracker — вся «присутственная» логика клиента.
//
// Задачи:
//   1. Локальный троттлинг: не более 30 Гц исходящих presence-кадров.
//      Движение мыши генерит 60–125 Hz, сервер бы захлебнулся.
//   2. Локальный реестр чужих presence + плавная интерполяция между
//      последними известными позициями (см. renderFrame).
//   3. Удаление «призраков»: если removed=true — сразу вычёркиваем.
//
// Никакой CRDT-логики здесь нет: presence — эфемерный, последний кадр
// всегда побеждает.

export interface RemoteState {
  clientId: string;
  name: string;
  color: string;
  tool?: string;
  selection: string[];
  // last-known «целевая» позиция. Сюда пишет сервер.
  targetX: number;
  targetY: number;
  // Текущая «отрисованная» позиция — результат интерполяции.
  renderX: number;
  renderY: number;
  lastUpdate: number;
  hasCursor: boolean;
}

const PALETTE = [
  '#2563eb', '#10b981', '#f59e0b', '#ef4444',
  '#8b5cf6', '#ec4899', '#06b6d4', '#84cc16',
];

// Стабильный цвет из clientId — hash → палитра. Делает UX предсказуемым:
// один и тот же clientId = один и тот же цвет на всех вкладках.
export function colorFor(clientId: string): string {
  let h = 0;
  for (let i = 0; i < clientId.length; i++) {
    h = (h * 31 + clientId.charCodeAt(i)) >>> 0;
  }
  return PALETTE[h % PALETTE.length];
}

export class PresenceTracker {
  private readonly remote = new Map<string, RemoteState>();
  private lastEmit = 0;
  private pending: PresenceState | null = null;

  // emit — callback, который реально отправляет кадр на сервер (main.ts
  // подключает его к Connection.send). false = offline, ждём следующего.
  constructor(private readonly emit: (s: PresenceState) => void) {}

  // Клиент вызывает при каждом pointermove. Трекер сам решает, слать или
  // нет; «пропущенный» кадр не потеряется — он станет pending и уйдёт на
  // следующем окне или на flush().
  recordCursor(x: number, y: number, tool: string, selection: string[]): void {
    const s: PresenceState = { cursor: { x, y }, tool, selection };
    this.pending = s;
    const now = performance.now();
    if (now - this.lastEmit >= 33) {
      this.flushNow();
    }
  }

  // Отправляет pending, если он есть. Вызывается из rAF-цикла, чтобы
  // «пропущенное» движение мыши всё-таки доходило до сервера.
  flush(): void {
    if (!this.pending) return;
    this.flushNow();
  }

  private flushNow(): void {
    if (!this.pending) return;
    this.emit(this.pending);
    this.pending = null;
    this.lastEmit = performance.now();
  }

  // applyRemoteEntries — вызывается из main.ts на каждый presence-frame.
  // Self-entries игнорируются: сервер их рассылает «для простоты», но
  // показывать свой курсор самим себе — смысл нулевой.
  applyRemoteEntries(entries: PresenceEntry[], selfId: string): void {
    for (const e of entries) {
      if (e.clientId === selfId) continue;
      if (e.removed) {
        this.remote.delete(e.clientId);
        continue;
      }
      const cur = this.remote.get(e.clientId);
      const x = e.cursor?.x ?? cur?.targetX ?? 0;
      const y = e.cursor?.y ?? cur?.targetY ?? 0;
      const now = performance.now();
      if (!cur) {
        // Первый кадр: render == target, иначе курсор «долетит» из (0,0).
        this.remote.set(e.clientId, {
          clientId: e.clientId,
          name: e.name,
          color: e.color,
          tool: e.tool,
          selection: e.selection ?? [],
          targetX: x,
          targetY: y,
          renderX: x,
          renderY: y,
          lastUpdate: now,
          hasCursor: !!e.cursor,
        });
      } else {
        cur.name = e.name;
        cur.color = e.color;
        cur.tool = e.tool;
        cur.selection = e.selection ?? [];
        cur.targetX = x;
        cur.targetY = y;
        cur.hasCursor = !!e.cursor;
        cur.lastUpdate = now;
      }
    }
  }

  list(): RemoteState[] {
    return Array.from(this.remote.values());
  }

  // step — сдвиг «отрисованной» позиции к целевой. rAF-цикл в board.ts
  // вызывает его каждый кадр. Коэффициент 0.35: быстро enough, чтобы
  // не догонять с «подтормаживанием», медленно enough, чтобы сгладить
  // 20 Гц серверного потока в плавные 60 Гц.
  step(alpha = 0.35): void {
    const now = performance.now();
    for (const s of this.remote.values()) {
      // Если курсор не обновлялся >200мс, почти «замораживаем»: иначе
      // при «зависшем» TCP он будет бесконечно скользить к последней
      // цели, что выглядит как баг.
      const idle = now - s.lastUpdate > 200;
      if (idle) continue;
      s.renderX += (s.targetX - s.renderX) * alpha;
      s.renderY += (s.targetY - s.renderY) * alpha;
    }
  }

  clear(): void {
    this.remote.clear();
  }
}
