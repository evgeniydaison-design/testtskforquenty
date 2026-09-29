// main.ts — «композиция корня»: DOM, сессия (модалка входа), connection, board.
//
// Сессия бывает трёх видов:
//   • none  — стартовое состояние, открыта модалка;
//   • solo  — работаем локально (doc в памяти, websocket не нужен);
//   • room  — подключение к комнате (опционально — с паролем).
//
// Решение «подключаться или нет» теперь ДОБРОВОЛЬНОЕ: никакого
// auto-join на загрузке страницы. Раньше это рождало две жалобы:
// «иногда не подключается» (два таба с одним clientId из localStorage —
// сервер отбивает дубликат, а мы бесконечно реконнектились) и
// «панель не видна» (закэшированный index.html → 404 на старые hashed
// ассеты; лечится Cache-Control на сервере + reload отсюда).

// Импорт CSS в точке входа: Vite выносит его в отдельный asset и
// вставляет <link> в dist/index.html. Без этой строки страница рендерится
// «голой» (без стилей) — это и был баг.
import './styles.css';
import { Connection } from './connection';
import { Board, type Tool } from './board';
import { LocalDoc } from './crdt';
import { colorFor, PresenceTracker } from './presence';
import {
  PROTO,
  type AckPayload,
  type Envelope,
  type ErrorPayload,
  type FillStyle,
  type HelloPayload,
  type OpsBroadcastPayload,
  type OpsPayload,
  type PresenceBroadcastPayload,
  type PresenceState,
  type SnapshotPayload,
  type StrokeStyle,
  type WelcomePayload,
} from './protocol';

// --- bootstrap helpers ---

// clientId —_per-tab_ (sessionStorage), а не localStorage: два таба
// одного браузера — это ДВА разных участника. С общим ID второй таб
// получал ErrDuplicateClient и «не подключался».
// Override через URL «?user=alice» — для ручной отладки.
function getClientId(): string {
  const p = new URLSearchParams(location.search);
  const forced = p.get('user');
  if (forced) return forced;
  const KEY = 'board.clientId';
  let id = sessionStorage.getItem(KEY);
  if (!id) {
    id = crypto.randomUUID();
    sessionStorage.setItem(KEY, id);
  }
  return id;
}

function getName(): string {
  const KEY = 'board.name';
  let n = localStorage.getItem(KEY);
  if (!n) {
    n = 'user-' + Math.floor(1000 + Math.random() * 9000);
    localStorage.setItem(KEY, n);
  }
  return n;
}

// SavedPrefs — то, чем предзаполняем модалку. Пароль храним только
// в этой вкладке (sessionStorage): чужой человек за тем же браузером
// не должен случайно «входить» в чужую комнату за него.
interface SavedPrefs { room: string; hasPass: boolean }
const PREFS_KEY = 'board.prefs';

function loadPrefs(): SavedPrefs {
  try {
    const raw = sessionStorage.getItem(PREFS_KEY);
    if (raw) return JSON.parse(raw) as SavedPrefs;
  } catch { /* ignore */ }
  const p = new URLSearchParams(location.search);
  return { room: p.get('room') || 'demo', hasPass: false };
}

function savePrefs(room: string, hasPass: boolean): void {
  try { sessionStorage.setItem(PREFS_KEY, JSON.stringify({ room, hasPass })); } catch { /* ignore */ }
}

function randomRoom(): string {
  return 'room-' + Math.random().toString(36).slice(2, 7);
}

// --- state ---

const clientID = getClientId();
const clientName = getName();
const clientColor = colorFor(clientID);
const doc = new LocalDoc();
const outbox: OpsPayload[] = [];
let batchSeq = 0;

type Mode = 'none' | 'solo' | 'room';
let mode: Mode = 'none';
let currentRoom = '';
let currentPassword = '';
let conn: Connection | null = null;
// hadSession — была ли на этой странице уже запущена сессия. Смена
// комнаты/режима ПОСЛЕ этого — только через reload: doc и undo-стек
// нельзя «переехать» между комнатами целиком и корректно.
let hadSession = false;

// --- DOM ---

