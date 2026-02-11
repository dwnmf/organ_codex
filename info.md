# План полной замены `codex exec` на API

## Цель
- Полностью убрать запуск внешнего процесса `codex` из оркестратора.
- Перевести генерацию файлов в `coder` на HTTP API (`wire_api=responses`).
- Использовать `reasoning.effort=high` по умолчанию для всех запросов.

## Текущее состояние (as-is)
- Генерация вызывается через subprocess в `internal/agent/worker.go` (`exec.CommandContext(..., "codex", "exec", ...)`).
- `coder` создается в `cmd/orchestrator/main.go` с привязкой к бинарю `codex`.
- Конфиг уже содержит нужные данные для API-провайдера:
  - `model_provider`
  - `model`
  - `model_providers.<provider>.base_url`
  - `model_providers.<provider>.wire_api`
  - `model_reasoning_effort` (будет дефолтить в `high`).

## Целевое состояние (to-be)
- `coder` получает абстракцию `PlanGenerator` вместо прямого `codex exec`.
- Базовая реализация `PlanGenerator` работает через HTTP Responses API (SSE, `stream=true`).
- Значение `reasoning.effort` всегда проставляется:
  - если в конфиге пусто -> `high`
  - если задано -> использовать заданное (валидация: `none|low|medium|high`).
- Поля ответа нормализуются в существующий `codexPlan` (`summary`, `files[]`), чтобы не ломать downstream-логику записи файлов/артефактов.

## План работ

### 1. Вынести контракт генерации
- Добавить интерфейс:
  - `type PlanGenerator interface { Generate(ctx context.Context, req domain.WorkRequestPayload) (codexPlan, error) }`
- В `Coder` заменить прямой вызов `generatePlanWithCodex(...)` на `c.generator.Generate(...)`.
- Сохранить текущий prompt-builder и парсер результата как переиспользуемые функции.

### 2. Реализовать API-генератор
- Добавить новый модуль (например `internal/agent/plan_generator_api.go`):
  - сбор endpoint: `<base_url>/<wire_api>`
  - обязательный `stream=true`
  - body:
    - `model`
    - `instructions` (системные правила ответа)
    - `input` (list формата Responses API)
    - `reasoning: { effort: ... }`
  - SSE-парсинг:
    - читать `response.output_text.delta`
    - завершение по `response.completed`
    - накапливать итоговый текст
  - распарсить итоговый JSON в `codexPlan`.

### 3. Конфиг и валидация
- Расширить runtime-конфиг оркестратора:
  - `backend = "api"` (дефолт)
  - `api_timeout_ms`, `api_retries`, `api_retry_backoff_ms`
  - `reasoning_effort_default = "high"` (или использовать глобальный `model_reasoning_effort`).
- Валидация на старте:
  - выбран `model_provider`
  - найден provider в `model_providers`
  - не пустые `base_url`, `wire_api`, `model`.

### 4. Подключение в `main`
- В `cmd/orchestrator/main.go` собирать `PlanGenerator` по конфигу.
- Передавать `PlanGenerator` в `NewCoder(...)`.
- Удалить зависимость от пути к бинарю `codex` из конструктора `Coder`.

### 5. Обработка ошибок и устойчивость
- Retry для 429/5xx/сетевых ошибок с backoff.
- Таймаут на запрос и отдельный таймаут на SSE чтение.
- Ошибки в формате API маппить в понятные `codex_exec_failed`/`blocked` причины.
- Ограничить максимальный размер накопленного output (защита от runaway stream).

### 6. Логи и телеметрия
- Сохранить существующие decision actions для совместимости монитора:
  - `codex_exec_started`, `codex_exec_progress`, `codex_exec_failed`, `codex_exec_finished`.
- В payload добавить API-метаданные:
  - `provider`, `model`, `wire_api`, `reasoning_effort`, `attempt`.

### 7. Тесты
- Unit:
  - сборка request body (включая дефолт `reasoning.effort=high`)
  - парсинг SSE событий в итоговый текст
  - парсинг итогового JSON в `codexPlan`.
- Integration (`httptest.Server`):
  - успешный stream
  - API error (4xx/5xx)
  - timeout/обрыв stream.
- Regression:
  - существующий happy-path `coder` (запись файлов, artifacts, DONE/BLOCKED) без изменений поведения.

### 8. Полное удаление `codex exec`
- Удалить:
  - код temp schema/output файлов
  - `os/exec` зависимость в `worker.go`
  - поля `codexBinary`, `codexWorkdir` и связанный мертвый код.
- Обновить README/документацию запуска (акцент на API-конфигурацию).

## Критерии готовности (Definition of Done)
- В коде нет вызовов `exec.CommandContext(... codex ...)`.
- `coder` успешно генерирует и пишет файлы через API на реальном endpoint.
- `reasoning.effort=high` применяется по умолчанию и виден в request/логах.
- Все тесты зеленые (`go test ./...`).
- Монитор корректно отображает этапы выполнения без изменений UX.

## Риски и меры
- Риск: нестабильный SSE/прокси.
  - Мера: retry + timeout + лимит буфера + понятные ошибки.
- Риск: несовместимый формат output модели.
  - Мера: строгий JSON prompt + обязательная валидация `summary/files`.
- Риск: скрытая зависимость на старые поля конструктора `NewCoder`.
  - Мера: компиляционный рефактор + покрытие integration-тестом orchestration flow.

## Порядок внедрения
1. Ввести интерфейс и API-генератор параллельно старому коду.
2. Переключить `main` на API backend по умолчанию.
3. Прогнать тесты + smoke на реальном endpoint.
4. Удалить legacy `codex exec` код.
5. Обновить документацию.
