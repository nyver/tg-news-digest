# ---------- build stage ----------
FROM golang:1.23-alpine AS builder

RUN apk --no-cache add git ca-certificates gcc musl-dev

WORKDIR /app

# Кэшируем зависимости Go
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 go build -o /bot ./cmd/bot

# ---------- runtime stage ----------
FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata

WORKDIR /app

COPY --from=builder /bot /app/bot
COPY configs/config.example.yaml /app/config.example.yaml

ENV TG_NEWS_BOT_TOKEN=""
ENV TG_NEWS_LLM_ENDPOINT="http://host.docker.internal:8080"
ENV TG_NEWS_APP_HEALTH_PORT="9100"

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:9100/health || exit 1

EXPOSE 9100

ENTRYPOINT ["/app/bot"]
CMD ["--config", "config.example.yaml"]