const canvas = document.getElementById('canvas') as HTMLCanvasElement;
const statusEl = document.getElementById('status') as HTMLElement;
const strokeInput = document.getElementById('stroke') as HTMLInputElement;
const fillInput = document.getElementById('fill') as HTMLInputElement;
const usersEl = document.getElementById('users') as HTMLElement;
const zoomLabel = document.getElementById('zoom-label') as HTMLElement | null;

const backdrop = document.getElementById('modal-backdrop') as HTMLElement;
const mName = document.getElementById('m-name') as HTMLInputElement;
const mRoom = document.getElementById('m-room') as HTMLInputElement;
const mPass = document.getElementById('m-pass') as HTMLInputElement;
const mError = document.getElementById('m-error') as HTMLElement;
const mJoin = document.getElementById('m-join') as HTMLButtonElement;
const mSolo = document.getElementById('m-solo') as HTMLButtonElement;
const roomChip = document.getElementById('room-chip') as HTMLButtonElement;
const roomChipLabel = document.getElementById('room-chip-label') as HTMLElement;
const leaveBtn = document.getElementById('leave') as HTMLButtonElement;

// --- connection + board + presence ---

const presence = new PresenceTracker((s: PresenceState) => {
  // Соло: пересылать некуда — курсоры других участников просто не живут.
  if (mode !== 'room' || !conn) return;
  conn.send({ type: 'presence', proto: PROTO, payload: s });
});

const board = new Board(canvas, {
  clientID,
  clientName,
  doc,
  presence,
  sendOps: (ops) => {
    const payload: OpsPayload = { batchId: `${clientID}#${++batchSeq}`, ops };
    if (mode !== 'room' || !conn) {
      // Соло/модалка: doc уже применён локально (Board делает это сам),
      // сеть не трогаем. Никакого outbox «на потом» — при заходе в
      // комнату мы перезагружаемся, локальные рисунки не переезжают.
      refreshHistoryButtons();
      return;
    }
    if (!conn.send({ type: 'ops', proto: PROTO, payload })) outbox.push(payload);
    refreshHistoryButtons();
  },
});

// --- session / modal ---

function setStatus(text: string): void {
  statusEl.textContent = text;
}

let statusTimer: number | null = null;
function flashStatus(msg: string): void {
  setStatus(msg);
  if (statusTimer !== null) window.clearTimeout(statusTimer);
  statusTimer = window.setTimeout(() => {
    if (mode === 'solo') setStatus('solo (офлайн)');
    else if (conn && conn.isOpen()) setStatus('online');
    else setStatus('offline (reconnect…)');
    statusTimer = null;
  }, 1500);
}

function updateChip(): void {
  if (mode === 'room') {
    roomChipLabel.textContent = currentRoom + (currentPassword ? ' 🔒' : '');
  } else if (mode === 'solo') {
    roomChipLabel.textContent = 'Соло';
  } else {
    roomChipLabel.textContent = '—';
  }
}

function openModal(message?: string): void {
  const prefs = loadPrefs();
  mName.value = clientName;
  mRoom.value = prefs.room;
  mPass.value = '';
  if (message) {
    mError.textContent = message;
    mError.classList.remove('hidden');
  } else {
    mError.classList.add('hidden');
  }
  backdrop.classList.remove('hidden');
  mName.focus();
}

function closeModal(): void {
  backdrop.classList.add('hidden');
}

