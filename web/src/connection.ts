import type { Envelope } from './protocol';

export interface ConnEvents {
  onOpen: () => void;
  onFrame: (env: Envelope) => void;
  // onClose возвращает решение: false — НЕ переподключаться
  // (напр. close 1008 «неверный пароль» — реконnect обречён и только
  // долбит сервер). undefined/true — обычный авто-reconnect.
  onClose: (info: { code: number; reason: string }) => void | boolean;
  onError: (err: unknown) => void;
}

// Connection — тонкая обёртка над WebSocket с автопереподключением и
// exponential backoff. Никакой бизнес-логики: только «доставить envelope».
//
// Send-вызов вне OPEN-состояния тихо игнорируется. Очередь offline —
// ответственность вызывающего (main.ts), чтобы логика ретраев
// не смешивалась с транспортом.
export class Connection {
  private ws: WebSocket | null = null;
  private attempts = 0;
  private reconnectTimer: number | undefined;
  private closedByUs = false;

  constructor(
    private readonly buildUrl: () => string,
    private readonly events: ConnEvents,
  ) {}

  connect(): void {
    this.closedByUs = false;
    const url = this.buildUrl();
    const ws = new WebSocket(url);
    ws.onopen = () => {
      this.attempts = 0;
      this.events.onOpen();
    };
    ws.onmessage = (ev) => {
      try {
        const env = JSON.parse(ev.data as string) as Envelope;
        this.events.onFrame(env);
      } catch (err) {
        this.events.onError(err);
      }
    };
    ws.onclose = (ev) => {
      const retry = this.events.onClose({ code: ev.code, reason: ev.reason });
      if (!this.closedByUs && retry !== false) this.scheduleReconnect();
    };
    ws.onerror = (e) => this.events.onError(e);
    this.ws = ws;
  }

  send(env: Envelope): boolean {
    if (this.ws && this.ws.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify(env));
      return true;
    }
    return false;
  }

  // isOpen — true, если соеденение реально OPEN. UI использует
  // это для восстановления статуса после flash-сообщений.
  isOpen(): boolean {
    return !!this.ws && this.ws.readyState === WebSocket.OPEN;
  }

  close(): void {
    this.closedByUs = true;
    if (this.reconnectTimer !== undefined) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = undefined;
    }
    this.ws?.close(1000, 'bye');
    this.ws = null;
  }

  // Backoff: 300мс → 600 → 1200 … потолок 30с. Случайный jitter не нужен
  // на Этапе 1: reconnects редкие, thundering herd нет.
  private scheduleReconnect(): void {
    this.attempts += 1;
    const delay = Math.min(30_000, 300 * 2 ** (this.attempts - 1));
    this.reconnectTimer = window.setTimeout(() => this.connect(), delay);
  }
}
