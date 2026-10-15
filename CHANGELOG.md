# Изменения

## Не выпущено

- Recover a retained scoped confirmation after a verified unseen owner attempt by submitting the exact saved command once in the same epoch and unchanged scope/settings. Keep passive Resolve, pending-owner suppression, recorded-outcome replay and reset/indeterminate behavior. Bound redelivery to ten seconds and preserve proposal serialization.

- Bound the test-only commit timeout probe's owner-entry wait and allow one second
  for actual HTTP dispatch. Keep the unknown outcome and single-attempt assertions.

- Accept monotonically newer Auth settings on an unchanged runtime registration.
  Preserve the current runtime and its retained proposal/attempt state without
  another warehouse factory call. Reject settings rollback, zone changes at the
  same version, changed stable identity, cancellation and closed scopes.

- Release the registry state mutex before warehouse discovery during registration. Preserve one factory call for concurrent identical registrations, support cancellation while waiting, and recheck closure before publishing a new runtime.

- Fixed JWKS recovery at unchanged injected owner time with a separate general host retry clock. JWT and key-cache validity retain owner time; rollback and cancellation remain bounded.
- Reject FIFO private config/credentials without a writer using nonblocking no-follow descriptor admission.
- Reject malformed clock read/set/checkpoint responses before Go parsing, matching protocol offset ranges and nanosecond precision.

- Added a test-only Serving protocol 1.1.0 server/control image as an explicit S4 binding supplement. Actual scoped process mechanisms use one controlled clock; private authenticated Unix controls provide readiness, clock compare-and-set/readback and checkpoints with bounded replay/cancellation. Production handlers remain unchanged.
- Documented the missing distinct Serving Auth discovery credential binding. Factory/Auth integration, independent supplement review and J1/E real-service acceptance remain required.

- BREAKING: the executable now serves only the private restaurant-scoped backend v2 API. Supply distinct caller transport keys, Auth discovery URL and packaged tzdata version; migrate legacy executable callers to scoped routes and captured epochs. Legacy bootstrap.NewHTTP remains an explicit library composition.
- Added bounded authenticated Auth registry/JWKS transport, current peer readiness selection, process reconciliation and cancellation/join on shutdown. Auth remains the sole signer and durable lifecycle owner. Discovery failure preserves unknown confirmation outcomes.
- Removed host port publication from the local Serving Compose configuration and declared required private topology variables.
- Added process red/green tests and frozen-schema response checks for catalog, menu, stop-list, FEFO recommendations, proposals, exact confirmation/replay, epoch reset, failed discovery and signing-key readiness. J1/E real-service acceptance remains pending.

- Preserve unknown confirmation outcomes during peer discovery outages, including cached replay and passive resolution. Preserve explicit reset outcomes and pre-dispatch proposal refusals.
- Validate passive orders before recording success. Reject empty accepted positions/allocations and future order instants; recover through valid passive replies without a second commit.
- Added Auth-only lifecycle registration/close, current-epoch readiness reconciliation, retained-handle fencing and idempotent partial cleanup without removing neighboring restaurant state.
- Scoped proposals and confirmations retain their Warehouse epoch and captured settings. Unknown retries resolve passively; Serving/peer epoch loss returns RuntimeReset/indeterminate before a fresh commit, including cached confirmations.
- Added frozen passive command/attempt/order-ID queries and saved-epoch headers. Scoped ConfirmationPending uses HTTP 200; the legacy handler keeps its compatibility behavior.
- Added red/green lifecycle, lost-reply, timezone-change, restart, exact-quantity and cancellation regressions. J1/E real-service acceptance remains a separate gate.

