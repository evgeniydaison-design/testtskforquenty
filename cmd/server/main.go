// Command server — WebSocket-сервер коллаборативной доски.
//
// Структура запуска:
//   1. разбираем флаги и строим logger;
//   2. при указанном -data подключаем FileStore (WAL + снапшоты);
//   3. rootCtx управляется SIGINT/SIGTERM — на нём живут hub и комнаты;
//   4. chi-маршрут /room/{id} отправляет запрос в transport.Handler;
//   5. http.Server.Shutdown до hook-а в rootCtx.Done() даёт активным
//      запросам завершиться, потом hub.Shutdown «выключает» комнаты,
//      и в самом конце store.CloseAll() досинхронизирует WAL.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"board/internal/hub"
	"board/internal/room"
	"board/internal/store"
	"board/internal/transport"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		addr       = flag.String("addr", ":8080", "адрес прослушивания (host:port)")
		staticDir  = flag.String("static", "./web/dist", "каталог фронтенда; пусто — не раздавать")
		serverID   = flag.String("server-id", "board-1", "метка процесса, уходит в welcome")
		allowAll   = flag.Bool("allow-all-origins", true, "отключить Origin-проверку (только dev!)")
		shutdownTO = flag.Duration("shutdown-timeout", 5*time.Second, "grace на shutdown")
		logLevel   = flag.String("log-level", "info", "debug|info|warn|error")
		dataDir    = flag.String("data", "", "каталог персистентности (WAL+snapshot); пусто = только память")
	)
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout,
		&slog.HandlerOptions{Level: parseLevel(*logLevel)}))
	slog.SetDefault(logger)

	rootCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	codec := transport.NewJSONCodec()

	// Store: при -data пусто — nil, hub стартует «в памяти» (как в
	// Этапах 1–3). При непустом — FileStore на disk; hub читает
	// состояние комнат лениво при первом join.
	var (
		hubOpts []hub.Option
		fs      *store.FileStore
	)
	if *dataDir != "" {
		var err error
		fs, err = store.New(*dataDir)
		if err != nil {
			return fmt.Errorf("main: store init: %w", err)
		}
		hubOpts = append(hubOpts, hub.WithStore(fs, fs))
		logger.Info("persistence enabled", "dir", *dataDir)
	}

	cfg := room.DefaultConfig()
	h := hub.New(rootCtx, codec, cfg, logger, hubOpts...)

	handler := transport.NewHandler(h, *serverID, logger,
		transport.WithAllowAllOrigins(*allowAll))

	mux := chi.NewMux()
	mux.Use(middleware.RequestID)
	mux.Use(middleware.RealIP)
	mux.Use(middleware.Recoverer)
	mux.Handle("/room/{id}", handler)
	mux.Get("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.Get("/rooms", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Упрощённый debug-эндпоинт. В Этапе 4 показывает и активные,
		// и «на диске». Полноценные /metrics появятся в Этапе 7.
		body, _ := json.Marshal(h.KnownRooms())
		_, _ = w.Write(body)
	})
	// Этап 5: GET /rooms/{id}/history?from=N&to=M — debug-аудит
	// последних батчей. Не создаёт комнату (peek, не get-or-create).
	mux.Handle("/rooms/{id}/history", transport.HistoryHandler(h, logger))
	if dir := *staticDir; dir != "" {
		if _, err := os.Stat(dir); err == nil {
			// Раздаём фронт через /* — chi отдаёт приоритет точным маршрутам.
			// Имя staticFS, а не fs: иначе затрём outer *store.FileStore и
			// vet/critic закричит «shadowed variable».
			staticFS := http.FileServer(http.Dir(dir))
			// Cache-Control — лечит «верхняя панель иногда не видна»:
			// после пересборки у ассетов новые hashes, а закэшированный
			// index.html ссылается на старые → 404 на CSS/JS → пустой
			// экран без стилей. HTML — no-cache (всегда ревалидация),
			// /assets/* — immutable (имя содержит контент-хеш).
			mux.Handle("/*", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					w.Header().Set("Cache-Control", "no-cache")
				}
				staticFS.ServeHTTP(w, r)
			}))
			logger.Info("serving static", "dir", dir)
		} else {
			logger.Warn("static dir missing, skipping", "dir", dir, "err", err)
		}
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Write/Idle timeout не ставим: WS живёт часами, а «залипшие»
		// соединения чистит heartbeat + ping/pong внутри transport.Handler.
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("main: listen %s: %w", *addr, err)
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()
	logger.Info("server started",
		"addr", *addr, "serverId", *serverID, "allowAllOrigins", *allowAll)

	select {
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("main: serve: %w", err)
	case <-rootCtx.Done():
		logger.Info("shutdown requested")
		sctx, scancel := context.WithTimeout(context.Background(), *shutdownTO)
		defer scancel()
		if err := srv.Shutdown(sctx); err != nil {
			logger.Warn("http shutdown incomplete", "err", err)
		}
		// hub.Shutdown дожидается Run-циклов комнат; каждая комната в
		// shutdownAll уже делает store.Flush/Close. Осталось закрыть
		// «общие» ресурсы самого FileStore.
		if err := h.Shutdown(sctx); err != nil {
			logger.Warn("hub shutdown incomplete", "err", err)
		}
		if fs != nil {
			if err := fs.CloseAll(); err != nil {
				logger.Warn("store closeall", "err", err)
			}
		}
		return nil
	}
}

func parseLevel(s string) slog.Level {
	var l slog.Level
	if err := l.UnmarshalText([]byte(s)); err != nil {
		return slog.LevelInfo
	}
	return l
}