// startRoom — создаёт Connection и подключается. Вызывается либо сразу
// после загрузки (первый join), либо после reload при смене комнаты.
function startRoom(room: string, password: string): void {
  currentRoom = room;
  currentPassword = password;
  mode = 'room';
  hadSession = true;
  savePrefs(room, password !== '');
  syncUrl(room);
  updateChip();
  setStatus('подключение…');

  conn = new Connection(
    () => {
      const p = location.protocol === 'https:' ? 'wss:' : 'ws:';
      return `${p}//${location.host}/room/${encodeURIComponent(currentRoom)}`;
    },
    {
      onOpen: () => {
        setStatus('online');
        const hello: HelloPayload = {
          clientId: clientID,
          name: clientName,
          color: clientColor,
          password: currentPassword || undefined,
        };
        conn?.send({ type: 'hello', proto: PROTO, payload: hello });
        // Досылаем всё, что накопилось за время reconnect-паузы.
        while (outbox.length && conn) {
          const p = outbox.shift()!;
          if (!conn.send({ type: 'ops', proto: PROTO, payload: p })) {
            outbox.unshift(p);
            break;
          }
        }
      },
      onFrame: handleFrame,
      onClose: ({ code }) => {
        // 1008 PolicyViolation — сервер сказал «нет» осознанно
        // (неверный пароль / дубликат clientId). Реконнект здесь —
        // бесконечный цикл; просим пере ввода через модалку.
        setStatus('offline');
        if (code === 1008) {
          mode = 'none';
          openModal('Сервер отклонил подключение. Проверьте пароль или нажмите «Соло».');
          return false;
        }
        setStatus('offline (reconnect…)');
        return true;
      },
      onError: (e) => console.error('[ws]', e),
    },
  );
  conn.connect();
}

function startSolo(): void {
  currentRoom = '';
  currentPassword = '';
  mode = 'solo';
  hadSession = true;
  savePrefs('', false);
  if (conn) { conn.close(); conn = null; }
  outbox.length = 0;
  presence.clear();
  renderUsers();
  updateChip();
  setStatus('solo (офлайн)');
  closeModal();
}

// switchSession — смена режима/комнаты ПОСЛЕ первой сессии: только
// через полную перезагрузку страницы (чистые doc/undo/board).
// Возвращает true, если reload уже запущен (вызывающему надо выйти).
function switchSession(url: string): boolean {
  if (!hadSession) return false;
  location.assign(url);
  return true;
}

function syncUrl(room: string): void {
  const url = new URL(location.href);
  url.searchParams.set('room', room);
  history.replaceState({}, '', url);
}

function handleJoinFromModal(): void {
  const room = mRoom.value.trim() || randomRoom();
  const pass = mPass.value;
  // Имя — в localStorage (переживает reload между вкладками).
  const nm = mName.value.trim();
  if (nm) localStorage.setItem('board.name', nm);

  // Отмена модалки без смены сессии (кликнули chip и Enter) — просто
  // закрываем, перезагрузка не нужна.
  if (mode === 'room' && room === currentRoom && pass === currentPassword) {
    closeModal();
    return;
  }

  const target = new URL(location.href);
  target.searchParams.set('room', room);
  if (switchSession(target.pathname + target.search)) return; // reload идёт

  closeModal();
  startRoom(room, pass);
}

function handleSoloFromModal(): void {
  const nm = mName.value.trim();
  if (nm) localStorage.setItem('board.name', nm);
  if (mode === 'room') {
    // Уже были в комнате → выход только через reload (иначе doc
    // «помнит» чужую комнату и шлёт в неё правки).
    const target = new URL(location.href);
    target.searchParams.delete('room');
    if (switchSession(target.pathname + target.search)) return;
  }
  startSolo();
}

mJoin.addEventListener('click', handleJoinFromModal);
mSolo.addEventListener('click', handleSoloFromModal);
backdrop.addEventListener('keydown', (e: KeyboardEvent) => {
  if (e.key === 'Enter') { e.preventDefault(); handleJoinFromModal(); }
  if (e.key === 'Escape' && mode !== 'none') closeModal(); // вернёмся в текущую сессию
});
roomChip.addEventListener('click', () => openModal());
leaveBtn.addEventListener('click', () => {
  if (mode === 'room') handleSoloFromModal();
  else openModal();
});

function handleFrame(env: Envelope): void {
  switch (env.type) {
    case 'welcome': {
      const p = env.payload as WelcomePayload;
      console.info('[welcome]', p);
      break;
    }
    case 'snapshot': {
      const p = env.payload as SnapshotPayload;
      board.loadSnapshot(p.elements, p.seq);
      break;
    }
    case 'ops': {
      const p = env.payload as OpsBroadcastPayload;
      board.applyRemote(p.ops, p.seq, p.origin);
      break;
    }
    case 'ack': {
      const p = env.payload as AckPayload;
      for (let i = 0; i < outbox.length; i++) {
        if (outbox[i].batchId === p.batchId) {
          outbox.splice(i, 1);
          break;
        }
      }
      board.onAck(p.batchId);
      break;
    }
    case 'presence': {
      const p = env.payload as PresenceBroadcastPayload;
      presence.applyRemoteEntries(p.entries, clientID);
      renderUsers();
      break;
    }
    case 'error': {
      const p = env.payload as ErrorPayload;
      console.error('[server error]', p.code, p.message);
      if (p.code === 'room_password') {
        flashStatus('неверный пароль комнаты');
      }
      break;
    }
    case 'pong':
      break;
    default:
      console.warn('[unknown frame]', env.type);
  }
}

