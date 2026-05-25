# TG News Digest Bot

Telegram-бот для ежедневного новостного дайджеста топ-10 новостей с AI-ранжированием через локальную LLM.

![Go](https://img.shields.io/badge/Go-1.23-blue.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)

## 📋 Содержание

- [Возможности](#-возможности)
- [Архитектура](#-архитектура)
- [Быстрый старт](#-быстрый-старт)
- [Конфигурация](#-конфигурация)
- [Команды бота](#-команды-бота)
- [Развёртывание](#-развёртывание)
  - [Local development](#local-development)
  - [Docker](#docker)
  - [Systemd](#systemd)
- [Healthcheck API](#healthcheck-api)
- [Разработка](#-разработка)
- [Тесты](#-тесты)
- [Обработка ошибок](#-обработка-ошибок)
- [FAQ](#-faq)

## ✨ Возможности

- **Ежедневный дайджест** — автоматическая генерация и рассылка топ-10 новостей
- **AI-ранжирование** — llama.cpp (OpenAI-совместимый API) для выбора и суммаризации новостей
- **RSS-интеграция** — параллельный сбор из нескольких источников
- **Дедупликация** — SHA256-хэширование + TTL-кэширование в SQLite
- **Отказоустойчивость** — retry, fallback на raw top-10, graceful shutdown
- **Healthcheck** — HTTP endpoint для мониторинга всех компонентов

## 🏗 Архитектура

```
┌──────────────┐    ┌──────────────┐    ┌──────────────┐
│  Scheduler   │───▶│  RSS Fetcher │───▶│   LLM Client │
│  (cron)      │    │  (parallel)  │    │ (llama.cpp)  │
└──────────────┘    └──────┬───────┘    └──────┬───────┘
                           │                   │
                    ┌──────▼───────┐    ┌──────▼───────┐
                    │  Dedup/Filter│───▶│  Formatter   │
                    │  SQLite      │    │  HTML/MD     │
                    └──────────────┘    └──────┬───────┘
                                               │
                                      ┌────────▼────────┐
                                      │ Telegram Bot    │
                                      │  (polling)      │
                                      └─────────────────┘
```

## 🚀 Быстрый старт

### Предварительные требования

- Go 1.23+
- Telegram Bot Token (получите от [@BotFather](https://t.me/BotFather))
- Локальная LLM через llama.cpp (опционально, есть fallback)

### Установка

```bash
# Клонировать репозиторий
git clone https://github.com/nyver/tg-news-digest.git
cd tg-news-digest

# Настроить конфигурацию
cp configs/config.example.yaml configs/config.yaml
# Отредактируйте configs/config.yaml — укажите токен бота и RSS-ленты

# Собрать и запустить
go build -o ./tg-news-digest.exe ./cmd/bot
./tg-news-digest.exe --config configs/config.yaml
```

## ⚙️ Конфигурация

Файл: `configs/config.yaml`

```yaml
bot:
  token: "YOUR_BOT_TOKEN_HERE"     # Токен от @BotFather
  parse_mode: "HTML"               # HTML или MarkdownV2
  owner_chat_id: 0                 # Chat ID для admin-команд

rss:
  feeds:
    - "https://rsshub.app/telegram/channel/durov"
    - "https://rsshub.app/telegram/channel/meduzanews"
  max_items_per_feed: 50
  fetch_timeout: 15s
  cache_ttl: 24h

llm:
  endpoint: "http://127.0.0.1:8080"
  model: "llama-3-8b-instruct-Q5_K_M"
  temperature: 0.3
  max_tokens: 2000
  context_window: 8192
  timeout: 60s

schedule:
  cron: "0 9 * * *"               # Ежедневно в 09:00
  timezone: "Europe/Moscow"

app:
  db_path: "./data/bot.db"
  log_level: "info"
  health_port: 9100
  digest_log_path: "./data/bot.log"
```

### Переменные окружения

Все параметры можно переопределить через ENV с префиксом `TG_NEWS_`:

```bash
export TG_NEWS_BOT_TOKEN="123456:ABC-DEF..."
export TG_NEWS_LLM_ENDPOINT="http://localhost:8080"
```

## 🤖 Команды бота

| Команда | Описание |
|---|---|
| `/start` | Регистрация подписчика + приветствие |
| `/subscribe` | Подписка на дайджест |
| `/unsubscribe` | Отписка |
| `/digest` | Принудительная генерация дайджеста (только владелец) |
| `/status` | Статус последнего запуска |

## 📦 Развёртывание

### Local development

```bash
make run                    # Запустить в режиме разработки
make build                  # Собрать бинарник
./bin/tg-news-digest \
    --config configs/config.yaml
```

### Docker

```bash
# Собрать и запустить
make docker-run

# Или через docker-compose
make docker-compose-up

# С переменными окружения
TG_NEWS_BOT_TOKEN="123456:ABC..." docker-compose up -d
```

### Systemd

```bash
# Установка через Makefile
make build
sudo cp bin/tg-news-digest /usr/local/bin/
sudo cp tg-news-digest.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now tg-news-digest

# Проверка статуса
sudo systemctl status tg-news-digest
journalctl -u tg-news-digest -f
```

## 🩺 Healthcheck API

Бот exposes HTTP endpoint на порту 9100 (настраивается через `app.health_port`):

```bash
curl http://localhost:9100/health
```

Пример ответа:

```json
{
  "status": "healthy",
  "started_at": "2026-05-24T09:00:00Z",
  "duration": "5.2s",
  "checks": {
    "database": { "status": "ok", "duration": "2ms" },
    "llm": { "status": "ok", "duration": "150ms" },
    "telegram": { "status": "ok", "duration": "320ms" }
  }
}
```

Статусы:
- `healthy` — все компоненты работают
- `degraded` — некоторые компоненты недоступны (warning)
- `unhealthy` — критические ошибки (HTTP 500)

## 🛠 Разработка

### Makefile targets

```bash
make build              # Собрать бинарник
make run                # Запустить в dev-режиме
make test               # Запустить тесты с race detector
make coverage           # Отчёт покрытия (coverage.html)
make lint               # Запустить линтер
make lint-fix           # Линтер с авто-фиксами
make fmt                # Форматирование кода
make clean              # Очистка артефактов
make docker             # Собрать Docker-образ
make install-sys        # Установить systemd service
```

### Структура проекта

```
├── cmd/bot/main.go           # Точка входа
├── internal/
│   ├── bot/                  # Telegram Bot API
│   ├── config/               # Загрузка конфигурации
│   ├── formatter/            # Форматирование сообщений
│   ├── healthcheck/          # Health HTTP endpoint
│   ├── llm/                  # LLM-клиент (llama.cpp)
│   ├── models/               # Доменные модели
│   ├── rss/                  # RSS-фетчер
│   ├── scheduler/            # Cron-планировщик
│   └── storage/              # SQLite хранилище
├── configs/
│   ├── config.example.yaml   # Пример конфигурации
│   └── config.yaml           # Активная конфигурация
├── data/                      # БД и логи
├── tests/                     # Интеграционные тесты
├── Dockerfile                 # Multi-stage Dockerfile
├── docker-compose.yml         # Docker Compose
├── tg-news-digest.service     # Systemd unit
├── Makefile                   # Build automation
└── .golangci.yml              # Linter config
```

## 🧪 Тесты

```bash
# Все тесты
make test

# С покрытием
make coverage

# Конкретный пакет
go test ./internal/bot/ -v -count=1
```

## ⚠️ Обработка ошибок

| Сценарий | Поведение |
|---|---|
| RSS недоступен/таймаут | Retry (3x), пропуск ленты, `warn`-лог |
| LLM вернул ошибку/пустоту | Fallback на raw top-10 по дате |
| Telegram rate limit (429) | Пакетная отправка с 50ms задержкой |
| БД недоступна | Аварийный выход |
| Контекст LLM переполнен | Усечение описаний, повторный запрос |

## ❓ FAQ

**Q: Где взять RSS-ленты для Telegram-каналов?**  
A: Используйте RSSHub (`rsshub.app/telegram/channel/<channel_name>`) или альтернативные мосты: `tg2rss`, публичные списки (`t.me/s/<channel_name>`).

**Q: Что если llama.cpp недоступен?**  
A: Бот автоматически переключится на fallback — топ-10 по дате публикации с пометкой `⚠️ AI недоступен`.

**Q: Как часто выходит дайджест?**  
A: По расписанию cron — по умолчанию `0 9 * * *` (ежедневно в 09:00 по Moscow Time). Можно изменить в конфиге или через `/digest` (owner).

**Q: Как бэкапить базу данных?**  
A: SQLite в WAL-режиме. Используйте `sqlite3 .dump` или просто копию файла `data/bot.db`.

**Q: Можно ли использовать с прокси?**  
A: Да, настройте переменные окружения `HTTP_PROXY` / `HTTPS_PROXY`.

---

**License**: MIT
