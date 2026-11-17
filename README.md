# llm-bridge

OpenAI-compatible HTTP адаптер: принимает `/v1/chat/completions` и проксирует
вопрос в постоянную Paseo-сессию (Devin, `smart`-режим) в фиксированном
workspace `C:\OSPanel\domains\Golang\voice-agent`.

Сделан для связки с [Quasar Bridge](https://github.com/skrashevich/quasar-bridge) на
Яндекс Станции: «Алиса, спроси нейросеть: …» → ответ агента из Paseo.

## Архитектура

```
Quasar → POST /v1/chat/completions
  ├─ classify-запрос → opencode/`devin -p` (быстрый путь, вне сессии)
  └─ вопрос → `paseo send <agentId>` → живая сессия Devin → ответ → TTS
```

Сессия постоянная: контекст держит сам агент (история `messages` от Quasar
не нужна — в сессию уходит только последняя реплика), транскрипт виден
в приложении Paseo, prompt-cache тёплый. Персона и контракт TG_SEND
живут в `voice-agent/AGENTS.md`. ID сессии — `run/voice-agent.id`
(gitignored); мёртвая сессия пересоздаётся автоматически.

## Сборка и запуск

```sh
go build -o llm-bridge.exe .
.\llm-bridge.exe
```

Слушает `:8765` по умолчанию. Требуется запущенный демон Paseo и CLI `paseo`
в PATH.

## Конфигурация (env)

| Переменная | Default | Назначение |
|---|---|---|
| `LISTEN_ADDR` | `:8765` | адрес прослушивания |
| `PASEO_BIN` | `paseo` | путь к paseo CLI |
| `PASEO_PROVIDER` | `devin/swe-2-flash-medium` | провайдер/модель сессии |
| `PASEO_WORKSPACE` | `wks_b080f9f7ce6d8e09` | id paseo-workspace сессии (пусто → создать по `VOICE_WORKSPACE_DIR`) |
| `VOICE_WORKSPACE_DIR` | `C:\OSPanel\domains\Golang\voice-agent` | папка воркспейса при создании |
| `VOICE_AGENT_FILE` | `run/voice-agent.id` | файл с id живой сессии |
| `TURN_TIMEOUT` | `90s` | таймаут одного вопроса (send + ожидание) |
| `DEVIN_BIN` / `DEVIN_MODEL` | `devin`, `swe-2-flash-medium` | fallback классификатора (`devin -p`) |
| `SYSTEM_PROMPT` | — | не используется: персона в `voice-agent/AGENTS.md` |
| `PROXY_URL` | — | прокси для исходящих Telegram-запросов |
| `AUTH_TOKEN` | — | если задан, требует `Authorization: Bearer <token>` |
| `CLASSIFIER_ENABLED` | `true` | `false` — classify-запросы уходят в `devin -p` |
| `CLASSIFIER_TIMEOUT` | `6s` | бюджет на классификацию (две попытки) |
| `OPENCODE_BIN` | `opencode` | путь к opencode CLI |
| `OPENCODE_MODEL` | `opencode/flint-40b-free` | модель классификатора |
| `OPENCODE_VARIANT` | `low` | вариант/effort модели opencode |
| `BOT_TOKEN` | — | токен Telegram-бота (прод: `run/secrets.ps1`, gitignored) |
| `BOT_ID` | — | chat_id, куда пересылать |
| `TTS_PREFIX` | — | префикс TTS-разметки к ответам (например `<speaker voice="zahar">`) |

## Отправка в Telegram (маркер TG_SEND)

Просьба переслать в телеграм — любой формулировкой («отправь в телеграм»,
«скинь это в телегу», «перешли мне список») — обрабатывается моделью: по
контракту из `AGENTS.md` она отвечает строкой `TG_SEND:` + текстом пересылки.
Мост вырезает маркер, шлёт payload через Bot API `sendMessage` и озвучивает
подтверждение. Одинаковые пересылки в течение 10 минут отклоняются
(«Уже отправлено.») — голая «спроси нейросеть» повторяет предыдущий вопрос
и раньше дублировала отправки.

## Классификатор маршрутов Quasar

Quasar Bridge шлёт classify-запросы (системный промпт с `{"route":"home|llm|yandex"}`)
в тот же `llm_base_url`, что и обычные вопросы. Бридж распознаёт их по маркеру
`{"route":"` во входящих сообщениях и отвечает вне голосовой сессии, чтобы
не засорять её контекст и таймлайн.

- Классифицирует бесплатный `opencode/flint-40b-free` — запуском
  `opencode run --pure` (anon free-tier Zen принимает только официальный CLI,
  ключа не нужно). Если `CLASSIFIER_ENABLED=false` — одноразовый `devin -p`.
- Ответ всегда валидный JSON `{"route":"..."}`; при ошибке/таймауте —
  `{"route":"yandex"}` (эквивалент `classifier.on_error: yandex` в Quasar).
- Две попытки со сдвигом 1.5 с — первая вернувшаяся побеждает.

## Конфиг Quasar на колонке

Бридж стоит на самой Станции: SSH `root@192.168.1.150 -p 2222` (ключи уже
приняты), состояние в `/data/opt/quasar-bridge/` (`config.json`, `runtime.env`,
`service.log`), приложение в `app/` (v0.7.6). Важно: удалённый sh исполняет
stdin как скрипт — передача файлов через `cat > file <<'EOF'` heredoc
(проверено, CRLF недопустимы).

В `config.json` задано:

- `"routing.default_route": "yandex"` + `"classifier.enabled": false` —
  классификатор выключен: каждый classify-запрос стоил 3–8 с `devin -p` и
  держал mutex (2026-11-17).
- `routing.rules` — явный список: два пасео-правила + 9 дефолтных из
  `config.example.json` той же версии. `paseo_call` (`contains_any`: `пасео`,
  `пассио`, `пассео`, `пасио`, `пасеу`, `пассиу`, `пасьо`, `пацио`, `па сео`,
  `по сео`, `paseo`, `poseo`) и `paseo_short` (regex — «пасе» отдельным
  словом, иначе ловило бы «с**пасе**т») оба → `llm`. Яндекс ASR реально пишет
  «пасео» как **«пассио»**, **«пасио»** и усечённое **«пасе»** — варианты
  собраны по `conversations.jsonl` (`event:"transcript"` — дословный текст;
  журнал включён секцией `conversation_log`). «Скажи пасео …» и
  «спроси нейросеть …» оба ведут в сессию.
- `llm_strip_prefixes` — матрица «скажи/спроси/голое» × `пасео`,`пассио`,
  `пасио`,`пассео`,`пасе` + дефолтные «спроси …»: префикс срезается до
  отправки в llm. Важно: это отдельный список от `routing.rules` — правило
  ловит подстроку в любом месте, а стрип только префикс в начале; вариант ASR
  должен быть в ОБОИХ, иначе вопрос доедет с префиксом. Требует
  `control.sh restart` (HUP его не подхватывает, в отличие от `routing`).

Управление: `app/control.sh restart|status|check|logs`; бэкапы —
`config.json.bak` (до выключения классификатора), `config.json.bak2` (до
правила paseo). Диагностика маршрута: `app/bin/quasar-bridge -config
config.json -route 'фраза'` (нужны env из `runtime.env`).

## Сессия и permissions

- Сессия создаётся в режиме `smart`: read-only операции авто-одобряются.
- Если агент всё же запросил permission — мост сам делает `paseo permit deny`
  и отвечает «Такое действие мне нельзя.» (голосу ждать клика нельзя).
- `paseo send` вызывается с очищенными `PASEO_AGENT_ID`/`PASEO_WORKSPACE_ID` —
  иначе вопросы приходили бы в сессию как agent-message, а не user-промпт.

## Эндпоинты

- `POST /v1/chat/completions` — OpenAI-формат, `stream: true` тоже принимается (SSE, одним чанком)
- `GET /v1/models` — список из одной модели
- `GET /healthz`

## Пример

```sh
curl http://localhost:8765/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"swe-2-flash-medium","messages":[{"role":"user","content":"Привет"}]}'
```

## Особенности

- Вопросы сериализуются mutex'ом — один turn в сессии за раз; `paseo send`
  ждёт завершения, ответ забирается последним `assistant_message` из
  `paseo logs --json`.
- Сессия накапливает контекст со временем; при деградации — удалить
  `run/voice-agent.id` и перезапустить (или создать нового агента в Paseo).
- Старый regex-разбор «отправь/скинь/кинь…» удалён: триггер пересылки —
  семантика модели, ложных срабатываний на «кинь идей» больше нет.
