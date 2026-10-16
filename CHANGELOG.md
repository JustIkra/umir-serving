# Изменения

## Не выпущено

- Refactor: the store ports gain a fenced per-proposal lease (`AcquireLease`, `ReleaseLease`, token in the context via `application.WithLease`), check bookkeeping for the reconciler (`RecordCheck`, `MarkUnresolved`) and a limit of active proposals; the pure position functions move to `internal/domain`; store errors are shared in `internal/domain`. One behavior suite, `tests/storecontract`, covers the in-process store (and the PostgreSQL store next); the older store tests are folded into it. A replacement proposal remembered on an attempt is written once and never overwritten.

- Feat: durable storage foundation. The reference PostgreSQL and RabbitMQ adapters of `umir-factory reference/durable` are copied unchanged to `internal/adapters/postgres` and `internal/adapters/rabbitmq` (R15, copy made with `copy.sh`), the Serving schema `0001_init.sql` (restaurant registrations, proposals, confirmation attempts, the first storage generation, row level security on the three restaurant tables) lives in `internal/adapters/pgstore`, and `make integration` runs the integration suite on real PostgreSQL and RabbitMQ.

- Fix: an invalid `SERVING_INSTANCE_ID` no longer fails the `healthcheck` subcommand; the server still refuses to start with it.

- Feat: service keys can be read from files. `SERVING_AUTH_KEY`, `AUTH_RUNTIME_KEY`, `GATEWAY_SERVING_KEY` and `SERVING_WAREHOUSE_KEY` each accept a `<NAME>_FILE` variable with the path of a file holding the key, for the secret-file deployment profile; the plain variables keep working. One trailing line break is removed from the file value, nothing else. Setting both forms of one key, an empty or unreadable file, or a file over 64 KiB stops the start with an error that names the variable and never contains the value (checked with canary values in errors and logs).

- Feat: structured JSON logs for unknown outcomes and reconciliation. The process writes one JSON line per event to stdout with `event`, `log_schema_version=1`, `service=umir-serving`, `version` and `instance_id` (`SERVING_INSTANCE_ID`, optional, otherwise a random UUID of the process) in the factory observability format. New events: `confirmation.unknown`, `confirmation.pending.started` and `.resolved`, `warehouse.protocol_error`, `reconcile.failed` (with a reason, including the skipped `registering` entry that is not installed yet, logged once per entry) and `registry.tenant_mismatch`. Records carry identifiers, codes and versions only: no tokens, `Authorization`, bodies, error texts or addresses (checked with canary values). The `log.Printf` and `log.Fatal` calls are replaced by `*slog.Logger`, injected through `Logger` options with a discarding default, so existing callers are unchanged. The `net/http` server error log is reduced to an `http.server_error` record with a kind (`panic`, `tls_handshake`, `accept`, `other`), without its text, peer address or stack. Registry records are written after its mutex is released, repeated `registry.tenant_mismatch` records are limited to one per minute for the same mismatch, and a retained attempt is reported as resolved only when it really reached an outcome.

- Fix: the default Warehouse HTTP client now keeps up to 64 idle connections for the Warehouse host. The transport set `MaxIdleConns: 64` but left `MaxIdleConnsPerHost` at the Go default of 2, so after a burst of parallel requests all but two connections were closed and the next burst paid for new TCP (and TLS) handshakes. Two bursts of 32 parallel requests now open 32 connections in total instead of about 62. The process shares this client with the Auth client, so the Auth host gets the same per-host limit; no other transport is created in production code.

- Fix: token verification no longer waits for the Auth key refresh. Every check used to pass through one gate that stayed locked during the JWKS request (up to 5 seconds), so a slow Auth stalled all requests at key expiry. Cached keys are now read under a read lock, and one shared background refresh starts when the cache is 20 seconds old, while the keys are still valid for another 10 seconds. Callers that need a refresh (cache older than 30 seconds, or an unknown `kid`) join the running one and still honor their own deadline. Fail-closed after 30 seconds and the throttling of unknown `kid` values and fetch attempts are unchanged. The refresh goroutine is owned by the client, bound to its lifetime and 5-second attempt limit, and the process stops it on shutdown (`authhttp.Client.Close`).

