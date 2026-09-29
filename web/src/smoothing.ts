// Сглаживание freehand-штрихов.
//
// Problem: пользователь тащит мышь, мы получаем ~60 сырых точек/сек.
// Если рисовать их как polyline — видны «ступеньки» (особенно на
// поворотах). Excalidraw решает это двухступенчато:
//   1. Упрощение (Ramer-Douglas-Peucker) — убираем «шумовые» точки,
//      оставляем только значимые.
//   2. Catmull-Rom → cubic bezier — из оставшихся точек строим ГЛАДКУЮ
//      кривую, которую canvas умеет рисовать через bezierCurveTo.
//
// Результат: линия выглядит рукописной, но без «ломаности».

export type Pt = [number, number];

// RDP: упрощаем полилинию, оставляя только точки, дающие отклонение
// больше epsilon. epsilon в МИРАХ ДОКУМЕНТА (не экранных пикселях).
export function rdp(points: Pt[], epsilon = 1.5): Pt[] {
  if (points.length < 3) return points.slice();
  const keep = new Uint8Array(points.length);
  keep[0] = 1;
  keep[points.length - 1] = 1;
  recurse(points, 0, points.length - 1, epsilon * epsilon, keep);
  const out: Pt[] = [];
  for (let i = 0; i < points.length; i++) if (keep[i]) out.push(points[i]);
  return out;
}

function recurse(pts: Pt[], first: number, last: number, sqEps: number, keep: Uint8Array): void {
  let maxSq = 0;
  let index = -1;
  const [x1, y1] = pts[first];
  const [x2, y2] = pts[last];
  const dx = x2 - x1;
  const dy = y2 - y1;
  const lenSq = dx * dx + dy * dy;
  for (let i = first + 1; i < last; i++) {
    const [px, py] = pts[i];
    let distSq: number;
    if (lenSq === 0) {
      const ddx = px - x1, ddy = py - y1;
      distSq = ddx * ddx + ddy * ddy;
    } else {
      // Проекция точки на отрезок (не прямую) — классический RDP.
      let t = ((px - x1) * dx + (py - y1) * dy) / lenSq;
      if (t < 0) t = 0;
      else if (t > 1) t = 1;
      const qx = x1 + t * dx, qy = y1 + t * dy;
      const ddx = px - qx, ddy = py - qy;
      distSq = ddx * ddx + ddy * ddy;
    }
    if (distSq > maxSq) {
      maxSq = distSq;
      index = i;
    }
  }
  if (maxSq > sqEps && index > 0) {
    keep[index] = 1;
    recurse(pts, first, index, sqEps, keep);
    recurse(pts, index, last, sqEps, keep);
  }
}

// Cubic bezier segment: кривая от a до b с контрольными точками c1, c2.
export interface Bez {
  a: Pt;
  c1: Pt;
  c2: Pt;
  b: Pt;
}

// Catmull-Rom → cubic bezier.
//
// Формула: для точек P[i-1], P[i], P[i+1], P[i+2] сегмент между
// P[i] и P[i+1] имеет контрольные точки:
//   c1 = P[i]   + (P[i+1] - P[i-1]) / 6 * tension
//   c2 = P[i+1] - (P[i+2] - P[i])   / 6 * tension
//
// tension = 1 → стандартный Catmull-Rom; <1 → более «тугая» кривая.
// Для краёв (i=0 и i=n-2) используем «виртуальные» точки, отражая
// соседнюю — это даёт естественное продолжение на концах.
export function catmullRomToBezier(points: Pt[], tension = 1): Bez[] {
  const n = points.length;
  if (n < 2) return [];
  if (n === 2) {
    // Деградация в прямую: контрольные точки на 1/3 и 2/3.
    const [a, b] = points;
    const c1: Pt = [a[0] + (b[0] - a[0]) / 3, a[1] + (b[1] - a[1]) / 3];
    const c2: Pt = [a[0] + (2 * (b[0] - a[0])) / 3, a[1] + (2 * (b[1] - a[1])) / 3];
    return [{ a, c1, c2, b }];
  }
  const out: Bez[] = [];
  for (let i = 0; i < n - 1; i++) {
    const p0 = points[i === 0 ? 0 : i - 1];
    const p1 = points[i];
    const p2 = points[i + 1];
    const p3 = points[i + 2 >= n ? n - 1 : i + 2];
    const c1: Pt = [
      p1[0] + ((p2[0] - p0[0]) / 6) * tension,
      p1[1] + ((p2[1] - p0[1]) / 6) * tension,
    ];
    const c2: Pt = [
      p2[0] - ((p3[0] - p1[0]) / 6) * tension,
      p2[1] - ((p3[1] - p1[1]) / 6) * tension,
    ];
    out.push({ a: p1, c1, c2, b: p2 });
  }
  return out;
}

