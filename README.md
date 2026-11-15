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
| `DEVIN_MODEL` | `swe-2-flash-medium` | модель (`devin models list`) |
| `DEVIN_TIMEOUT` | `90s` | таймаут одного вызова |
| `MAX_HISTORY` | `10` | сколько сообщений истории включается в промпт |
| `SYSTEM_PROMPT` | голосовой ассистент | префикс промпта |
| `PROXY_URL` | — | если задан, прокидывается в `HTTP(S)_PROXY`/`ALL_PROXY` дочернего процесса |
| `AUTH_TOKEN` | — | если задан, требует `Authorization: Bearer <token>` |
| `CLASSIFIER_BASE_URL` | `https://api.z.ai/api/paas/v4` | OpenAI-совместимый endpoint классификатора |
| `CLASSIFIER_API_KEY` | — | ключ классификатора (пусто → fast-path выключен) |
| `CLASSIFIER_MODEL` | `glm-4.5-air` | модель классификатора |
| `CLASSIFIER_TIMEOUT` | `6s` | бюджет на классификацию (две попытки) |
| `CLASSIFIER_PROXY` | `env` | `direct` — без прокси; URL — свой прокси; `env` — как у процесса |
| `CLASSIFIER_EXTRA` | — | JSON, мержится в тело запроса классификатора |
| `TTS_PREFIX` | — | префикс TTS-разметки к ответам (например `<speaker voice="zahar">`) |

## Классификатор маршрутов Quasar

Quasar Bridge шлёт classify-запросы (системный промпт с `{"route":"home|llm|yandex"}`)
в тот же `llm_base_url`, что и обычные вопросы. Бридж распознаёт их по маркеру
`{"route":"` во входящих сообщениях и отвечает дешёвой моделью за ~1 с
вместо запуска `devin -p` (5–8 с и блокировка общего mutex'а).

- Ответ всегда валидный JSON `{"route":"..."}`; при ошибке/таймауте —
  `{"route":"yandex"}` (эквивалент `classifier.on_error: yandex` в Quasar).
- Две попытки со сдвигом 1.5 с — первая вернувшаяся побеждает.
- `thinking: disabled` отправляется по умолчанию (GLM reasoning добавлял ~5 с);
  переопределяется через `CLASSIFIER_EXTRA`.

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
- `--permission-mode auto` (default): read-only инструменты авто-одобрены —
  по умолчанию SYSTEM_PROMPT разрешает только веб-поиск (погода, новости).
  write/exec в non-interactive режиме не получат подтверждения — голосовой
  prompt-injection не выполнится.
