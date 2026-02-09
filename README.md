# organ_codex

MVP оркестратор для codex-ориентированного мультиагентного workflow на `Go + SQLite`.

## Что реализовано
- Оркестратор с lifecycle задач, retry/dispatch loop, watchdog.
- Прямое общение агентов через in-process bus.
- Централизованный контроль:
  - file permissions (`default deny`);
  - channels + type allowlist + max messages;
  - hop limit на задачу.
- SQLite state store:
  - задачи, permissions, channels, messages, acks, artifacts, idempotency, audit logs.
- Recovery:
  - pending messages сохраняются в БД и продолжают dispatch после рестарта.
- Demo-агенты:
  - `planner` и `coder`.

## Запуск
```powershell
& "C:\Program Files\Go\bin\go.exe" mod tidy
& "C:\Program Files\Go\bin\go.exe" test ./...
& "C:\Program Files\Go\bin\go.exe" run ./cmd/orchestrator --demo
```

По умолчанию конфигурация читается из `~/.codex/config.toml`.

## Конфиг
Базовые поля берутся из `~/.codex/config.toml` полностью.

Дополнительно можно задать runtime-параметры оркестратора:
```toml
[orchestrator]
addr = ":8091"
db_path = "data/organ_codex.db"
workspace_root = "workspace"
dispatch_interval_ms = 250
watchdog_interval_ms = 3000
retry_delay_ms = 1000
max_retries = 6
idle_timeout_ms = 45000
default_max_hops = 8
```

## HTTP API
- `GET /healthz`
- `GET /config`
- `GET /tasks`
- `POST /tasks`
- `GET /tasks/{id}`
- `POST /tasks/{id}/start`
- `POST /tasks/{id}/permissions`
- `POST /tasks/{id}/channels`
- `GET /tasks/{id}/messages`
- `GET /tasks/{id}/acks`
- `GET /tasks/{id}/decisions`

Пример:
```powershell
curl -Method Post -Uri http://localhost:8091/tasks -ContentType "application/json" -Body '{
  "goal":"Implement feature",
  "scope":"Use planner->coder flow",
  "owner_agent":"planner",
  "auto_start":false,
  "max_hops":8
}'
```

## Единое окно (оркестратор + монитор + ввод промпта)
По умолчанию монитор работает в embedded-режиме: сам поднимает оркестратор и показывает live-взаимодействия в одном окне.

```powershell
& "C:\Program Files\Go\bin\go.exe" run ./cmd/monitor --addr http://localhost:8091 --embedded
```

Внизу окна есть поле `Prompt -> Orchestrator`: нажмите `Enter`, и монитор автоматически:
- создаст задачу из промпта;
- выдаст стандартные permissions/channels;
- запустит выполнение.

Горячие клавиши:
- `F5` обновить
- `F10` выход
- `Ctrl+L` фокус в prompt
- `Ctrl+T` фокус на таблицу задач

В правых панелях видны:
- задачи и их статусы;
- сообщения между агентами (`from -> to`, тип, статус, retry);
- ack от каждого агента;
- решения оркестратора (deny/blocked/done и причины).

Хотите использовать только UI поверх уже запущенного оркестратора:

```powershell
& "C:\Program Files\Go\bin\go.exe" run ./cmd/monitor --addr http://localhost:8091 --embedded=false
```