- Fixed scoped Warehouse refusal status forwarding for InvalidRequest, ServiceUnavailable, LimitExceeded, ProductConflict and DishConflict. Reject conflicting batch expiry or product units across an entire captured menu before recording context, while preserving exact quantities and equivalent instants.
- Added restaurant-bound Warehouse v2 HTTP ports with original Auth token verification, consumer route/capability checks, epoch headers, bounded decoding and inherited request deadlines.
- Scoped FEFO results preserve exact quantities, units, settings and explicit-offset instants; inconsistent or expired allocations and widened rechecks fail closed. Recommendations retain all tied reasons and do not reserve stock.
- Forward verified committed expiry-preparation metadata across private Serving responses, including calculation failures; preserve downstream retryability and unknown modifying outcomes.
- Added focused scope, credential, payload, FEFO, metadata and Ed25519 composition regressions. Passive scoped resolution and lifecycle are wired into the process.

- Added an explicitly registered restaurant runtime with isolated proposal/attempt stores and tenant-bound Warehouse factory ports. No request implicitly creates a tenant.
- Added private scoped HTTP composition with Auth Ed25519 JWT verification, Gateway transport authentication, per-route capability/operation checks and copied request scope before business dispatch.
- Scoped results require observed Warehouse context and preserve exact metadata; missing context cannot fabricate a successful result or a definite mutation outcome.
- Scoped handlers reject browser identity/context headers and cookies. The selected legacy handler retains its behavior. Lifecycle fencing and process cutover are implemented.

- Обновлены закреплённые GitHub Actions до выпусков с поддержкой Node.js 24.
- Обновлены минимальная версия Go и builder image до 1.26.9 для получения актуальных исправлений безопасности стандартной библиотеки.
- Добавлены публичные contract/bootstrap, порты склада и Runtime с ProposalStore в памяти.
- Добавлены подготовка и подтверждение неизменяемых предложений, сериализация по proposalID, pending и атомарная замена предложения.
- Добавлены каталог, меню, стоп-лист и рекомендации по фактическим партиям; чтения каталога сохраняют evaluatedAt/stateVersion.
- Разделены списания заказа и распределения меню; записанные результаты списания просрочки сохраняют все факты.
- Изменены сигнатуры `GetDish` в serving contract/application/bootstrap: метод возвращает `DishDetails` с метаданными захвата и вложенным `Dish`. Go-клиентам нужно читать определение через поле `Dish`.
- Подтверждение связывает commandID глобально с proposalID и принятым составом. Завершённые исходы, включая EmptyOrder и определённые отказы, повторяются точно.
- Добавлен пассивный Resolve через отдельный attempt-reader port. Он возвращает запись склада или ConfirmationPending и не запускает Commit.
- Известный результат warehouse сохраняется типизированно до локального завершения. Замена предложения и снятие pending выполняются атомарно; точный повтор завершает тот же переход без второго commit.
- Dockerfile включает production-пакеты; добавлены закреплённые build/lint/architecture/race gates.
- Реализованы 11 публичных serving HTTP-операций и внутренний warehouse HTTP-клиент. Запросы проверяются строго; действуют ограничение тела, бюджеты запроса/warehouse и лимит ответа.
- Outcomes для `InvalidArgument` теперь отражают тип операции: `unavailable` для GET, `not_committed` для отклонённого POST и `unknown` для отклонённого Resolve.
- Warehouse `InventoryUnavailable/unknown` сохраняет неопределённый исход для catalog POST. POST `/order-proposals` сохраняет `InventoryUnavailable/unavailable` от чтения оценки; GET принимает только `unavailable`, а `not_committed` допускается только для отказа владельца на order commit.
- Quality checks reject unsupported nested modules and support library-only builds without source-tree executables. Pure-core clock restrictions also cover time.Since and time.Until.
- Health checks preserve response-cleanup failures; health-response encoding errors are handled explicitly.
- Added Go development, architecture and Git/release rules with pinned quality tooling.

## [0.1.0] - 2026-10-10

- Добавлены запускаемый каркас, Dockerfile и проверки GitHub Actions.
- Добавлена публикация образов из main по SHA коммита.
