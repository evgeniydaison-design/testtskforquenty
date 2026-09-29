// Viewport — соответствие «мир ↔ экран».
//
// Мир (world) — координаты документа, те что лежат в CRDT (x, y, w, h
// элементов). NEVER меняются при зуме/пане — иначе undo-стек и
// параллельные правки других пользователей сойдут с ума.
//
// Экран (screen) — пиксели canvas. Все pointer events приходят в
// экранных координатах; перед тем как что-то делать с документом,
// конвертируем через viewport.toWorld.
//
// Zoom «вокруг курсора»: factor применяется к scale, а offset
// подстраивается так, чтобы мировая точка под курсором НЕ сдвинулась.

export class Viewport {
  // offset — позиция мировой точки (0,0) на экране, в экранных пикселях.
  x = 0;
  y = 0;
  scale = 1;

  toWorld(sx: number, sy: number): [number, number] {
    return [(sx - this.x) / this.scale, (sy - this.y) / this.scale];
  }

  toScreen(wx: number, wy: number): [number, number] {
    return [wx * this.scale + this.x, wy * this.scale + this.y];
  }

  // zoomAt — масштабировать вокруг экранных координат (sx, sy).
  // factor > 1 — приблизить; < 1 — отдалить.
  zoomAt(sx: number, sy: number, factor: number, min = 0.1, max = 8): void {
    const newScale = Math.max(min, Math.min(max, this.scale * factor));
    if (newScale === this.scale) return;
    const [wx, wy] = this.toWorld(sx, sy);
    this.scale = newScale;
    // После изменения scale мировая точка (wx, wy) должна остаться
    // на экране в (sx, sy): sx = wx * scale + x → x = sx - wx * scale.
    this.x = sx - wx * this.scale;
    this.y = sy - wy * this.scale;
  }

  pan(dx: number, dy: number): void {
    this.x += dx;
    this.y += dy;
  }

  reset(): void {
    this.x = 0;
    this.y = 0;
    this.scale = 1;
  }

  // Fit — вписать мировые координаты (minX, minY, maxX, maxY) в
  // экранный rect (0, 0, screenW, screenH) с полями padding.
  fit(minX: number, minY: number, maxX: number, maxY: number, screenW: number, screenH: number, padding = 40): void {
    const w = Math.max(1, maxX - minX);
    const h = Math.max(1, maxY - minY);
    const sx = (screenW - 2 * padding) / w;
    const sy = (screenH - 2 * padding) / h;
    this.scale = Math.max(0.1, Math.min(8, Math.min(sx, sy)));
    this.x = padding - minX * this.scale + (screenW - 2 * padding - w * this.scale) / 2;
    this.y = padding - minY * this.scale + (screenH - 2 * padding - h * this.scale) / 2;
  }
}
