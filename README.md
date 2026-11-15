# devin-llm-bridge

OpenAI-compatible HTTP адаптер над `devin -p`: принимает `/v1/chat/completions`,
вызывает Devin CLI (SWE-2 и другие модели) и возвращает ответ в формате OpenAI.

Сделан для связки с [Quasar Bridge](https://github.com/skrashevich/quasar-bridge) на
Яндекс Станции: «Алиса, спроси нейросеть: …» → ответ от Devin.

## Сборка и запуск

```sh
go build -o devin-llm-bridge.exe .
.\devin-llm-bridge.exe
```

Слушает `:8765` по умолчанию.

## Конфигурация (env)

| Переменная | Default | Назначение |
|---|---|---|
| `LISTEN_ADDR` | `:8765` | адрес прослушивания |
| `DEVIN_BIN` | `devin` | путь к devin CLI |
| `DEVIN_MODEL` | `swe-2-high` | модель (`devin models list`) |
| `DEVIN_TIMEOUT` | `90s` | таймаут одного вызова |
| `MAX_HISTORY` | `10` | сколько сообщений истории включается в промпт |
| `SYSTEM_PROMPT` | голосовой ассистент | префикс промпта |
| `PROXY_URL` | — | если задан, прокидывается в `HTTP(S)_PROXY`/`ALL_PROXY` дочернего процесса |
| `AUTH_TOKEN` | — | если задан, требует `Authorization: Bearer <token>` |

## Эндпоинты

- `POST /v1/chat/completions` — OpenAI-формат, `stream: true` тоже принимается (SSE, одним чанком)
- `GET /v1/models` — список из одной модели
- `GET /healthz`

## Пример

```sh
curl http://localhost:8765/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"swe-2-high","messages":[{"role":"user","content":"Привет"}]}'
```

## Особенности

- Вызовы сериализуются mutex'ом — одновременно работает один `devin -p`.
- Каждый вызов идёт в свежий временный каталог → stateless; память диалога
  обеспечивает история `messages` от клиента.
- `--permission-mode auto` (default): read-only инструменты авто-одобрены,
  write/exec в non-interactive режиме не получат подтверждения — голосовой
  prompt-injection не выполнится.
