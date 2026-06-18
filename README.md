# TG News Digest Bot

Telegram-бот для ежедневного новостного дайджеста топ-N новостей с AI-ранжированием, категориями, дайджестом по запросу и переводом на язык пользователя.

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

- **Ежедневный дайджест** — автоматическая генерация и рассылка топ-N новостей всем подписчикам, с кратким саммари (1-2 предложения) для каждой новости
- **AI-ранжирование** — два провайдера: llama.cpp (локально) и OpenRouter (облако)
- **Категории/рубрики** — подписчик выбирает интересующие темы через `/categories` (inline-кнопки); новости классифицируются LLM + keyword-фоллбэком, дайджест фильтруется индивидуально под каждого
- **Персональные настройки** — `/settings` открывает inline-меню для времени доставки, timezone, top-N, режима, тихих выходных, языка и тем
- **Режимы дайджеста** — `/mode` переключает формат: `brief`, `detailed`, `executive`, `links`, `why_it_matters`
- **Дайджест по запросу** — `/digest <тема>` находит и кратко суммирует только новости, релевантные произвольной теме пользователя, без рассылки остальным
- **Перевод на язык пользователя** — `/language <язык>` переключает дайджест (заголовки, саммари) на любой язык; перевод выполняется одним LLM-вызовом на каждую языковую группу подписчиков
- **RSS-интеграция** — параллельный сбор из HTTP-URL и локальных XML-файлов
- **Дедупликация** — SHA256-хэширование (title|link) + TTL-кэширование в SQLite (WAL-режим)
- **Автоочистка БД** — удаление записей истории дайджестов старше 30 дней и новостей старше `fetched_items_retention_days` (по умолчанию 3 дня)
- **Отказоустойчивость** — fallback на raw top-N по дате при ошибке LLM, graceful shutdown, авто-отписка заблокированных
- **Healthcheck и dashboard** — HTTP endpoints для мониторинга БД, LLM, Telegram и read-only статистики бота
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

# Категории для классификации новостей (LLM + keyword-фоллбэк).
# Используются в /categories (выбор подписчиком) и в /digest <тема>.
categories:
  - "Большие языковые модели"
  - "Генеративный ИИ"
  - "Научные исследования и публикации"
  - "Open Source модели"
  - "Машинное обучение и инструменты"
  - "Компьютерное зрение"
  - "Обработка естественного языка"
  - "ИИ-агенты и автоматизация"
  - "Робототехника"
  - "ИИ в продуктах и сервисах"
  - "Бизнес и стартапы"
  - "Аппаратное обеспечение и инфраструктура"
  - "Корпоративные новости"
  - "Регулирование и право"
  - "Этика и безопасность ИИ"
  - "Влияние на общество и рынок труда"
  - "Разработка и программирование"
  - "Мнения и аналитика"

app:
  db_path: "./data/bot.db"
  log_level: "info"
  retry_max: 3
  retry_backoff: 2s
  digest_log_path: "./data/bot.log"
  health_port: 9100                  # Порт HTTP-эндпоинта healthcheck
  digest_top_n: 10                   # Количество новостей в ежедневном дайджесте
  fetched_items_retention_days: 3    # Хранить fetched_items не дольше N дней (housekeeping)
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
| `/start` | Регистрация подписчика + актуальная справка по командам |
| `/subscribe` | Подписка на дайджест |
| `/unsubscribe` | Отписка |
| `/digest` | Принудительная генерация и рассылка полного дайджеста (только владелец) |
| `/digest <тема>` | Дайджест по произвольной теме только для запросившего, без рассылки (доступно всем) |
| `/categories` | Выбор интересующих категорий новостей через inline-кнопки (без выбора — приходят все темы) |
| `/addcategory <тема>` | Добавить кастомную тему в персональные категории |
| `/removecategory <тема>` | Удалить кастомную тему из персональных категорий |
| `/language <язык>` | Смена языка дайджеста (например, `/language English`); без аргумента — показывает текущий |
| `/mode <режим>` | Режим дайджеста: `brief`, `detailed`, `executive`, `links`, `why_it_matters`; без аргумента показывает текущий |
| `/settings` | Inline-меню персональных настроек: время, timezone, top-N, режим, тихие выходные, язык и темы |
| `/status` | Статус последнего запуска |

## 🩺 Healthcheck API

Бот exposes HTTP endpoint на порту 9100 (настраивается через `app.health_port`):

```bash
curl http://localhost:9100/health
```

Read-only dashboard:

```bash
open http://localhost:9100/dashboard
curl http://localhost:9100/dashboard.json
```

Dashboard показывает источники RSS, последние ошибки RSS, последние дайджесты, количество подписчиков, статистику отправленных/упавших сообщений и популярные категории.

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

### Покрытие тестами

Цель — **более 70%** покрытия критических пакетов. Текущее состояние:

| Пакет | Покрытие | Статус |
|---|---|---|
| `internal/rss` | 86.4% | ✅ |
| `internal/healthcheck` | 79.6% | ✅ |
| `internal/llm` | 78.1% | ✅ |
| `internal/config` | 80.0% | ✅ |
| `internal/scheduler` | 75.0% | ✅ |
| `internal/formatter` | 67.2% | ⚠️ |
| `internal/storage` | 64.8% | ⚠️ |
| `internal/bot` | 33.8% | 🔴 |
| `cmd/bot` | 0.0% | 🔴 |

Для запуска отчёта о покрытии:

```bash
make coverage
# или
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out  # откроет отчёт в браузере
```

## ⚠️ Обработка ошибок

| Сценарий | Поведение |
|---|---|
| RSS недоступен/таймаут | Пропуск ленты с `warn`-логом, остальные ленты обрабатываются параллельно |
| LLM вернул ошибку/пустоту | Fallback на raw top-N по дате публикации, категория — по ключевым словам |
| LLM недоступен при `/digest <тема>` | Fallback на поиск по ключевым словам темы в заголовке/описании |
| LLM недоступен при переводе (`/language`) | Дайджест отправляется на русском (языке оригинала) без ошибки |
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

**Q: Бот автоматически очищает базу данных?**
A: Да, при запуске бота и затем каждые 24 часа в фоновом режиме:
- записи истории дайджестов (`digest_runs`) старше 30 дней;
- собранные новости (`fetched_items`) старше `app.fetched_items_retention_days` (по умолчанию 3 дня) — независимо от TTL-кэша дедупликации (`rss.cache_ttl`).

**Q: Как настроить категории новостей?**
A: Список категорий задаётся один раз в конфиге (`categories:`) и используется как для классификации новостей LLM (с keyword-фоллбэком, если LLM недоступна), так и для inline-кнопок в `/categories`. Подписчик без выбранных категорий получает дайджест без фильтрации.

**Q: Можно ли получать дайджест не на русском языке?**
A: Да, командой `/language <язык>` (например, `/language English`). Новости всегда ранжируются и суммаризируются на русском, а перевод заголовков и саммари в выбранный язык выполняется отдельным LLM-вызовом — один раз на каждую языковую группу подписчиков, а не на каждого отдельно.

**Q: Можно ли использовать с прокси?**
A: Да, настройте переменные окружения `HTTP_PROXY` / `HTTPS_PROXY`, или используйте MTProxy в конфиге бота (поддержка частичная).

---

**License**: MIT
