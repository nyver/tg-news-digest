# TG News Digest Bot

Telegram-бот для ежедневного новостного дайджеста топ-10 новостей с AI-ранжированием через локальную LLM.

![Go](https://img.shields.io/badge/Go-1.23-blue.svg)
![License](https://img.shields.io/badge/license-MIT-green.svg)

## 📋 Содержание

- [Возможности](#-возможности)
- [Структура проекта](#-структура-проекта)
- [Быстрый старт](#-быстрый-старт)
- [Docker](#-docker)
- [Docker Compose](#-docker-compose)
- [Конфигурация](#-конфигурация)
- [Команды бота](#-команды-бота)
- [Healthcheck API](#healthcheck-api)
- [Разработка](#-разработка)
- [Обработка ошибок](#-обработка-ошибок)
- [FAQ](#-faq)

## ✨ Возможности

- **Ежедневный дайджест** — автоматическая генерация и рассылка топ-10 новостей всем подписчикам
- **AI-ранжирование** — два провайдера: llama.cpp (локально) и OpenRouter (облако)
- **RSS-интеграция** — параллельный сбор из HTTP-URL и локальных XML-файлов
- **Дедупликация** — SHA256-хэширование (title|link) + TTL-кэширование в SQLite (WAL-режим)
- **Отказоустойчивость** — fallback на raw top-10 при ошибке LLM, graceful shutdown, авто-отписка заблокированных
- **Healthcheck** — HTTP endpoint для мониторинга БД, LLM и Telegram
- **Двойное логирование** — JSON в файл + текст в stdout

## 🏗 Структура проекта

```
├── cmd/bot/main.go           # Точка входа
├── internal/
│   ├── bot/                  # Telegram Bot логика
│   ├── config/               # Загрузка конфигурации
│   ├── formatter/            # Форматирование сообщений
│   ├── healthcheck/          # Health HTTP endpoint
│   ├── llm/                  # LLM-клиент (llama.cpp)
│   ├── models/               # Доменные модели
│   ├── rss/                  # RSS-фетчер
│   ├── scheduler/            # Cron-планировщик
│   ├── storage/              # SQLite хранилище
│   ├── tgbot/                # (пустой) Заготовка для обёртки над Telegram API
│   └── version/              # Версия приложения
├── configs/
│   └── config.example.yaml   # Пример конфигурации
├── Dockerfile                # Docker-образ (multi-stage)
├── data/                      # БД и логи
└── tests/                     # Юнит-тесты
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

# Собрать и запустить (Windows)
build.bat
.\bot.exe --config configs/config.yaml

# Или через Makefile (Linux/macOS/Windows с GnuWin32)
make build
./bin/tg-news-digest --config configs/config.yaml
```

## 🐳 Docker

### Сборка образа

```bash
docker build -t tg-news-digest .
```

### Запуск

```bash
docker run -d \
  --name tg-news-digest \
  -p 9100:9100 \
  -v $(pwd)/data:/app/data \
  -v $(pwd)/configs:/app/configs \
  -e TG_NEWS_BOT_TOKEN="YOUR_BOT_TOKEN" \
  -e TG_NEWS_LLM_ENDPOINT="http://host.docker.internal:8080" \
  tg-news-digest
```

Или с кастомным конфиг-файлом:

```bash
docker run -d \
  --name tg-news-digest \
  -p 9100:9100 \
  -v ./configs/config.yaml:/app/config.example.yaml:ro \
  tg-news-digest
```

### Healthcheck

Healthcheck встроен в образ и проверяет HTTP-эндпоинт каждые 30 секунд:

```bash
docker inspect --format='{{.State.Health.Status}}' tg-news-digest
```

## 🐙 Docker Compose

### Быстрый запуск

```bash
# Подготовка конфигурации
cp configs/config.example.yaml configs/config.yaml
# Отредактируйте configs/config.yaml — укажите токен бота и RSS-ленты

# Сборка и запуск
docker compose up -d
```

### Просмотр логов

```bash
docker compose logs -f tg-news-digest
```

### Остановка

```bash
docker compose down
# С удалением данных (БД будет сброшена)
docker compose down -v
```

### Переопределение конфигурации

По умолчанию используется `configs/config.yaml`, смонтированный как `config.example.yaml` внутри контейнера.
Чтобы использовать другой файл:

```yaml
# docker-compose.override.yml
services:
  tg-news-digest:
    volumes:
      - ./configs/production.yaml:/app/config.example.yaml:ro
```

И запустить:

```bash
docker compose -f docker-compose.yml -f docker-compose.override.yml up -d
```

## ⚙️ Конфигурация

Файл: `configs/config.yaml`

```yaml
bot:
  token: "YOUR_BOT_TOKEN_HERE"     # Токен от @BotFather
  parse_mode: "HTML"               # HTML или MarkdownV2
  owner_chat_id: 0                 # Chat ID для admin-команд
  mtproxy:
    enabled: false                 # Включить использование MTProxy (частичная поддержка)
    host: "example.com"            # Адрес MTProxy-сервера
    port: 443                      # Порт MTProxy-сервера (по умолчанию 443)
    secret: ""                     # Base64-кодированный секрет прокси

rss:
  feeds:
    - "https://rsshub.app/telegram/channel/durov"
    - "https://rsshub.app/telegram/channel/meduzanews"
  max_items_per_feed: 50
  fetch_timeout: 15s
  cache_ttl: 24h

llm:
  provider: "llama-cpp"            # llama-cpp или openrouter
  endpoint: "http://127.0.0.1:8080"
  model: "llama-3-8b-instruct-Q5_K_M"
  api_key: ""                      # Для openrouter обязателен
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
  retry_max: 3
  retry_backoff: 2s
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
make fmt                # Форматирование кода
make clean              # Очистка артефактов
```

### Сборка (Windows)

```bash
build.bat
```

### Тесты

```bash
make test
```

```bash
# Конкретный пакет
go test ./internal/bot/ -v -count=1
```

## ⚠️ Обработка ошибок

| Сценарий | Поведение |
|---|---|
| RSS недоступен/таймаут | Пропуск ленты с `warn`-логом, остальные ленты обрабатываются параллельно |
| LLM вернул ошибку/пустоту | Fallback на raw top-10 по дате публикации |
| Telegram rate limit (429) | Пакетная отправка с 50ms задержкой между сообщениями |
| Блокировка бота / активация удалена | Подписчик автоматически помечается как неактивный |
| БД недоступна | Аварийный выход |
| Graceful shutdown | SIGINT/SIGTERM → 2s drain, остановка scheduler и long-polling |

## ❓ FAQ

**Q: Где взять RSS-ленты для Telegram-каналов?**
A: Используйте RSSHub (`rsshub.app/telegram/channel/<channel_name>`) или альтернативные мосты: `tg2rss`, публичные списки (`t.me/s/<channel_name>`). Также поддерживаются локальные XML-файлы — укажите путь в конфиге, например `./data/rss/local-feed.xml`.

**Q: Какие LLM-провайдеры поддерживаются?**
A: Два провайдера через единый интерфейс:
- **llama-cpp** — локальный сервер llama.cpp с OpenAI-compatible API (endpoint из конфига, optional API key)
- **openrouter** — облачный доступ через OpenRouter.ai (API key обязателен)

Переключается полем `llm.provider` в конфиге.

**Q: Что если LLM недоступен?**
A: Бот автоматически переключится на fallback — топ-10 по дате публикации без AI-ранжирования.

**Q: Как часто выходит дайджест?**
A: По расписанию cron — по умолчанию `0 9 * * *` (ежедневно в 09:00 по Moscow Time). Можно изменить в конфиге или через `/digest` (owner).

**Q: Как бэкапить базу данных?**
A: SQLite в WAL-режиме. Используйте `sqlite3 .dump` или просто копию файла `data/bot.db`.

**Q: Можно ли использовать с прокси?**
A: Да, настройте переменные окружения `HTTP_PROXY` / `HTTPS_PROXY`, или используйте MTProxy в конфиге бота (поддержка частичная).

---

**License**: MIT