function renderUsers(): void {
  const list = presence.list();
  if (list.length === 0) {
    usersEl.textContent = '';
    return;
  }
  usersEl.innerHTML = '';
  for (const u of list) {
    const chip = document.createElement('span');
    chip.className = 'user-chip';
    chip.style.background = u.color;
    chip.textContent = u.name;
    usersEl.appendChild(chip);
  }
}

// --- toolbar wiring ---

// Undo/Redo.
const undoBtn = document.getElementById('undo') as HTMLButtonElement | null;
const redoBtn = document.getElementById('redo') as HTMLButtonElement | null;
function refreshHistoryButtons(): void {
  if (undoBtn) undoBtn.disabled = !board.canUndo();
  if (redoBtn) redoBtn.disabled = !board.canRedo();
}
if (undoBtn) {
  undoBtn.addEventListener('click', () => {
    if (!board.undo()) flashStatus('нечего отменять');
    refreshHistoryButtons();
  });
}
if (redoBtn) {
  redoBtn.addEventListener('click', () => {
    if (!board.redo()) flashStatus('нечего повторить');
    refreshHistoryButtons();
  });
}

// Tools.
const toolButtons = document.querySelectorAll<HTMLButtonElement>('button.tool');
function activateTool(name: Tool): void {
  toolButtons.forEach((b) => b.classList.toggle('active', b.dataset.tool === name));
  board.setTool(name);
}
toolButtons.forEach((btn) => {
  btn.addEventListener('click', () => activateTool(btn.dataset.tool as Tool));
});

// Stroke palette.
const swatches = document.querySelectorAll<HTMLButtonElement>('#stroke-palette .swatch[data-color]');
swatches.forEach((sw) => {
  sw.addEventListener('click', () => {
    const c = sw.dataset.color!;
    board.setStrokeColor(c);
    strokeInput.value = c;
    swatches.forEach((x) => x.classList.toggle('active', x === sw));
    if (board.selection.length > 0) {
      board.setColorForSelection(c, fillInput.value);
      refreshHistoryButtons();
    }
  });
});
strokeInput.addEventListener('input', () => {
  board.setStrokeColor(strokeInput.value);
  swatches.forEach((x) => x.classList.remove('active'));
  if (board.selection.length > 0) {
    board.setColorForSelection(strokeInput.value, fillInput.value);
    refreshHistoryButtons();
  }
});
fillInput.addEventListener('input', () => {
  board.setFillColor(fillInput.value);
  if (board.selection.length > 0) {
    board.setColorForSelection(strokeInput.value, fillInput.value);
    refreshHistoryButtons();
  }
});

// Stroke style (solid / dashed / dotted).
const ssBtns = document.querySelectorAll<HTMLButtonElement>('#ss-group .ss');
ssBtns.forEach((b) => {
  b.addEventListener('click', () => {
    const s = b.dataset.ss as StrokeStyle;
    board.setStrokeStyle(s);
    ssBtns.forEach((x) => x.classList.toggle('active', x === b));
    if (board.selection.length > 0) {
      board.setStrokeStyleForSelection(s);
      refreshHistoryButtons();
    }
  });
});

// Stroke width.
const widthBtns = document.querySelectorAll<HTMLButtonElement>('#width-group .width');
widthBtns.forEach((b) => {
  b.addEventListener('click', () => {
    const w = parseInt(b.dataset.w!, 10);
    board.setStrokeWidth(w);
    widthBtns.forEach((x) => x.classList.toggle('active', x === b));
    if (board.selection.length > 0) {
      board.setStrokeWidthForSelection(w);
      refreshHistoryButtons();
    }
  });
});

