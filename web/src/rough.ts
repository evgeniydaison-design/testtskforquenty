// Rough / sketchy rendering — фирменный стиль Excalidraw.
//
// Идея: каждая линия рисуется ДВАЖДЫ, слегка «дрожа» вокруг идеальной
// геометрии. Углы прямоугольника не сходятся идеально, эллипс чуть
// «дышит». Это даёт рукописный вид без потери читаемости.
//
// КРИТИЧНО: jitter ДОЛЖЕН быть детерминированным по element.id. Иначе
// на каждом rAF линия будет «кипеть» — и визуально, и по сети (если
// мы вдруг решим кэшировать пути). Поэтому у нас seed-функция + PRNG.

import type { Bez, Pt } from './smoothing';

// Детерминированный PRNG (mulberry32). Быстрый, 32-bit state,
// достаточно хорош для визуального шума.
function mulberry32(seed: number): () => number {
  let a = seed >>> 0;
  return function () {
    a = (a + 0x6d2b79f5) | 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

// Хэш строки в 32-битный seed. FNV-1a — короткий, без коллизий на
// практичных длинах.
export function hashSeed(s: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return h >>> 0;
}

// roughness: 0 = «чисто», 1 = «средне» (дефолт Excalidraw), 2 = «сильно».
// Возвращает амплитуду смещения в мировых координатах.
function ampFor(roughness: number, lineWidth: number): number {
  const base = [0, 1.3, 2.4][Math.max(0, Math.min(2, roughness))];
  return base + lineWidth * 0.08;
}

// Линейная интерполяция.
function lerp(a: number, b: number, t: number): number {
  return a + (b - a) * t;
}

// Нормаль к вектору (dx, dy), единичная.
function normal(dx: number, dy: number): [number, number] {
  const len = Math.hypot(dx, dy) || 1;
  return [-dy / len, dx / len];
}

// Генерирует точки «дрожащей» линии от a до b.
//
// Алгоритм:
//   1. Делим отрезок на N сегментов (N ~ длина/10, минимум 4).
//   2. На каждой внутренней точке добавляем смещение вдоль нормали,
//      величина — amp * (шум в [-1, 1]).
//   3. Концы смещаем ПЕРПЕНДИКУЛЯРНО на amp * 0.5 — это даёт эффект
//      «рука не вернулась в ту же точку».
export function roughLinePts(a: Pt, b: Pt, rand: () => number, amp: number): Pt[] {
  const dx = b[0] - a[0];
  const dy = b[1] - a[1];
  const len = Math.hypot(dx, dy);
  if (len < 1) return [a, b];
  const n = Math.max(4, Math.min(40, Math.floor(len / 10)));
  const [nx, ny] = normal(dx, dy);
  const pts: Pt[] = [];
  // Смещение начала — вдоль нормали, случайное.
  const startOff = (rand() - 0.5) * amp;
  pts.push([a[0] + nx * startOff, a[1] + ny * startOff]);
  for (let i = 1; i < n; i++) {
    const t = i / n;
    const px = lerp(a[0], b[0], t);
    const py = lerp(a[1], b[1], t);
    const off = (rand() - 0.5) * amp * 2;
    pts.push([px + nx * off, py + ny * off]);
  }
  const endOff = (rand() - 0.5) * amp;
  pts.push([b[0] + nx * endOff, b[1] + ny * endOff]);
  return pts;
}

// Двойная «дрожащая» линия — то, что рисует Excalidraw для rect/ellipse.
// Два независимых прохода с разными seed-смещениями.
export function roughDoubleLine(a: Pt, b: Pt, seed: number, roughness: number, lineWidth: number): Pt[][] {
  const amp = ampFor(roughness, lineWidth);
  const r1 = mulberry32(seed);
  const r2 = mulberry32(seed ^ 0x9e3779b9);
  return [roughLinePts(a, b, r1, amp), roughLinePts(a, b, r2, amp)];
}

// Rough rect: 4 стороны, каждая — двойная линия. Углы НЕ совпадают
// между соседними сторонами — это и есть «sketchy» эффект.
export function roughRect(
  x: number,
  y: number,
  w: number,
  h: number,
  seed: number,
  roughness: number,
  lineWidth: number,
): Pt[][] {
  const p1: Pt = [x, y];
  const p2: Pt = [x + w, y];
  const p3: Pt = [x + w, y + h];
  const p4: Pt = [x, y + h];
  const out: Pt[][] = [];
  const corners: [Pt, Pt][] = [[p1, p2], [p2, p3], [p3, p4], [p4, p1]];
  for (let i = 0; i < corners.length; i++) {
    const [a, b] = corners[i];
    const pair = roughDoubleLine(a, b, (seed + i * 0x1234567) >>> 0, roughness, lineWidth);
    out.push(...pair);
  }
  return out;
}

// Rough ellipse: параметрическая кривая, две «петли» с jitter.
// Углы не «замыкаются» идеально — это тоже эффект руки.
export function roughEllipse(
  cx: number,
  cy: number,
  rx: number,
  ry: number,
  seed: number,
  roughness: number,
  lineWidth: number,
): Pt[][] {
  const amp = ampFor(roughness, lineWidth);
  const steps = Math.max(24, Math.min(80, Math.floor((rx + ry) / 3)));
  const draw = (rand: () => number, offset: number): Pt[] => {
    const pts: Pt[] = [];
    // «Перебор» — рисуем чуть больше 2π, чтобы был виден «хвостик».
    const end = Math.PI * 2 + 0.1;
    for (let i = 0; i <= steps; i++) {
      const t = (i / steps) * end;
      const jitter = (rand() - 0.5) * amp * 2;
      const rr = 1 + jitter / Math.max(rx, ry, 1);
      pts.push([cx + Math.cos(t + offset) * rx * rr, cy + Math.sin(t + offset) * ry * rr]);
    }
    return pts;
  };
  return [draw(mulberry32(seed), 0), draw(mulberry32(seed ^ 0x85ebca6b), 0.03)];
}

// Rough freehand: исходная кривая + «призрак» с лёгким смещением.
// Это НЕ удваивает линии как rect — здесь вторая линия имитирует
// «второй проход ручки», что выглядит естественно для штриха.
export function roughCurve(
  segs: Bez[],
  seed: number,
  roughness: number,
  lineWidth: number,
): { main: Pt[]; ghost: Pt[] } {
  const amp = ampFor(roughness, lineWidth) * 0.6;
  const rand1 = mulberry32(seed);
  const rand2 = mulberry32(seed ^ 0xc2b2ae35);
  const sample = (rand: () => number): Pt[] => {
    const pts: Pt[] = [];
    if (segs.length === 0) return pts;
    pts.push(segs[0].a);
    for (const s of segs) {
      // Дискретизируем bezier на 12 шагов, добавляем jitter.
      for (let i = 1; i <= 12; i++) {
        const t = i / 12;
        const mt = 1 - t;
        const a = mt * mt * mt;
        const b = 3 * mt * mt * t;
        const c = 3 * mt * t * t;
        const d = t * t * t;
        const px = a * s.a[0] + b * s.c1[0] + c * s.c2[0] + d * s.b[0];
        const py = a * s.a[1] + b * s.c1[1] + c * s.c2[1] + d * s.b[1];
        const off = (rand() - 0.5) * amp;
        pts.push([px + off, py + off]);
      }
    }
    return pts;
  };
  return { main: sample(rand1), ghost: sample(rand2) };
}

// Hachure fill — «штриховка» как в Excalidraw.
// Возвращает набор коротких штрихов внутри прямоугольника.
// Для rect: параллельные линии под ~45°. Для ellipse: те же линии,
// обрезаемые по эллипсу.
//
// Упрощение: делаем только для rect. Для ellipse — «сплошной» fill.
export function hachureFillRect(
  x: number,
  y: number,
  w: number,
  h: number,
  gap: number,
  angle: number,
  seed: number,
): Pt[][] {
  const rand = mulberry32(seed);
  const cos = Math.cos(angle);
  const sin = Math.sin(angle);
  const cx = x + w / 2;
  const cy = y + h / 2;
  // Проекция прямоугольника на направление, перпендикулярное штрихам.
  const proj = (px: number, py: number) => -(px - cx) * sin + (py - cy) * cos;
  const p1 = proj(x, y), p2 = proj(x + w, y), p3 = proj(x + w, y + h), p4 = proj(x, y + h);
  const minP = Math.min(p1, p2, p3, p4);
  const maxP = Math.max(p1, p2, p3, p4);
  const out: Pt[][] = [];
  for (let p = minP + gap / 2; p < maxP; p += gap) {
    // Линия: все точки (px, py) с proj == p. Параметризуем вдоль (cos, sin).
    // Ищем пересечения с 4 сторонами.
    const hits = clipLineToRect(x, y, w, h, cx - sin * p, cy + cos * p, cos, sin);
    if (hits.length === 2) {
      const [a, b] = hits;
      const jitter = (rand() - 0.5) * 1.5;
      out.push([
        [a[0] + jitter, a[1] + jitter],
        [b[0] + jitter, b[1] + jitter],
      ]);
    }
  }
  return out;
}

function clipLineToRect(
  rx: number,
  ry: number,
  rw: number,
  rh: number,
  px: number,
  py: number,
  dx: number,
  dy: number,
): Pt[] {
  // Параметрический clip по Liang-Barsky.
  let t0 = -Infinity, t1 = Infinity;
  const p = [-dx, dx, -dy, dy];
  const q = [px - rx, rx + rw - px, py - ry, ry + rh - py];
  for (let i = 0; i < 4; i++) {
    if (p[i] === 0) {
      if (q[i] < 0) return [];
    } else {
      const r = q[i] / p[i];
      if (p[i] < 0) {
        if (r > t1) return [];
        if (r > t0) t0 = r;
      } else {
        if (r < t0) return [];
        if (r < t1) t1 = r;
      }
    }
  }
  if (t0 === -Infinity || t1 === Infinity) return [];
  return [
    [px + dx * t0, py + dy * t0],
    [px + dx * t1, py + dy * t1],
  ];
}

// Рисуем набор Pt[] как polyline в ctx.
export function strokePts(ctx: CanvasRenderingContext2D, pts: Pt[]): void {
  if (pts.length < 2) return;
  ctx.beginPath();
  ctx.moveTo(pts[0][0], pts[0][1]);
  for (let i = 1; i < pts.length; i++) ctx.lineTo(pts[i][0], pts[i][1]);
  ctx.stroke();
}
