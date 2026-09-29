# Board — collaborative online whiteboard (Stage 5)

Портфолио-проект: сервер на Go как источник истины, CRDT-синхронизация,
реалтайм-мультиплеер, персистентность, undo/redo. Этап 5 из 8.

## Что здесь уже работает

- WebSocket-сервер с комнатами `/room/{id}`
- Модель: `rect`, `ellipse`, `line` (freehand), `text`, `arrow` (скелет)
- **Полный CRDT**: HLC-часы, LWW на уровне **свойства** (per-property),
  fractional-index для z-order, tombstone-GC с 30-дневным retention
- Presence: курсоры + выделение + инструмент. Client-throttle 30 Гц +
  server coalescing 20 Гц + интерполяция на rAF
- **Персистентность**: JSON-lines WAL + atomic snapshot на файловой
  системе. Флаг `-data <dir>`. hex(roomID) против path traversal.
  Durability invariant: WAL-append happens BEFORE broadcast
- **Undo/Redo (Этап 5)**: per-client стек в браузере. Undo — обычный
  upsert/delete **с новым TS**, поэтому корректно мержится в LWW
  поверх параллельных правок других пользователей. Drag-на-2-секунды
  = ОДИН undo шаг (жест-коалесинг)
- **История (Этап 5)**: in-memory ring buffer последних N батчей
  (default 1000) + HTTP-эндпоинт `GET /rooms/{id}/history?from=&to=`
  для debug/audit + основа под future incremental sync
- UX: drag-to-move, click-to-select, color-on-selection, bring/send
  forward/backward (PageUp/PageDown), Delete, Esc, клавиши инструментов
  (V/R/L/T/E), **Ctrl+Z / Ctrl+Shift+Z / Ctrl+Y**
- HLC-эхо фильтруется по `origin`: серверный `Now()`-bump не затирает
  более свежие локальные правки
- Клиент: свой минимальный canvas-редактор на TypeScript + Vite
- Тесты: unit (crdt/hlc, crdt/doc, crdt/fractional, room, hub, transport,
  presence, store, history) + интеграционные (реальный WS, включая
  `TestWS_PartialOpsMerge`, `TestWSPersistence_ReopenServerSeesPriorState`,
  `TestHistoryEndpoint_ReturnsAppliedBatches`) + benchmarks. Все зелёные
  под `-race`, утечки горутин ловит `goleak`
- Офлайн — в следующих этапах

## Структура

```
cmd/server/main.go        — HTTP/WS точка входа + -data + /history wiring
internal/crdt/            — HLC, Register, Element, Op, Doc, fractional, GC
internal/presence/        — эфемерное состояние (курсоры, выделения)
internal/room/            — горутина-актор одной комнаты
                            (+ store.go: Store/Loader интерфейсы,
                             + history.go: ring buffer,
                             + HistoryCmd в dispatch)
internal/store/           — FileStore: WAL + snapshot
internal/hub/             — менеджер комнат (lazy create + recovery + Peek)
internal/transport/       — envelope + WebSocket handler + backpressure
                            + history.go: HTTP GET /rooms/{id}/history
web/                      — клиент (Vite + TS + Canvas)
  src/crdt.ts             — HLC, LocalDoc, fractional index
  src/protocol.ts         — типы wire-протокола (zerкало Go)
  src/history.ts          — UndoStack, HistoryFrame, ZERO_TS
  src/board.ts            — canvas + жесты + undo/redo + gesture coalescing
  src/main.ts             — bootstrap + keyboard shortcuts
  src/presence.ts         — интерполяция чужих курсоров
  src/connection.ts       — WS reconnect backoff
loadtest/                 — появится в Этапе 7
```

## Требования