// Roughness.
const roughBtns = document.querySelectorAll<HTMLButtonElement>('#rough-group .rough');
roughBtns.forEach((b) => {
  b.addEventListener('click', () => {
    const r = parseInt(b.dataset.ro!, 10);
    board.setRoughness(r);
    roughBtns.forEach((x) => x.classList.toggle('active', x === b));
    if (board.selection.length > 0) {
      board.setRoughnessForSelection(r);
      refreshHistoryButtons();
    }
  });
});

// Fill style.
const fillBtns = document.querySelectorAll<HTMLButtonElement>('.fill-toggle .fill');
fillBtns.forEach((b) => {
  b.addEventListener('click', () => {
    const s = b.dataset.fs as FillStyle;
    board.setFillStyle(s);
    fillBtns.forEach((x) => x.classList.toggle('active', x === b));
    if (board.selection.length > 0) {
      board.setFillStyleForSelection(s);
      refreshHistoryButtons();
    }
  });
});

// Zoom controls.
function updateZoomLabel(): void {
  if (zoomLabel) zoomLabel.textContent = Math.round(board.viewport.scale * 100) + '%';
}
document.getElementById('zoom-in')?.addEventListener('click', () => {
  const r = canvas.getBoundingClientRect();
  board.viewport.zoomAt(r.width / 2, r.height / 2, 1.2);
  updateZoomLabel();
});
document.getElementById('zoom-out')?.addEventListener('click', () => {
  const r = canvas.getBoundingClientRect();
  board.viewport.zoomAt(r.width / 2, r.height / 2, 1 / 1.2);
  updateZoomLabel();
});
document.getElementById('zoom-reset')?.addEventListener('click', () => {
  board.viewport.reset();
  updateZoomLabel();
});

// --- keyboard shortcuts ---

window.addEventListener('keydown', (e) => {
  // В модалке — не перехватываем хоткеи (пользователь печатает имя/пароль).
  if (!backdrop.classList.contains('hidden')) return;
  const t = e.target as HTMLElement | null;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
    return;
  }

  const mod = e.ctrlKey || e.metaKey;

  // Хоткеи определяем по e.code — ФИЗИЧЕСКОЙ клавише, независимо от
  // раскладки. На русской раскладке Ctrl+Z приходит как e.key='я', и
  // старая проверка e.key === 'z' молча ломала отмену (и другие буквы).
  switch (e.code) {
    case 'KeyV': if (!mod) activateTool('select'); return;
    case 'KeyR': if (!mod) activateTool('rect'); return;
    case 'KeyO': if (!mod) activateTool('ellipse'); return;
    case 'KeyL': if (!mod) activateTool('line'); return;
    case 'KeyT': if (!mod) activateTool('text'); return;
    case 'KeyE': if (!mod) activateTool('erase'); return;
    case 'KeyD':
      // Ctrl/Cmd+D = duplicate, просто D = diamond.
      if (mod) {
        e.preventDefault();
        board.duplicateSelection();
        refreshHistoryButtons();
      } else {
        activateTool('diamond');
      }
      return;
    case 'KeyA':
      // Ctrl/Cmd+A — выделить всё; без модификатора — инструмент arrow.
      if (mod) {
        e.preventDefault();
        board.selectAll();
      } else {
        activateTool('arrow');
      }
      return;
    case 'KeyZ':
      // Ctrl/Cmd+Z — undo; Ctrl/Cmd+Shift+Z — redo. Работает на любой
      // раскладке, т.к. смотрим e.code, а не e.key.
      if (mod) {
        e.preventDefault();
        const ok = e.shiftKey ? board.redo() : board.undo();
        if (!ok) flashStatus(e.shiftKey ? 'нечего повторить' : 'нечего отменять');
        refreshHistoryButtons();
      }
      return;
    case 'KeyY':
      // Ctrl/Cmd+Y — redo (конвенция Windows).
      if (mod) {
        e.preventDefault();
        if (!board.redo()) flashStatus('нечего повторить');
        refreshHistoryButtons();
      }
      return;
    case 'Equal': case 'NumpadAdd': // Ctrl/Cmd «+»
      if (mod) {
        e.preventDefault();
        const r = canvas.getBoundingClientRect();
        board.viewport.zoomAt(r.width / 2, r.height / 2, 1.2);
        updateZoomLabel();
      }
      return;
    case 'Minus': case 'NumpadSubtract': // Ctrl/Cmd «-»
      if (mod) {
        e.preventDefault();
        const r = canvas.getBoundingClientRect();
        board.viewport.zoomAt(r.width / 2, r.height / 2, 1 / 1.2);
        updateZoomLabel();
      }
      return;
    case 'Digit0': case 'Numpad0': // Ctrl/Cmd «0»
      if (mod) {
        e.preventDefault();
        board.viewport.reset();
        updateZoomLabel();
      }
      return;
    case 'Delete':
    case 'Backspace':
      if (board.selection.length > 0) {
        e.preventDefault();
        board.deleteSelection();
        refreshHistoryButtons();
      }
      return;
    case 'Escape':
      board.clearSelection();
      return;
    case 'PageUp':
      e.preventDefault();
      board.bringForward();
      refreshHistoryButtons();
      return;
    case 'PageDown':
      e.preventDefault();
      board.sendBackward();
      refreshHistoryButtons();
      return;
  }
});