// Полный конвейер: сырые точки → RDP → Catmull-Rom → bezier-сегменты.
// Вызывается ОДИН раз при завершении жеста (pointerup), не на каждый
// pointermove — иначе CPU сожжём.
export function smoothStroke(raw: Pt[], epsilon = 1.5): Bez[] {
  if (raw.length === 0) return [];
  const simplified = rdp(raw, epsilon);
  return catmullRomToBezier(simplified, 1);
}

// Добавить сегменты в Path2D. Используется и для «чистого» рендера,
// и для rough-оверлея.
export function appendBeziers(path: Path2D, segs: Bez[]): void {
  if (segs.length === 0) return;
  path.moveTo(segs[0].a[0], segs[0].a[1]);
  for (const s of segs) {
    path.bezierCurveTo(s.c1[0], s.c1[1], s.c2[0], s.c2[1], s.b[0], s.b[1]);
  }
}

// Простейшая оценка «расстояния до кривой» — для hit-testing.
// Возвращает true, если точка (x,y) ближе чем threshold к любому
// сегменту кривой (аппроксимация: дискретизируем bezier на 16 шагов).
export function hitTestBeziers(segs: Bez[], x: number, y: number, threshold: number): boolean {
  const th2 = threshold * threshold;
  for (const s of segs) {
    for (let i = 0; i <= 16; i++) {
      const t = i / 16;
      const [px, py] = evalBezier(s, t);
      const dx = px - x, dy = py - y;
      if (dx * dx + dy * dy <= th2) return true;
    }
  }
  return false;
}

function evalBezier(s: Bez, t: number): Pt {
  const mt = 1 - t;
  const a = mt * mt * mt;
  const b = 3 * mt * mt * t;
  const c = 3 * mt * t * t;
  const d = t * t * t;
  return [
    a * s.a[0] + b * s.c1[0] + c * s.c2[0] + d * s.b[0],
    a * s.a[1] + b * s.c1[1] + c * s.c2[1] + d * s.b[1],
  ];
}

// Bounding box для кривой — нужен для resize handles.
export function beziersBBox(segs: Bez[]): { x: number; y: number; w: number; h: number } {
  if (segs.length === 0) return { x: 0, y: 0, w: 0, h: 0 };
  let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
  for (const s of segs) {
    for (const p of [s.a, s.c1, s.c2, s.b]) {
      if (p[0] < minX) minX = p[0];
      if (p[1] < minY) minY = p[1];
      if (p[0] > maxX) maxX = p[0];
      if (p[1] > maxY) maxY = p[1];
    }
  }
  return { x: minX, y: minY, w: maxX - minX, h: maxY - minY };
}

// StableQuadraticPath — инкрементально-стабильный сглаживающий путь.
//
// КЛЮЧЕВОЕ отличие от RDP+Catmull-Rom: добавление новой точки в
// ХВОСТ НИКОГДА не меняет уже нарисованные сегменты. Это лечит
// жалобу «прошлая часть линии чуть меняется при продолжении».
//
// Алгоритм (классика «smooth freehand», используется в Excalidraw для
// live-preview и в tldraw):
//   moveTo(pts[0])
//   для i = 1..n-2: quadraticCurveTo(pts[i], midpoint(pts[i], pts[i+1]))
//   final:  lineTo(pts[n-1])
// Каждая «подтверждённая» вершина — середина между двумя raw-точками,
// а контрольная — сама raw-точка. Новый x_{n+1} влияет только на
// финальный segment; предыдущие mid'ы не пересчитываются.
export function stableQuadraticPath(pts: Pt[]): Path2D | null {
  if (pts.length < 2) return null;
  const p = new Path2D();
  p.moveTo(pts[0][0], pts[0][1]);
  if (pts.length === 2) {
    p.lineTo(pts[1][0], pts[1][1]);
    return p;
  }
  for (let i = 1; i < pts.length - 1; i++) {
    const cur = pts[i];
    const next = pts[i + 1];
    const mx = (cur[0] + next[0]) / 2;
    const my = (cur[1] + next[1]) / 2;
    p.quadraticCurveTo(cur[0], cur[1], mx, my);
  }
  // Последняя точка — доводим прямым отрезком, чтобы курсор «упирался»
  // ровно туда, где руку оторвали.
  const last = pts[pts.length - 1];
  p.lineTo(last[0], last[1]);
  return p;
}

// EMA-фильтр сырых pointer-точек. alpha — вес НОВОГО сэмла: 1 = без
// сглаживания, 0.5 = половинное. 0.45 — хороший баланс «шум vs lag»
// для мыши 60 Hz.
export function emaPoint(prev: Pt | null, cur: Pt, alpha = 0.45): Pt {
  if (!prev) return cur;
  return [prev[0] + (cur[0] - prev[0]) * alpha, prev[1] + (cur[1] - prev[1]) * alpha];
}