- Fix: creating one restaurant no longer closes access for the whole cell. Reconciliation with the Auth registry used to fail with `RuntimeNotReady` and drop process readiness when Auth listed a restaurant as `registering` that was not installed locally yet, so every restaurant was refused until the next poll (1 to 30 seconds). Such an entry is now skipped: process readiness stays `true`, and only that restaurant answers `RuntimeNotReady` until its registration arrives. Readiness is still `false` before the first complete reconciliation, and after an incomplete or invalid snapshot, a revision rollback, an unavailable Auth, a `ready` entry that is not installed locally, a `registering` entry that does not match the local registration or whose restaurant is closed locally, or a local restaurant missing from the snapshot.

- Docs: correct the README on the Warehouse client timeout and the unseen retry rule. The timeout bounds each Warehouse call by `WAREHOUSE_TIMEOUT` (default 5 seconds, at most 5 seconds in the process), not 10 seconds. A repeated Confirm may resend the saved commit once after an `unseen` owner attempt with no recorded command and an unchanged epoch, org, restaurant, time zone and settings version; the README no longer says `unseen` can never authorize another commit. No code change.

- Fix: a settings change no longer hides a known confirmation outcome. The scoped Warehouse client checked the org, tenant, epoch and the saved time zone and settings version of a retained attempt against the response envelope, but the Warehouse fills the envelope with the settings of the presented token, so after a change `ResolveConfirmation` and a repeated `ConfirmOrder` failed with a protocol error (502, `unknown`) although the Warehouse attempt was `Completed`. Passive reads of a retained attempt (`/order-attempts`, `/commands`, `/orders`) now compare only org, tenant and epoch (plus their own result checks) and report the settings saved with the attempt; `/order-commits` under a retained binding stays strict, and the single saved-command redelivery still requires unchanged settings, so a changed scope answers `ConfirmationPending` without a second commit.

- Fix: a retained confirmation attempt whose Warehouse attempt is `Pending` or `Unseen` now reads `/commands` before answering `ConfirmationPending`, in both `ResolveConfirmation` and the scoped `ConfirmOrder` retry. If the owner recorded the order but lost the reply, the attempt completes with `OrderConfirmed` instead of staying pending forever. A record with another command fingerprint completes the attempt as `CommandConflict` with `outcome=not_committed`; a failed lookup is `unknown` and leaves the attempt unchanged; no record keeps `ConfirmationPending`. The lookup happens before the single saved-command redelivery, so an order the owner already recorded is never committed again, and neither path sends a commit when a record exists. Redelivery still requires provenance from the owner attempt read itself. A live Warehouse attempt without a recorded command remains pending.

- Fix: waiting for a proposal lock now honors the request context. A `ConfirmOrder` or `ResolveConfirmation` call queued behind a stuck confirmation of the same proposal returns its context error as soon as its own deadline or cancellation fires, before any store or Warehouse access, instead of holding a goroutine for the whole queue and blocking registry shutdown. The error is returned as is (an unknown outcome for writes, never `not_committed`). Serialization of confirmations per proposal and cleanup of lock entries are unchanged.

- Internal: remove the unused `MarkPending`, `ResolvePending` and `Replace` methods from the `ProposalStore` port and the in-memory adapter. Production code already reserved, completed and replaced proposals only through `BeginAttempt`, `ReserveAttempt` and `CompleteAttempt`; the contract and orders tests now use the same methods, so the atomic replacement coverage is unchanged. No behavior change.

- Fix: Serving now reports only codes from the public v2 `Error.code` enum for its own errors. A command identity conflict (same command ID with another proposal or accepted positions) is `CommandConflict` instead of `CommandIdentityConflict`, and an unclassified failure or an `OperationError` without a code is `TechnicalError` (HTTP 500) instead of `InternalError`; the Gateway turned the old codes into 502/`unknown`. The HTTP status (409) and `outcome=not_committed` are unchanged. Stored rejected attempts that still carry `CommandIdentityConflict` are read and served as `CommandConflict`, and the old code is no longer written, including when a non-scoped Warehouse reports a rejected attempt with it. A Warehouse response about an ambiguous write is not renamed. Clients that matched the old codes must match the new ones.

- Refuse legacy optional envelope fields on the closed FIN1 recommendation-input context before recording its observation. Preserve existing compatibility decoding for unrelated requests.

- Add the scoped FIN1 recommendation policy and strict coherent Warehouse input port. Rank with exact urgency, risky portion share and consumption factors, using explicit history/fallback demand and finite explanatory values. Keep selected legacy v1 behavior and its tests separate. FIN1 consumers must adopt the composed response profile and narrow recommendation permission together.

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
