# llm-bridge — заметки для агента

OpenAI-совместимый бэкенд `/v1/chat/completions` на `:8765` для Quasar Bridge
на Яндекс.Станции. Один файл `main.go`, Go, без зависимостей кроме stdlib.

## Запуск / рестарт / проверка

```powershell
go build -o llm-bridge.exe .
powershell -NoProfile -ExecutionPolicy Bypass -File run\restart-bridge.ps1   # стоп+старт, печатает pid
curl http://localhost:8765/healthz
```

Логи: `bridge.log` (stdout, все req=… / ответы / ошибки), `bridge.err`.
Фикстуры для ручных тестов: `run\fixture-question.json`, `run\fixture-telegram.json`
(`curl -X POST :8765/v1/chat/completions -d @run\fixture-question.json`).

## Архитектура (актуальная)

```
Quasar (192.168.1.150) → POST :8765
  ├─ classify-запрос (route-литералы в messages) → opencode run | devin -p fallback → {"route":"…"}
  └─ обычный вопрос → `paseo send <voiceAgent> --behavior interrupt` → ответ из `paseo logs --json`
       └─ ответ начинается с TG_SEND: → payload в Bot API → колонке "Отправлено."
```

- Голосовая сессия — **персистентный Paseo-агент** в воркспейсе
  `C:\OSPanel\domains\Golang\voice-agent` (workspace id `wks_b080f9f7ce6d8e09`),
  провайдер `devin/swe-2-flash-medium`, режим `ask`. Контракт ассистента —
  `voice-agent\AGENTS.md` (стиль, TG_SEND, быстрые маршруты fetch).
- `run\voice-agent.id` хранит agent id; пересоздание автоматическое при смерти
  сессии и при простое > `VOICE_IDLE_TTL` (30 мин). **mtime файла = время
  последней активности** — не храни в нём ничего кроме id.
- Дочерним `paseo`-вызовам вычищаются `PASEO_AGENT_ID`/`PASEO_WORKSPACE_ID` —
  иначе демон доставляет промпты как agent-messages. Не убирай это из `paseoCmd`.
- `--behavior interrupt` обязателен: без него send **встраивается** в ещё
  выполняющийся turn (steer), и вопрос тонет в зависшем ресёрче.
- Классификаторные запросы НЕ должны попадать в голосовую сессию — они цитируют
  фразу пользователя и засоряют контекст. `CLASSIFIER_ENABLED=false` → fallback
  `devin -p` во временной папке.

## Telegram

- Токен/чат — `BOT_TOKEN`/`BOT_ID` в `run\secrets.ps1` (dot-source из
  `start-bridge.ps1`). **Файл в .gitignore, секреты в tracked-файлы не писать.**
- Модель помечает пересылку маркером `TG_SEND:` (контракт в voice-agent\AGENTS.md),
  мост шлёт `sendMessage`. Regex-разбора фраз больше нет — намерение решает модель.
- Dedup: `lastSent[ip]` 10 мин — повтор payload → «Уже отправлено.» (quasar по
  голой «спроси нейросеть» переотправляет прошлый вопрос — закрывает дыру).
- `api.telegram.org` напрямую с этой машины мёртв (DPI): идёт через
  `HTTPS_PROXY=http://127.0.0.1:1080` (Shadowsocks.exe; `ProxyFromEnvironment`).
  Туннель флапает → `sendTelegram` ретраит транспортные ошибки до 4 раз.
  `sendTelegramOnce` убирает URL из url.Error — иначе токен светится в логе.
- Если ТГ массово умирает — сначала проверить жив ли апстрим ss
  (`curl -x http://127.0.0.1:1080 https://www.google.com`), а не код моста.

## Quasar на колонке

См. раздел «Конфиг Quasar на колонке» в README — SSH `root@192.168.1.150 -p 2222`,
конфиг в `/data/opt/quasar-bridge/`, `app/control.sh restart`. Важные факты:

- Голая «спроси нейросеть» — команда ПОВТОРА прошлого вопроса внутри quasar,
  до моста не доходит. Вопрос передаётся в той же фразе.
- `routing.rules` (пасео-префиксы + дефолтные) и `llm_strip_prefixes` — это
  ДВА списка: правило ловит подстроку, стрип режет только префикс; ASR-вариант
  («пассио», «пасио», «пасе»…) должен быть в обоих. Стрип требует restart.

## Quirks этой машины

- Шелл глючит на длинных/кавычковых командах и `$_` → сложные команды писать в
  `run\*.ps1` и звать `powershell -File`. `tail`, `head`, heredoc — нет.
- `findstr` с несколькими паттернами ругается — по одному или PowerShell.
- `paseo send` из-под paseo-агента = agent-message (см. выше про env).
- Тесты гипотез: сначала спросить у пользователя предсказание (см. его AGENTS.md).

## Что нельзя ломать

- `TTS_PREFIX` дописывается к ответу — колонка озвучивает только его хвост.
- Режим сессии `ask`: любой exec-промпт = мёртвый голосовой turn.
  AGENTS.md запрещает Shell — держать этот запрет; permission-диалогов
  в голосе быть не должно.
- Мьютекс `b.mu` — одна голосовая очередь; не распараллеливать askVoice.
