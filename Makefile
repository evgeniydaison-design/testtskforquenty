# Makefile для локальной разработки и CI.
# Предполагается Unix-шелл (Git Bash на Windows / WSL / Linux / macOS).
# Команды — обычные `go`/`npm`, можно запускать и вручную из PowerShell.

GO         ?= go
NPM        ?= npm
BIN_DIR    ?= bin
SERVER_BIN ?= $(BIN_DIR)/board-server
# DATA — каталог персистентности (Этап 4). Пусто = сервер «в памяти».
# Пример: `make run DATA=./data`. Непустое значение пробрасывается
# в флаг -data; сам каталог создаётся при старте.
DATA       ?=

.PHONY: help tidy build run test test-race bench lint fmt vet cover clean \
        web-install web-dev web-build web-test

help:
	@echo "build        - собрать сервер в $(SERVER_BIN)"
	@echo "run          - запустить сервер (dev-адрес :8080)"
	@echo "test         - краткие тесты без -race"
	@echo "test-race    - тесты с -race (обязательный gate)"
	@echo "bench        - бенчмарки горячих путей"
	@echo "lint         - golangci-lint"
	@echo "cover        - HTML-отчёт покрытия"
	@echo "web-install  - npm ci в web/"
	@echo "web-dev      - vite dev (proxy /room -> :8080)"
	@echo "web-build    - прод-сборка статики в web/dist"

tidy:
	$(GO) mod tidy

build:
	$(GO) build -trimpath -o $(SERVER_BIN) ./cmd/server

run:
	@if [ -n "$(DATA)" ]; then \
		$(GO) run ./cmd/server -addr :8080 -static ./web/dist -data $(DATA); \
	else \
		$(GO) run ./cmd/server -addr :8080 -static ./web/dist; \
	fi

test:
	$(GO) test ./...

test-race:
	$(GO) test -race -count=1 ./...

bench:
	$(GO) test -run '^$$' -bench . -benchmem -benchtime 2s ./...

cover:
	$(GO) test -coverprofile coverage.out ./...
	$(GO) tool cover -html coverage.out -o coverage.html

lint:
	golangci-lint run ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

clean:
	rm -rf $(BIN_DIR) coverage.out coverage.html web/dist

# --- web (Vite + TS) ---
web-install:
	cd web && $(NPM) ci

web-dev:
	cd web && $(NPM) run dev

web-build:
	cd web && $(NPM) ci && $(NPM) run build

web-test:
	cd web && $(NPM) test