- **Go 1.22+** ([go.dev/dl](https://go.dev/dl/))
- **Node 20+ / npm 10+**
- **Git Bash / WSL / Linux / macOS** для запуска `make` (Makefile использует
  Unix-шелл). На чистой Windows-консоли команды можно дублировать руками —
  см. «Ручной запуск» ниже
- Опционально: `golangci-lint` для `make lint`

## Быстрый старт

```bash
# 1. Установить Go-зависимости (создаст go.sum)
make tidy

# 2. Собрать фронтенд (npm ci + vite build → web/dist)
cd web && npm ci && npm run build && cd ..

# 3. Терминал A: запустить сервер с персистентностью
make run DATA=./data
# [info] persistence enabled dir=./data
# [info] server started addr=:8080 serverId=board-1 allowAllOrigins=true

# 4. Терминал B: запустить dev-режим фронтенда (Vite на :5173, proxy на :8080)
make web-dev
```

Открой в **двух разных вкладках**:

```
http://localhost:5173/?room=demo
```

Проверь готовность Этапа 5 (undo/redo):

1. Вкладка A: нарисуй 3 прямоугольника. Каждый — один undo frame.
2. Вкладка A: **Ctrl+Z** три раза — все три исчезнут.
3. Вкладка A: **Ctrl+Shift+Z** дважды — два вернутся.
4. Вкладка A: тащи один прямоугольник по холсту ~2 секунды. Отпусти.
5. **Ctrl+Z** ОДИН раз — прямоугольник вернётся в НАЧАЛЬНУЮ позицию
   (не в «предыдущий кадр перетаскивания»). Это работает благодаря
   жест-коалесингу: drag = один HistoryFrame.
6. Вкладка A: создай rect. Вкладка B: кликни по нему (select), **Del**.
7. Вкладка A: **Ctrl+Z** — НЕ восстанавливает: удаление сделал не ты.
   Undo стек на клиентах разделён.

Проверь готовность Этапа 4 (не регрессировал):

1. Порисуй. Ctrl+C сервера → запусти снова с тем же `-data ./data`.
2. Обнови вкладку — доска восстановилась.

Проверь готовность Этапа 3 (не регрессировал):

1. Вкладка A: нарисуй прямоугольник.
2. Вкладка B: нажми **V** (select), кликни по нему.
3. Вкладка A: **не отпуская кнопку**, тащи мышь — элемент движется.
4. Вкладка B, **одновременно**, меняет цвет в палитре — stroke/fill
   обновляются.
5. Отпусти. Обе вкладки видят И НОВУЮ ПОЗИЦИЮ, И НОВЫЙ ЦВЕТ.

## Ручной запуск (без make)

```powershell
# Windows PowerShell / cmd
go mod tidy

# «только память» — поведение Этапов 1–3
go run .\cmd\server -addr :8080 -static .\web\dist

# с персистентностью (Этап 4+)
go run .\cmd\server -addr :8080 -static .\web\dist -data .\data

# в другом окне
cd web
npm ci
npm run build
# или dev-сервер с proxy
npm run dev
```

На Linux/macOS:

```bash
go run ./cmd/server -addr :8080 -static ./web/dist -data ./data
```

## History HTTP endpoint

```bash
curl 'http://localhost:8080/rooms/demo/history'
# {
#   "room": "demo",
#   "current": 42,
#   "records": [
#     {"seq": 1, "origin": "alice", "ops":[{"kind":"upsert","id":"e1",...}], "at": 1759000000000},
#     {"seq": 2, "origin": "bob",   "ops":[{"kind":"upsert","id":"e1","props":{"stroke":"#ff0000"}}], "at": 1759000010000},
#     ...
#   ]
# }

curl 'http://localhost:8080/rooms/demo/history?from=10&to=20'
```

Параметры:
- `from` — нижняя граница Seq (inclusive), 0 = без нижней
- `to` — верхняя граница Seq (inclusive), 0 = без верхней
- Ответ содержит все батчи, ещё не вытесненные из ring-буфера.

Комната ищется через `hub.Peek(id)`, а НЕ `GetOrCreate`: GET-запрос не
создаёт actora. Для неизвестной комнаты — 404.

## Проверки

```bash
make test         # короткие тесты
make test-race    # то же + -race — gate в CI
make bench        # бенчмарки (hot paths: Doc.Apply, Room.ApplyOps)
make cover        # coverage.html
make lint         # golangci-lint (нужен бинарь)
make vet          # go vet
```

Важные тесты, которые стоит запустить точечно при ревью Этапа 5:

```bash
# unit: ring buffer + range query + wrap-around
go test ./internal/room/ -run 'TestHistoryRing|TestRoom_History' -v -race

# integration: HTTP GET /rooms/{id}/history
go test ./internal/transport/ -run 'TestHistoryEndpoint' -v -race

# Этап 4 не регрессировал
go test ./internal/store/ ./internal/room/ ./internal/transport/ -run 'Persistence|Snapshot|Store' -v -race

# Этап 3 не регрессировал
go test ./internal/transport/ -run 'TestWS_PartialOpsMerge' -v -race
```

### Что должно получиться

```
ok      board/internal/crdt        (cached)   coverage: ~90%
ok      board/internal/store       (cached)   coverage: ~85%
ok      board/internal/room        (cached)   coverage: ~87%
ok      board/internal/hub         (cached)   coverage: ~90%
ok      board/internal/presence    (cached)   coverage: ~85%
ok      board/internal/transport   (cached)   coverage: ~82%
```

## Протокол (v1)

Все сообщения — компактный JSON в envelope:

```json
{ "type": "hello" | "welcome" | "snapshot" | "ops" | "ack" | "error"
        | "ping" | "pong" | "bye" | "presence",
  "proto": "v1",
  "payload": { ... } }
```

| Направление | Type       | Payload                                              |
|-------------|------------|------------------------------------------------------|
| C→S         | `hello`    | `{ clientId, name?, color?, lastSeq? }`              |
| C→S         | `ops`      | `{ batchId, ops: Op[] }`                             |
| C→S         | `presence` | `{ cursor?: {x,y}, selection?: string[], tool?: string }` |
| C→S         | `ping`     | `{ t }`                                              |
| C→S         | `bye`      | `{}`                                                 |
| S→C         | `welcome`  | `{ serverId, proto }`                                |
| S→C         | `snapshot` | `{ seq, elements: Element[] }`                       |
| S→C         | `ops`      | `{ seq, origin, ops: Op[] }`                         |
| S→C         | `presence` | `{ entries: PresenceEntry[] }`                       |
| S→C         | `ack`      | `{ batchId, seq }`                                   |
| S→C         | `error`    | `{ code, message }`                                  |
| S→C         | `pong`     | `{ t }`                                              |

**Протокол Этапа 5 НЕ меняется.** Undo/redo — это обычный `ops`
сообщение с новыми TS. Сервер не знает, что это «откат»: для него
это такой же upsert/delete. Именно поэтому серверная логика остаётся
CRDT-чистым, а undo работает в многопользовательской среде.

Добавлен **HTTP GET** (не WS) эндпоинт `/rooms/{id}/history` — см.
секцию выше.

### Op (Этап 3 — частичный, per-property LWW)

```json
{
  "kind": "upsert" | "delete",
  "id":   "<uuid>",
  "ts":   { "w": <unixMs int64>, "l": <uint32>, "n": "<node>" },
  "type": "rect" | "ellipse" | "line" | "text" | "arrow",   // только при первом upsert
  "props": { "x": <number>, "stroke": "<hex>", ... }        // только изменённые ключи
}
```

- `props` **не содержит `del`** — удаление только через `kind: "delete"`.
- `ts.n` на клиенте = clientId; на серверном эхе = `"server:<roomID>"`.
  Клиент использует `origin` из `OpsBroadcastPayload`, чтобы отфильтровать
  своё же эхо.

### Element (snapshot)

```json
{
  "id":   "<uuid>",
  "type": "rect",
  "props": {
    "x":     { "v": 42.0,   "ts": { "w": 1730000000000, "l": 0, "n": "alice" } },
    "y":     { "v": 17.5,  "ts": { ... } },
    "rank":  { "v": "i",    "ts": { ... } },
    "stroke":{ "v": "#2563eb", "ts": { ... } }
  }
}
```

## Формат персистента

Каталог, указанный в `-data`, выглядит так:

```
data/
├── 64656d6f/             ← hex(roomID="demo")
│   ├── snapshot.json     ← atomic tmp+rename
│   └── wal.log           ← JSON-lines (append-only)
└── 6f74686572/
    └── wal.log           ← если снапшот ещё не писали
```

`snapshot.json` — `room.Snapshot` в JSON: `{seq, elements, savedAt}`.

`wal.log` — по одной `room.Record` на строку:
`{"seq":<uint64>,"origin":"<clientId>","ops":[<Op>...],"at":<unixMs>}`.

**History-records** (in-memory ring) имеют тот же формат `room.Record`,
что и WAL: один и тот же `Record` кладётся в `r.hist` и (если store
подключён) в `r.store.Append(...)`. Разница — WAL persists через
краш, ring buffer — только в рамках life-цикла комнаты.

## Ключевые архитектурные решения

### Этап 5

- **Undo = новая операция, а не «откат».** `board.undo()` достаёт
  frame из стека и отправляет `inverse-ops` как обычный батч с
  **новым** `clock.now()` TS. Никакой серверной «отматывающей»
  логики: всё тот же LWW-merge, всё тот же Observe+Now на сервере.
- **Per-client undo стек.** Стек живёт в браузере одного пользователя.
  «B» не может случайно «undo-нуть» действие «A». Каждый видит свои
  undo/redo — это то, что делает collaborative undo предсказуемым.
- **Жест-коалесинг.** `beginGesture/endGesture` — «скобки» вокруг
  drag/draw/multi-delete. Внутри жеста все `emitBatch` мержатся в
  один `HistoryFrame`: inverse **keep-first** (самое первое «до»
  значение), redo **merge-props** (чтобы create+drag+resize дал
  полный forward). Итог: 2-секундный drag на 60 emit-ов = один
  undo шаг.
- **buildFrame** — чистая функция «ops + before → {inverse, redo}».
  Никакой сетевой логики, легко тестировать. Инварианты:
  - `upsert` нового id → `inverse = [delete id]`
  - `upsert` существующего id → `inverse = [upsert id, props:<before-values-of-changed-keys>]`
  - `delete` существующего → `inverse = [upsert id, type:<before-type>, props:<all-before>]`
  - `delete` уже удалённого → не попадает в стек (guarded в `deleteElement`)
  - upsert нового prop (которого раньше не было) → НЕ включается в
    inverse: LWW не умеет «unset». Практически все props
    создаются вместе с элементом, так что этот corner-case редок.
- **Placeholder TS в stack-ах.** Внутри `HistoryFrame` TS =
  `{w:0,l:0,n:''}`. Реальный `clock.now()` подставляется ОДИН раз,
  прямо перед sendOps. Это защищает от «закостенелого» TS, который
  после долгого сидения в стеке уже не выигрывает LWW.
- **History ring buffer на сервере** — 1000 последних батчей per-room,
  O(1) push через кольцевой буфер. Это НЕ «persistent history»
  (WAL — то, что на disk), а горячий кэш для HTTP debug-эндпоинта
  и будущей фазы «client reconnect по lastSeq» (Этап 6).
- **HistoryCmd — read-only command, идёт через dispatch.** Так же,
  как Join. Иначе пришлось бы городить мьютекс на `r.hist` и ломать
  инвариант актora. Reply буферизован (cap=1) — handler никогда не
  блокируется на клиенте.
- **`hub.Peek(id)`** — «комната, если она уже есть». Нужен, чтобы
  GET /history НЕ порождал actora как побочный эффект. Классическая
  ловушка «read endpoint, который creates resource».

### Этап 4 (остался в силе)

- **Store на стороне потребителя, FileStore отдельно** — `room.Store`
  (hot) vs `room.Loader` (cold), реализация `*store.FileStore`.
- **WAL ДО broadcast** — durability invariant.
- **fsync батчем** (`WalFlushTick=50ms`), atomic snapshot+rename
  каждые `SnapshotEvery=200`, WAL truncate.
- **`lastSeq` идемпотентность append-а** — retry-safe.
- **Crash-tolerant tail** — `Load` break-ается на первой непарсящейся
  строке.
- **`WithLoaded` поднимает HLC** до `max(recovered.TS)`.
- **hex(roomID)** против path traversal.

### Этап 3 (остался в силе)

- **HLC вместо Lamport+client**, per-property LWW, fractional index,
  tombstone-GC с 30-дневным retention, сервер нормализует TS через
  `Observe+Now`, эхо фильтруется по `origin`.

### Этапы 1–2 (остались в силе)

- **Комната = горутина-актор.** Никаких `sync.Mutex` на документе.
- **Reader/writer goroutine на соединение** + non-blocking send в
  клиентский буфер — backpressure.
- **Отмена везде**, `goleak` — gate в `_test.go` каждого пакета.
- **Codec interface** на стороне комнаты.
- **Presence** живёт отдельным тикером внутри того же Run-цикла.

## Что будет дальше

| Этап | Фокус                                                     |
|------|-----------------------------------------------------------|
| 6    | Офлайн: IndexedDB-queue + инкрементальный sync по lastSeq + текстовый CRDT (RGA/YATA) |
| 7    | Метрики, pprof, loadtest, async WAL writer, room eviction |
| 8    | Auth, rate-limit, экспорт (PNG/JSON), Docker Compose      |

## Известные ограничения Этапа 5

- **Undo не «конфликтует» — он побеждает по LWW.** Если A сделал
  move, B сделал color, а потом A жмёт undo — восстановится
  позиция, но цвет B останется (undo A трогает только x/y).
  Корректно. Однако если B сделал move ПОСЛЕ A, undo A вернёт
  позицию к «до-А» и **перезапишет** move B. Это известное
  поведение Figma, Miro и других real-time collaborative tools:
  undo работает в рамках одного пользователя, «конфликт-
  resolution» — на LWW. Более «умный» undo (с обнаружением
  конфликтов) требует OT-подобной machinery — это НЕ цель
  данного проекта.
- **Нет persistent undo-стека.** После F5 undo-стек пуст:
  стек живёт в памяти браузера, не в LocalDoc-е и не на сервере.
  Персист стека (в localStorage или IndexedDB) — Этап 6 вместе
  с offline.
- **`upsert` нового prop без «before» не undo-ается.** Если
  пользователь изменит свойство, которого раньше у элемента не
  было (например, `angle` на элементе, созданном до Этапа 3),
  inverse не сможет его «unset» — LWW не умеет unset. На практике
  все props создаются в `createElement`, поэтому corner-case
  редок. Правильный фикс — per-key tombstone, но это ломает
  компактность Element-а; оставляем до Этапа 6.
- **History ring buffer — только в памяти.** После restart-а
  комнаты ring пустой; «история» физически лежит в WAL, но
  HTTP /history его НЕ читает. Это осознанно: cold-path
  (load from disk) и hot-path (query ring) — разные задачи.
  Чтение из WAL для /history появится в Этапе 6-7, если
  понадобится.
- **Undo/redo не «склеивают» разные типы действий.** Create +
  Drag = два undo шага. Это правильное поведение (два разных
  жеста), но может быть неочевидно. UX-паттерн — см. заметки
  о Figma.
- **Нет keyboard-shortcut для redo в macOS-стиле (Cmd+Shift+Z**
  работает, Cmd+Y — нет, потому что Cmd+Y на macOS = ٣ special
  character `«`). Мы используем Ctrl+Y и Ctrl+Shift+Z, plus
  Cmd+Shift+Z. Это стандарт.
- **Всё, что было на Этапе 4** (sync writes, no eviction, fsync
  loss window) — в силе.
- **Всё, что было на Этапе 3** (ErrRankExhausted, неизменяемый
  text, отсутствие resize/rotate/arrow, отсутствие auth) — в
  силе.