// --- clipboard paste (Ctrl+V) ---

// handlePastedImage — вставляем картинку, но «тяжёлые» dataURL жрёт
// лимит WS-сообщения (8 MiB на сервере): пер-кодируем через canvas —
// longest side до 1600 px + webp q0.9 (альфа сохраняется). Лёгкие
// картинки не трогаем — без потери качества.
function handlePastedImage(src: string, img: HTMLImageElement): void {
  let outSrc = src;
  let w = img.naturalWidth;
  let h = img.naturalHeight;
  if (src.length > 4 << 20) {
    const k = Math.min(1, 1600 / Math.max(w, h, 1));
    const c = document.createElement('canvas');
    c.width = Math.max(1, Math.round(w * k));
    c.height = Math.max(1, Math.round(h * k));
    const g = c.getContext('2d');
    if (g) {
      g.drawImage(img, 0, 0, c.width, c.height);
      const webp = c.toDataURL('image/webp', 0.9);
      if (webp.startsWith('data:image/webp')) {
        outSrc = webp;
        w = c.width;
        h = c.height;
      }
    }
  }
  if (outSrc.length > 7 << 20) {
    flashStatus('картинка слишком большая для вставки');
    return;
  }
  board.pasteImage(outSrc, w, h);
  refreshHistoryButtons();
  flashStatus('вставлена картинка');
}

window.addEventListener('paste', (e: ClipboardEvent) => {
  // В модалке или в текстовых полях (включая inline-редактор текста
  // на доске) — не перехватываем: там Ctrl+V работает «как обычно».
  if (!backdrop.classList.contains('hidden')) return;
  const t = e.target as HTMLElement | null;
  if (t && (t.tagName === 'INPUT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) {
    return;
  }
  const dt = e.clipboardData;
  if (!dt) return;
  // Картинка приоритетнее текста: в буфере скриншота обычно только она.
  for (const item of Array.from(dt.items)) {
    if (!item.type.startsWith('image/')) continue;
    const file = item.getAsFile();
    if (!file) continue;
    e.preventDefault();
    const reader = new FileReader();
    reader.onload = () => {
      const src = String(reader.result ?? '');
      if (!src.startsWith('data:image/')) {
        flashStatus('не удалось прочитать картинку');
        return;
      }
      const img = new Image();
      img.onload = () => handlePastedImage(src, img);
      img.onerror = () => flashStatus('не удалось прочитать картинку');
      img.src = src;
    };
    reader.readAsDataURL(file);
    return;
  }
  const text = dt.getData('text/plain');
  if (text) {
    e.preventDefault();
    board.pasteText(text);
    refreshHistoryButtons();
    flashStatus('вставлен текст');
  }
});

// --- go ---

// Никакого авто-подключения. Модалка решает всё за пользователя.
updateChip();
updateZoomLabel();
openModal();

declare global {
  interface Window {
    board: { doc: LocalDoc; board: Board; presence: PresenceTracker };
  }
}
window.board = { doc, board, presence };
