# umir-serving

Scoped v2 recommendations use the [FIN1 contract supplement](https://github.com/JustIkra/umir-factory/blob/80c240065892153129b590f4bdbd2411b7c0319a/docs/completion-api-amendment.md). Serving reads one coherent menu/risk/demand capture from Warehouse through `consumer.recommendations.read`; it does not request stock management or arbitrary order history. Auth must issue the FIN1 recommendation token, with both Serving and Warehouse audiences. The Gateway and frontend must consume the composed FIN1 response profile.

FIN1 ranks available dishes with risky stock expiring in the next 72 hours. It uses exact rational `U×A×E` factors with a 12-hour urgency decay and whole recipe quantities, then returns the top ten by exact score, expiry and dish ID. Six-place display rounding does not determine order. Demand uses recorded portions over a complete 168-hour history window; incomplete history or a dish without orders uses two portions per hour. Each reason contains aggregate risk, recipe quantity and target portions. Targets explain the risk quantity; they do not guarantee production capacity. Reads do not reserve stock.

`Options.RecommendationsWarehouse` and `WarehousePorts.Recommendations` supply the narrow typed input port. The scoped registry always selects FIN1 and returns an unavailable error when that port is absent. The existing single-runtime constructor without the input port keeps the selected legacy v1 policy and wire shape. Legacy mode does not select a restaurant implicitly or fabricate FIN1 history. The existing v1 implementation and its original regression tests remain active for that public compatibility surface.

The FIN1 input decoder rejects malformed quantities, missing fields, mixed units, duplicate batches/demand, inconsistent capture identities and invalid period/coverage facts before recording an observation. Warehouse `503 LimitExceeded` on recommendation inputs maps to public `503 CalculationUnavailable`; other refusal mappings remain unchanged. FIN1 replies over 16MiB fail with `CalculationUnavailable`, without partial success. `make check` runs the complete owner race suite, legacy compatibility tests, signed scoped FIN1 composition and process tests. `S4_RESPONSE_DIR` exports current process responses to `fin1-wire-responses.json`; these are FIN1 responses, not predecessor C2 evidence.

Registration serializes factory creation through a cancellable gate. It does not hold the registry state mutex during external warehouse discovery. Auth can read runtime status during that discovery. Before publishing the runtime, registration rechecks the local tombstone and exact registration identity. A concurrent close prevents a late factory result from recreating its scope. Concurrent identical registrations still call the factory once.

An Auth registration with the same organization, restaurant, operation, epoch and
lifecycle version can refresh monotonically newer settings. A valid zone and
newer positive settingsVersion update only registry metadata. The existing runtime,
proposals and attempts remain in place, and warehouse discovery is not repeated.
An older settings version, a different zone at the same version, cancellation,
changed identity or a closed scope cannot apply that refresh.

The executable serves the private restaurant-scoped backend v2 API. The legacy single-runtime constructors remain available for the selected assembly tests.

Компоненты: OrdersService реализует подготовку и подтверждение предложений; MenuService предоставляет каталог, меню и стоп-лист; RecommendationsService ранжирует доступные блюда по фактическому распределению партий.

Меню и рецептуры, подготовка предложений заказа, подтверждение оставшихся блюд, стоп-лист и рекомендации по продаже. Serving владеет временными предложениями и обращается к warehouse для окончательной проверки и оформления.

Продукты, блюда и рецептуры хранятся в warehouse вместе с событиями. MenuService читает и регистрирует определения через порт склада.

Основной документ бизнес-контрактов находится в [docs фабрики](https://github.com/JustIkra/umir-factory/blob/main/docs/business-core.md). При расхождении описаний контрактов приоритет имеет docs. Структура Go-пакетов и границы зависимостей описаны в [архитектуре Go](https://github.com/JustIkra/umir-factory/blob/main/docs/go-architecture.md).

## Текущее состояние

Legacy library HTTP API обслуживает `GET /healthz`, каталог и меню (`/products`, `/dish-catalog`, `/dish`, `/dishes`, `/menu`, `/stop-list`, `/recommendations`), подготовку заказов (`/order-proposals`) и их подтверждение или разрешение (`/order-confirmations`, `/order-confirmations/resolve`). Контракты запросов и ответов описаны в [OpenAPI фабрики](https://github.com/JustIkra/umir-factory/blob/main/docs/api/openapi.yml).

Serving обращается к warehouse по внутреннему HTTP API. Процесс берёт адрес из `WAREHOUSE_INTERNAL_URL` (по умолчанию `http://warehouse:8082`), ограничивает каждый вызов склада значением `WAREHOUSE_TIMEOUT` (по умолчанию 5 секунд; процесс не принимает больше 5 секунд) и ответ 16 MiB. Он не повторяет изменяющие запросы автоматически. Публичный handler читает POST-тело не дольше общего бюджета запроса, по умолчанию 12 секунд, и принимает до 1 MiB. JSON проверяется на точные имена полей, повторные поля, `null`, неизвестные поля и второй JSON value. `GET /dish` требует ровно один параметр `dishID`; специальные символы в ID нужно percent-encode. `baseUnit` у ингредиента можно опустить; если он есть, допустимы только `g`, `ml` и `pcs`. Int64 передаётся десятичной строкой, а время использует `+03:00`. Ошибка `InvalidArgument` возвращает `unavailable` для GET, `not_committed` для отклонённого POST и `unknown` для отклонённого Resolve, если владелец ещё не установил терминальный исход. Warehouse `InventoryUnavailable/unknown` сохраняется для catalog POST; POST `/order-proposals` сохраняет `InventoryUnavailable/unavailable` от чтения оценки; GET принимает только `unavailable`, а `not_committed` допускается только для отказа владельца на order commit.

`OrdersService` сохраняет предложения и попытки в памяти одного процесса serving. В пределах одного Runtime commandID глобально связывается с proposalID и принятым составом. Завершённый ответ повторяется точно; неизвестный исход остаётся pending до установленного результата или безопасного повторного Confirm в том же epoch/scope после warehouse unseen. Resolve только читает warehouse и никогда не начинает оформление. Если proposal отсутствует и чтения склада не установили исход, Confirm и Resolve возвращают `ProposalNotFound` с `outcome=unknown`. Перезапуск процесса удаляет его локальные предложения и попытки.

`bootstrap.NewInMemory` собирает Runtime с пустым ProposalStore и переданными serving-портами склада. `bootstrap.NewHTTP` сохраняет legacy warehouse-клиент, Runtime и handler для явной совместимой сборки. Процесс использует `bootstrap.NewProcessHTTP` и private v2 registry. `Options` требует Now, NewID и все три складских порта. `MenuService` строит меню и стоп-лист из складских оценок. `RecommendationsService` ранжирует доступные блюда по срокам фактически используемых партий и возвращает причины с количеством на порцию.
`GetDish` возвращает `contract.DishDetails`: `EvaluatedAt`, `StateVersion` и вложенный `Dish`. MenuService сохраняет метаданные захвата склада и копирует рецепт.
Часовой пояс — `Europe/Moscow`; количество ингредиентов хранится целыми тысячными базовой единицы.

## Restaurant registry and trusted HTTP scope (S1)

`bootstrap.NewHTTP` retains the legacy routes and Warehouse transport as an explicit library composition. The executable uses `bootstrap.NewProcessHTTP`; it never attaches the unscoped handler to the backend v2 topology.

For the new topology, compose `bootstrap.NewRegistry` with an injected UUIDv4 Serving epoch, clock, ID generator and `WarehouseClientFactory`. Register each restaurant explicitly with its org, operation ID, expected epoch and lifecycle version. Registration builds one proposal/attempt store and separate Orders, Menu and Recommendations services. An identical registration keeps the runtime. A registration with a changed stable identity conflicts; only the settings refresh described above is allowed. Reads and writes never create a restaurant. Command and proposal identities are local to one restaurant runtime. A retained runtime requires its matching request scope and capability on every method.

Compose `bootstrap.NewJWTVerifier` with an injected clock and read-only signing-key source, then `bootstrap.NewScopedHTTP` with the registry, verifier, Gateway transport key, request budget and body limit (at most 1 MiB). Keep this handler on an unpublished private listener. Off the isolated test network, use TLS and validate certificates and hostnames. The key source resolves only trusted Auth Ed25519 public keys by kid; it must report key discovery failures and unknown keys. Serving has no signing key. `NewProcessHTTP` supplies bounded, authenticated Auth JWKS discovery.

The scoped handler accepts only `/internal/serving/v2/r/{restaurantID}/` products, dish-catalog, dish, menu, stop-list, recommendations, order-proposals, order-confirmations and passive GET order-confirmations/resolve. It checks the per-caller transport secret, Auth-only EdDSA signature, exact issuer/audiences, user/session/version claims, 60-second lifetime, operation, tenant and effective capability before runtime resolution. Forged browser identity headers and cookies fail before business work. Auth supplies the capability union for the selected restaurant; Serving does not derive rights from role headers. Health is separate and does not resolve a tenant. Every protected response is no-store.

The request context carries a copied `contract.RequestScope`, including the original Auth token for the scoped downstream client. A scoped Warehouse adapter must call `contract.RecordWarehouseContext` with the verified response org/tenant/epoch/settings, evaluatedAt and exact stateVersion. Each HTTP request has its own synchronized observation collector. Successful responses carry ServingContext and the original typed result. Missing or mismatched observed context yields RuntimeNotReady; a modifying request retains outcome=unknown. No time or version is invented. Proposal responses include the selected Warehouse epoch.

The registry, scoped Warehouse transport, lifecycle boundary and process composition implement the frozen backend v2 protocol. Unit transport doubles do not prove joint-service or public API acceptance. Factory J1/E checks remain required before selecting the candidate for an assembly.

## Scoped Warehouse HTTP ports (S2)

Use `bootstrap.NewScopedWarehouseHTTPPorts` inside the registry's Warehouse factory. Supply the selected restaurant ID, acknowledged Warehouse epoch, private URL, Serving-to-Warehouse transport key, request timeout and the same trusted Auth JWT verifier used by the scoped handler. The timeout must be positive and at most five seconds. The verifier must validate both Serving and Warehouse audiences. Each call revalidates the original token and matches its actor, tenant, operation, permissions, settings and lifecycle to the copied request scope. The adapter forwards the original token and the separate Serving transport key. It does not mint credentials.

The adapter supports scoped product/dish reads, menu evaluation, order evaluation, order commit and passive command/order/attempt reads. It uses the selected restaurant in the frozen private v2 route and supplies the saved Warehouse epoch for POST and passive resolution GET. Consumer permissions are paired with the original public operation. Catalog writes, delivery, stock and final writeoff cannot dispatch through this client. POST bodies are bounded to 1 MiB and responses to 16 MiB. Parent deadlines are preserved. Redirects and automatic modifying retries are disabled. Product/dish query IDs are encoded once.

Successful responses require matching org, tenant, Warehouse epoch and captured settings. Result time/version must agree with the envelope. Quantities remain exact int64 values and units remain g, ml or pcs. The v2 decoder accepts explicit RFC3339 offsets, including Z, without recomputing stored instants. The selected legacy decoder still requires +03:00. Menu allocations must be usable at the captured evaluation instant. Recommendations keep every reason at the actual minimum expiry, including ties, and never reserve stock. Evaluation and recheck responses must match the requested immutable positions.

Across one captured menu, each product has one base unit and each product/batch pair has one expiry instant. Contradictory metadata fails before recording Warehouse context. Different dishes may allocate different quantities from the same batch. Equivalent timestamp offsets identify the same instant. Serving preserves the HTTP status, code, outcome and retryability of every refusal accepted by the scoped Warehouse decoder, including input errors, conflicts, capacity limits and temporary unavailability.

Verified expiry-preparation headers pass through the private Serving response on success and calculation failure. They retain the committed version, Warehouse epoch and invalidation flag for the authenticated restaurant. A malformed modifying reply remains unknown; a GET protocol failure is unavailable. This metadata does not confirm final writeoff. Gateway owns removal of these private headers at the browser boundary.

Captured provenance and lifecycle fencing are wired into the executable. Unit/contract tests use synthetic Warehouse transport and real local Ed25519 validation; joint real-service acceptance remains J1/E work. Product and dish ID suggestions belong to Warehouse: `GET /internal/warehouse/v2/r/{restaurantID}/id-suggestions?kind=product|dish`, with catalog.write authorization. Serving has no suggestion or catalog-write route and grants no Warehouse access through menu.read.


## Epoch reset and lifecycle fencing (S3)

Configure a distinct Auth transport key and an explicit packaged tzdata version in `ScopedHTTPOptions.AuthKey` and `TZDataVersion`. The private `GET /internal/serving/v2/runtime` reports the injected process epoch, reconciliation readiness and tzdata version. Auth and Gateway transport credentials can inspect this status. Only Auth's transport key plus its signed `service:auth` token can use `PUT` or `DELETE /internal/serving/v2/r/{restaurantID}/registry`. The token must target only Serving and match the lifecycle operation, restaurant, generation and expected epoch. Registration requires the frozen settings object. Close forbids it. Replies identify the restaurant, Serving epoch and lifecycle generation explicitly.

Auth owns durable registration/deletion intents and startup retries. It installs current-epoch runtimes through registration. After receiving a complete bounded Auth registry, composition passes its validated projection to `Registry.Reconcile`. The projection carries the registry revision, entries and tombstones. Reconciliation checks that active generations are installed and deleting/deleted generations are closed. It does not create or remove runtimes from a missing or partial registry. Readiness remains false on validation failure. The HTTP status alone is not proof that Auth has acknowledged every service for a restaurant. Auth's current-epoch handshake governs business access.

Close serializes with registration and runtime admission. It retains an epoch-local deletion marker immediately, cancels admitted work and waits for completion before reporting closed. A deadline can leave cleanup pending. Retry the identical operation/epoch/generation to finish cleanup. A retained handle cannot admit another operation after the marker. Neighboring restaurant stores remain independent. Local markers last for this epoch; persistent Auth tombstones restore the cutoff after restart. Auth never reconstructs proposals, stock or unknown attempts.

The scoped HTTP resolver captures current Warehouse ports from the injected factory for each request. The factory must return tenant-bound ports for a currently acknowledged peer epoch within the caller's deadline. Proposals retain the actual observed Warehouse epoch and settings. Confirmations keep that epoch, immutable accepted positions and the settings used by their first attempt. A retained unknown scoped confirmation is first checked by passive lookup (the owner attempt, then the recorded command). Resolve never commits. A repeated Confirm may send the saved commit once, and only after the owner attempt is `unseen`, the local attempt is still pending and `/commands` has no record; it also needs the same Warehouse epoch and unchanged org, restaurant, time zone and settings version in the saved attempt, the owner attempt read and the request scope (see the confirmation boundaries below). An `unseen` result with any of these changed, or a Warehouse `pending` attempt, cannot authorize another commit. A changed command/proposal identity conflicts. Fresh commands receive current verified settings; owner refusals such as SettingsChanged remain explicit. Signed current credentials authorize resolution but do not replace the saved interpretation. The Warehouse echoes the settings of the presented token, so passive reads (`/order-attempts`, `/commands`, `/orders`) of a retained attempt are checked against the saved org, tenant and Warehouse epoch only, and a settings change does not hide a known outcome; the observation keeps the saved settings. A commit under a retained binding still requires the saved settings.

Peer discovery failure cannot establish the outcome of an earlier confirmation. Confirmation and passive resolution return `unknown` when discovery fails or returns invalid ports; an explicit peer reset remains `indeterminate`. This conservative result applies before the resolver can inspect local attempt identity. Fresh proposal admission keeps its pre-dispatch refusal semantics. Passive command, order and completed-attempt replies use the same order validation as scoped commits: accepted positions and allocations must be nonempty, and placedAt cannot exceed the verified envelope instant. Malformed passive replies leave the retained attempt unknown and cannot cache a confirmed order. A later valid passive reply can finish the same attempt without another commit.

Serving epoch mismatch is checked after scope validation and before peer discovery or ID lookup. A proposal/attempt whose saved Warehouse epoch differs from the currently captured peer epoch returns `RuntimeReset/indeterminate`, including a locally cached completed confirmation. An actual downstream reset retains that outcome through confirmation and passive resolution. Empty state after restart is not evidence of an old success or refusal. A new operation can capture a new peer epoch; an old operation cannot. Terminal replay and replacement proposals retain the original observed metadata and exact quantities. The legacy single-runtime API retains its compatibility behavior; scoped ConfirmationPending uses the frozen HTTP 200 result.

The frozen consumer order query takes `orderID`. The concrete client exposes `FindOrderByID` for that route. Scoped confirmation resolves by command and attempt, then relies on the owner's atomic proposal identity check. It never sends the legacy proposalID query to the v2 order route. `FindOrderByProposal` remains a legacy port and refuses scoped dispatch.

## Scoped process composition and migration

`cmd/server` creates a fresh UUIDv4 epoch and calls `bootstrap.NewProcessHTTP`. Supply all four distinct transport keys, a private Auth URL and the packaged timezone data version. Configuration errors stop startup. Serving loads bundled IANA data and never signs identity tokens. Health reports process liveness; authenticated runtime status reports reconciliation readiness separately.

The process starts one cancellable registry reader. Each attempt has a five-second upper bound. It validates the complete Auth registry, restaurant/service association, generations, tombstones and signing-key discovery before marking reconciliation ready. Failure clears readiness and retries with 1/2/4/8/16/30-second delays, capped by the configured polling interval. Successful reads repeat at that interval (default 30 seconds). Missing or partial documents never create an empty successful registry or delete a runtime. Auth alone sends signed registration/close requests and owns durable intents and acknowledgments. The process does not synthesize lifecycle operation IDs or install restaurants from a snapshot.

Each authenticated business request obtains the selected Warehouse epoch from a fresh bounded Auth registry read. The restaurant must have ready Warehouse and current Serving acknowledgments with the same generation. Registration can use a ready Warehouse acknowledgment while Serving is still registering. Failed discovery preserves unknown confirmation outcomes; no discovery or reconciliation retry dispatches a commit. Shutdown cancels and joins the registry reader before closing idle HTTP connections.

JWKS discovery accepts at most three unique OKP/Ed25519/EdDSA public keys. Known keys are copied from a cache refreshed every 30 seconds of the injected owner clock. JWT validity uses that same owner clock. Failed or stale-key discovery attempts are throttled for one second by host mechanism time, independently of owner time. The Auth client defaults to monotonic `time.Now` samples and supports a separate general retry-clock dependency. A backward mechanism sample resets the throttle interval; it does not permit an immediate retry. An unknown kid gets one refresh per cache interval; repeated misses fail closed. Unknown-key tracking is bounded. Failed refresh cannot substitute an expired cache for readiness. Discovery responses are strict JSON, bounded to 16 MiB, inherit caller deadlines and never follow redirects.

Migrate callers to `/internal/serving/v2/r/{restaurantID}/...` with the Gateway transport key, Auth-signed token and agreed operation/capabilities. Unsafe requests carry the Serving epoch header. Passive resolution is GET with commandID, proposalID and the saved Serving runtimeEpoch query parameter. Results include ServingContext, the saved Warehouse epoch and decimal-string quantities/versions. The executable rejects legacy unscoped routes. Existing selected images/pins and explicit legacy library tests remain independent.

Business proposals, attempts and local deletion markers exist only in memory. After restart, Auth must register active restaurants at the new epoch and reapply durable tombstones. Old confirmation identities remain RuntimeReset/indeterminate; empty state never proves rejection, success or permission to recommit. The process does not recover stock, proposals or unknown attempts. Real Auth/Warehouse/Gateway/Serving/Feedback assembly and public API acceptance remain separate Factory J1/E gates.

## Operational test fixture (SI supplement to S4)

The test-only [Serving fixture binding](tests/fixtures/protocol/README.md) defines protocol 1.1.0 server/control binaries and the separate tests/fixtures/Dockerfile. It composes the actual NewProcessHTTP, registry/JWKS/Warehouse clients, scoped handlers and memory runtimes with one injected clock. Its authenticated Unix controls do not appear on the production HTTP handler.

Supported controls are readiness, clock read/set and checkpoint. Other owner operations fail closed. Private readers use nonblocking no-follow opens and admit only bounded regular files with the expected owner and 0600 mode before reading. The helper validates read/set/checkpoint instants against the server grammar: years 2000–2099, explicit offsets (hours 00–23 and minutes 00–59), and optional dot fractions of one through nine digits. Credentials/config remain in private 0600 files, the socket is 0600 in a 0700 directory, and controls have finite request, concurrency and replay bounds. Process instance and business epoch are fresh on restart; real Auth reconciliation determines readiness. No fixture business state, signing or persistence replacement is supplied.

The independently approved operator binding correction requires exactly serving-auth, auth-serving, gateway-serving and serving-warehouse in the service credential map, plus fixture-serving as the separate control reference. The wrapper enforces that map and five distinct private credential values. Factory correction and actual Auth provisioning remain pending before J1. Fixture unit/composition checks and a missing-peer Docker smoke do not establish real-service acceptance. Accepted S4 review remains historical; this supplement requires fresh independent review.

### Isolated owner catalog amendment

The test wrapper also accepts a separate authenticated POST /owner-profile/v1 on its Unix socket. Send the strict CatalogRequest with action catalog-report, project, requestID, expectedInstanceID and expectedGeneration. The amendment is umir-owner-profiles/0.1.0-unpublished. Run /fixture-control profile --request /run/umir-fixtures/profile-request.json; the fixed request file must be private, owned by the fixture user and mode 0600. No profile selector or additional credential is accepted. RPC 1.1.0 capabilities and ordinary serve/healthcheck commands stay unchanged. Serving has no missing-data profile; Warehouse owns that refusal.

Build with docker build -f tests/fixtures/Dockerfile --target runtime. The build pins Go 1.26.9 by image digest, parses its actual time/tzdata AST string constant, verifies equality with its zoneinfo.zip, and checks the compiled fixture executable's build identity and time/tzdata symbol. The scratch image contains the immutable archive and provenance at /usr/local/lib/umir/zoneinfo.zip and catalog-provenance.json. Provenance binds toolchain image/source, executable digest, archive digest/size and go1.26.9-embedded. The read-only helper verifies those bytes, ZIP parsing and Europe/Moscow data before returning a report. A ZONEINFO override, wrong provenance, missing archive, unavailable real runtime, stale instance or generation fails closed. Reports use the existing owner clock and config digest. Qualification must additionally inspect the image filesystem and mounts to exclude host/system/GOROOT timezone sources. A report alone does not establish BE-E4-16 or J1/E acceptance.

The Feedback discovery amendment leaves Serving's four service references and separate fixture-serving credential unchanged. It grants Serving no feedback-auth credential or new Auth permission.

## Структура бизнес-ядра

- `internal/domain` содержит модель предложения и глубокое копирование её срезов.
- `internal/application` задаёт `ProposalStore` и `ConfirmationAttemptStore`, содержит `OrdersService`, `MenuService` и `RecommendationsService`.
- `internal/adapters/memory` содержит общий ProposalStore и глобальный индекс попыток с глубоким копированием и атомарным завершением/заменой.
- Публичный `contract` содержит значения входов/выходов и узкие порты склада, включая отдельный пассивный `OrderAttemptReader`. Serving владеет этими портами; production-код не импортирует `umir-warehouse`.
- `internal/adapters/warehousehttp` реализует складские порты; `internal/adapters/httpapi` кодирует публичный HTTP API; `internal/adapters/httpjson` проверяет точную форму JSON.
- Публичный `bootstrap.NewInMemory` собирает memory `ProposalStore` и application services в `Runtime`; `Options` принимает часы, генератор ID и собственные serving-порты склада. Runtime предоставляет методы задач заказов, каталога, меню, стоп-листа и рекомендаций.
- `cmd/server` собирает private v2 registry, Auth discovery и scoped handler через `bootstrap.NewProcessHTTP`. Build stage Dockerfile копирует `cmd`, `contract`, `bootstrap` и `internal`; тестовая поддержка в образ не копируется.

Тестовая сборка межсервисного взаимодействия находится в отдельном Go-модуле фабрики `tests/business`. Её bridge преобразует serving contract DTO в контракты warehouse и вызывает warehouse bootstrap Runtime. Она связывает оба сервиса в одном тестовом процессе без импорта чужих `internal` пакетов. Production serving не зависит от Go-модуля warehouse.

Тестовые фикстуры и `testsupport` держите вне production-пакетов. Не помещайте демо- или seed-данные в `internal/domain`, `internal/application`, публичные пакеты или адаптеры.

## Бизнес-правила

- Рецептура описывает одну порцию. Продукт встречается в ней один раз; количество положительное.
- GetMenu возвращает только доступные для одной порции блюда: ID и название, без количества порций. Полный GetDishCatalog нужен внутренним расчётам; GetProducts и GetDish предоставляют определения через порт warehouse.
- Позиции заказа обрабатываются в исходном порядке с общим временным остатком. Доступное число порций уменьшается до целого; при нуле позиция удаляется с объяснением дефицита.
- Подготовка не резервирует и не расходует ингредиенты заказа. Перед расчётом warehouse может записать наступившие списания просрочки. Предложение подтверждается действием «Оформить оставшиеся блюда».
- При подтверждении warehouse повторно проверяет принятый состав. При уменьшении доступности нужно новое предложение и подтверждение. Рост остатков не увеличивает согласованный заказ автоматически.
- Serving связывает commandID глобально с proposalID и принятым составом до вызова склада. Точный повтор возвращает исходный результат. Повтор commandID с другим proposal или составом получает CommandConflict.
- Перед новым оформлением warehouse восстанавливает очередь, записывает наступившие BatchExpired и применяет их к проекции. При сбое возвращается InventoryUnavailable; новый заказ и его расход не записываются. Меню и OrdersService собственной проверки дат не содержат.
- OrderConfirmed означает установленную запись OrderPlaced. Уже записанный заказ возвращается до подготовки очереди. Потеря ответа о записи остаётся технической неопределённостью до разрешения исходной попытки.
- Блюдо входит в стоп-лист, когда нельзя приготовить одну порцию. Независимая доступность нескольких блюд не гарантирует их совместную доступность в заказе.
- EvaluateMenu в warehouse независимо оценивает все блюда на одном неизменяемом состоянии с общей парой evaluatedAt/stateVersion. Serving использует оценки для меню, стоп-листа и рекомендаций; чистый складской расчёт может выполняться вне сериализации изменений.
- Рекомендации используют распределение FEFO для одной порции каждого доступного блюда и сортируются по ближайшему сроку реально расходуемой партии, затем dishID. Объяснение содержит продукт, партию, количество и срок.
- Расчёт рекомендаций не расходует ингредиенты. Системная подготовка склада перед запросом может записать просрочку; при её сбое запрос возвращает InventoryUnavailable. Генерация заказов и поставок от рекомендаций не зависит.

Serving хранит неизменное OrderProposal и типизированную запись попытки с результатом warehouse, завершённым ответом или определённым отказом. Известный результат сохраняется до локального завершения; замена и снятие pending выполняются атомарно. Гарантии действуют, пока живы процессы serving и warehouse. Постоянное восстановление после перезапуска и несколько экземпляров сервиса не входят в этот этап.

## Границы портов и подтверждения

- Каждый serving Runtime владеет ProposalStore с общим индексом commandID и сериализует Confirm/Resolve по proposalID. Один процесс serving — один владелец предложений; совместное использование несколькими экземплярами требует распределённой сериализации.
- До создания новой локальной привязки приложение проверяет записанную команду и заказ в warehouse. Авторитетная идентичность warehouse не может быть перезаписана локальным запросом.
- Serving атомарно связывает commandID и сохраняет pending до вызова склада. Пока исход вызова неизвестен, разрешён точный повтор с тем же составом; другая команда не подтверждает и не заменяет предложение. Ответ об неизвестном исходе не снимает pending.
- Resolve проверяет существующую serving-привязку и выполняет пассивный FindOrderAttempt. Он не вызывает Commit. Если у сохранённой попытки склад отвечает Pending или Unseen, перед ответом `ConfirmationPending` читается `/commands`: записанная команда с тем же отпечатком завершает попытку заказом, с другим отпечатком даёт `CommandConflict/not_committed`, ошибка чтения оставляет `unknown` и попытку без изменений, отсутствие записи оставляет `ConfirmationPending`. Unseen без локального pending, Pending без записанной команды и ошибки чтения не доказывают `not_committed`.
- Повторный scoped Confirm после достоверного warehouse `unseen` может один раз отправить сохранённый commit с исходными commandID, proposalID и accepted positions. Нужны локально pending-попытка с сохранёнными accepted positions, отсутствие записи в `/commands`, тот же наблюдаемый Warehouse epoch и неизменные org, restaurant, timezone и settingsVersion в сохранённой попытке, в чтении попытки склада и в области запроса. Повтор ограничен родительским deadline и десятью секундами, а каждый вызов склада — `WAREHOUSE_TIMEOUT`. Warehouse Pending, отсутствие provenance, смена org, restaurant, epoch или settings при `unseen` не разрешают новый commit. Resolve остаётся пассивным.
- Известный результат warehouse сохраняется в типизированной попытке до локального завершения. OrderCommitted сохраняет OrderConfirmed; RecheckRequired сохраняет одну замену; определённый отказ завершает попытку. Локальная ошибка после результата оставляет pending, чтобы точный повтор завершил тот же переход без второго commit.
- Сохраняйте новое предложение и ссылку `supersededBy` на прежнее атомарно. Возвращайте `RequiresConfirmation` только после успешного сохранения замены. При ошибке сохранения прежнее предложение остаётся активным.
- Порт склада возвращает согласованные оценки с `stateVersion` и `evaluatedAt`. Serving не проверяет сроки годности самостоятельно и не расходует остатки при подготовке меню, заказа или рекомендаций.

`Products`, `DishCatalog` и `DishDetails` содержат метаданные захвата из склада. Serving копирует recipe внутри `DishDetails` и сортирует Items каталога. `Allocation` заказа содержит productID, batchID и quantity. `MenuAllocation` дополнительно содержит название продукта, единицу и срок для объяснений рекомендации. Записанный `ExpiryResult` сохраняет продукт, причину, срок и время обработки.

ProposalStore и attempt index существуют только в памяти одного Runtime. После перезапуска serving теряет предложения и локальные попытки. Warehouse lookup может вернуть уже записанный заказ, но отсутствие предложения не доказывает, что запись склада не выполнялась.

## Готовность ядра в памяти

Приёмка должна подтвердить следующие группы сценариев:

- **Порядок меню и заказа:** блюда доступны независимо на одном снимке, а заказ обрабатывает позиции в исходном порядке с общим временным остатком. Меню показывает только доступные блюда без количества порций; стоп-лист объясняет дефицит на одну порцию.
- **Сроки во время подготовки:** warehouse обрабатывает срок, наступивший до или во время подготовки, до оценки остатков. При ошибке обязательного списания serving возвращает техническую ошибку и не сохраняет предложение или заказ.
- **Изоляция значений:** изменение данных после возврата меню, оценки или предложения не меняет сохранённое состояние; последующая команда использует новую версию склада.
- **Неизвестный исход подтверждения:** serving сохраняет исходную команду и pending до определённого ответа. Confirm повторяет только тот же вход; Resolve читает попытку warehouse и не вызывает commit. Уже записанный заказ возвращается без второго расхода; конфликт версии приводит к новой оценке склада.
- **Замена предложения:** снижение остатков создаёт новое предложение на подтверждение; рост остатков не увеличивает принятый состав. Сбой атомарного сохранения замены оставляет старое предложение активным.
- **Причины рекомендаций:** порядок следует фактическому FEFO-распределению одной порции; объяснение указывает продукт, партию, количество и срок. Одинаковый ближайший срок разрешается по `dishID`, а сам расчёт не расходует ингредиенты.

Событийный склад и проверка полных границ команд, включая snapshot после всей команды, описаны в [основном контракте](https://github.com/JustIkra/umir-factory/blob/main/docs/business-core.md). В in-memory интеграционной приёмке также проверьте неизвестный исход записи, точный повтор, копирование снимка чтения и запрет на публикацию частично применённой команды.

## Разработка

Требуется Go 1.26.9. Из каталога `umir-serving` установите закреплённые инструменты и выполните полный gate:

```sh
make tools
GOWORK=off make check
GOWORK=off make quality-test
GOWORK=off go mod verify
GOWORK=off go vet ./...
```

Configure the required private endpoints and transport credentials below through the test topology. Then run `GOWORK=off go run ./cmd/server` on that private network. Keep the listener unpublished. `GET /healthz` is liveness only; use authenticated `/internal/serving/v2/runtime` to observe reconciliation. Business requests require Auth installation and current peer readiness.

`make check` проверяет форматирование, сборку, закреплённые golangci-lint/go-arch-lint и полные race-тесты с `GOWORK=off`. Архитектурная проверка исключает тесты `internal/application`, потому что они напрямую собирают memory adapters; production-код application остаётся ограничен domain и contract. Версии инструментов находятся в `.go-quality-versions`, правила — в `.golangci.yml` и `.go-arch-lint.yml`. Для форматирования отдельных файлов используйте `make fmt FILES='path/to/file.go'`; несколько путей передавайте через пробел. Межсервисные сценарии запускаются через `umir-checks business --local` из фабрики. Численный RPS порог этот этап не задаёт.

## Совместимость и версии

Публичная поверхность — экспортируемые значения и порты `contract`, конструкторы и методы `bootstrap`, публичные HTTP-маршруты, состояния подтверждения и причины ошибок. Сигнатуры `CatalogWarehouse.GetDish`, `MenuService.GetDish` и `Runtime.GetDish` возвращают `contract.DishDetails`; Go-клиенты получают определение блюда в поле `Dish`. Подтверждение использует tagged result с одним payload для своего состояния.

Используется SemVer. До 1.0 совместимые исправления получают patch, новые возможности или несовместимые изменения — minor с описанием миграции. После 1.0 несовместимые изменения требуют major. Изменения записываются в CHANGELOG под «Не выпущено» до выпуска. Теги корневого модуля имеют вид `vX.Y.Z`; уже выпущенные теги не изменяются. Образы и сборка фабрики сохраняют точные SHA.

The local Compose file builds only Serving. It exposes port 8080 to its private network and publishes no host port. Configure the required variables and attach it to the isolated Auth/Warehouse test topology. Standalone Serving without those peers cannot pass business readiness. Validate configuration with `docker compose config --quiet`; container build and real-service acceptance remain Factory gates.

| Setting | Default / requirement |
| --- | --- |
| `PORT` | `8080` on the private listener |
| `AUTH_INTERNAL_URL` | Required private Auth origin |
| `WAREHOUSE_INTERNAL_URL` | `http://warehouse:8082` |
| `SERVING_AUTH_KEY` or `SERVING_AUTH_KEY_FILE` | Required Serving → Auth transport key |
| `AUTH_RUNTIME_KEY` or `AUTH_RUNTIME_KEY_FILE` | Required Auth → Serving transport key |
| `GATEWAY_SERVING_KEY` or `GATEWAY_SERVING_KEY_FILE` | Required Gateway → Serving transport key |
| `SERVING_WAREHOUSE_KEY` or `SERVING_WAREHOUSE_KEY_FILE` | Required Serving → Warehouse transport key |
| `TZDATA_VERSION` | Required packaged version, matching all peer containers |
| `AUTH_TIMEOUT` | `5s`; positive, at most five seconds |
| `WAREHOUSE_TIMEOUT` | `5s`; positive, at most five seconds |
| `REQUEST_TIMEOUT` | `12s` |
| `REGISTRY_POLL_INTERVAL` | `30s`; positive, at most 30 seconds |
| `MAX_REQUEST_BYTES` | `1048576`; positive, at most 1 MiB |
| `SERVING_INSTANCE_ID` | Optional replica name (letters, digits, `.`, `_`, `-`, up to 64 characters); a random UUID of the process if unset. Written as `instance_id` in logs |

Each of the four transport keys is given either directly (`SERVING_AUTH_KEY`, legacy profile) or as the path of a file (`SERVING_AUTH_KEY_FILE`, secret-file profile); the same applies to the other three. The file holds the key; one trailing line break (`\n` or `\r\n`) is removed, spaces are kept, and the file may be up to 64 KiB. Setting both forms of one key, an empty or unreadable file, or a file over the limit stops the start; the error names the variable and never contains the value. Do not reuse one key across caller pairs. URLs must be private trusted endpoints; Auth takes an origin without a path. Use certificate/hostname-validated TLS outside the isolated test network. No secret value belongs in source or logs. Programmatic `ProcessHTTPOptions` exposes the same configuration plus injected epoch, clock, ID source and optional HTTP client.

Общая сборка и команды запуска описаны в [README фабрики](https://github.com/JustIkra/umir-factory#readme).

## Structured logs

The process writes one JSON object per line to stdout (`bootstrap.NewLogger`). Every record has `time` (`+03:00`), `level`, `msg` (equal to `event`), `event`, `log_schema_version` (1), `service` (`umir-serving`), `version` (VCS revision or `unknown`) and `instance_id`, as in the factory observability format. Records carry identifiers, codes and versions only: no tokens, `Authorization`, request or response bodies, error texts, user IDs or network addresses. Client-supplied IDs are cut at 256 characters. The `net/http` server error log is reduced to an `http.server_error` record with a `kind` (`panic`, `tls_handshake`, `accept`, `other`); its text, peer address and stack are dropped. Every logger is an injected `*slog.Logger` (`Logger` options of `ProcessHTTPOptions`, `HTTPOptions`, `RegistryOptions`, `ScopedHTTPOptions`, `ScopedOptions`, `OrdersService.WithLogger`); the default discards.

| Event | Level | Fields besides the common ones |
| --- | --- | --- |
| `confirmation.unknown` | `ERROR` (`WARN` for `ProposalNotFound` and for a context that ended before this call sent anything) | `operation` (`confirm`/`resolve`), `command_id`, `proposal_id`, `tenant_id`, `org_id`, `runtime_epoch`, `warehouse_epoch`, `settings_version`, `error_code`, `outcome` (`unknown`/`indeterminate`), `cause` (`deadline`/`canceled`, optional) |
| `confirmation.pending.started` | `INFO` | as above; the call reserved the attempt and ended without an outcome |
| `confirmation.pending.resolved` | `INFO` | as above plus `result` (result kind or error code); a retained pending attempt reached an outcome |
| `warehouse.protocol_error` | `ERROR` | `tenant_id`, `operation`, `warehouse_epoch`, `error_code` |
| `reconcile.failed` | `ERROR`; `WARN` for `registering_not_installed` and for a failed Auth or key fetch | `reason`, `error_code`, `revision`, and `tenant_id`, `org_id`, `lifecycle_version` when one entry is at fault |
| `registry.tenant_mismatch` | `WARN`; the same mismatch is written at most once a minute | `where` (`reconcile`/`resolve`), `reason` (`org_mismatch`/`lifecycle_mismatch`), `tenant_id`, `registered_org_id`, `presented_org_id`, `registered_lifecycle_version`, `presented_lifecycle_version` |
| `http.response_failed` | `WARN`; `ERROR` for encoding | `stage`, `error_kind` |
| `process.started`, `process.stopped`, `process.start_failed`, `process.failed` | `INFO`/`ERROR` | `port`, `error` (startup errors only) |
| `http.server_error` | `ERROR` | `kind` |

`reconcile.failed` reasons: `registering_not_installed` (Auth lists a registering restaurant not yet installed here; skipped, logged once per entry, readiness unaffected), `snapshot_invalid`, `revision_invalid`, `revision_rollback`, `invalid_entry`, `ready_not_installed`, `registering_closed_locally`, `tombstone_not_applied`, `tenant_mismatch`, `local_missing_from_snapshot`, `context_done` (deadline only; a canceled context at shutdown is silent), and, from the poll loop, `auth_registry_unavailable` and `signing_keys_unavailable`.

## Совместная работа

Main защищён активным ruleset. Обновлять main и объединять PR может только `JustIkra`.
`Kxqusos` добавлен с ролью write и работает в других ветках.
Остальные пользователи GitHub могут создать issue, fork и PR.
Владелец кода указан в `.github/CODEOWNERS`.

Перед PR выполните локальные проверки. Изменение межсервисного контракта согласуйте с вызывающей стороной.

## CI/CD

[GitHub Actions](https://github.com/JustIkra/umir-serving/actions) проверяет push в ветки, PR в main и ручные запуски.
После успешной проверки main публикует `ghcr.io/justikra/umir-serving:sha-<полный SHA коммита>` для `linux/amd64` и `linux/arm64`.
Публикация использует `GITHUB_TOKEN`, с `packages:write` только у задания публикации.
Коммит документации запускает тот же workflow.

[Фабрика](https://github.com/JustIkra/umir-factory) закрепляет выбранный SHA в `compose.yaml`.
Обновление фабрики выполняется отдельным PR; новый образ не развёртывается автоматически.
Секреты передаются через окружение; `.env` исключён из Git.

## Development standard and releases

Repository instructions are in [AGENTS.md](AGENTS.md). Public compatibility covers documented requests, results, error states and operational interfaces; warehouse/serving also publish their declared Go contract surfaces when implemented. Internal refactoring does not automatically change this contract.

Go tools are pinned to golangci-lint 2.14.0 and go-arch-lint 1.19.0 in [.go-quality-versions](.go-quality-versions). Local and CI checks use the same commands:

```sh
make tools
make fmt FILES="cmd/server/main.go"
make check
make quality-test
```

`make check` verifies the module inventory, formatting without writes, build, lint, source dependency architecture and fresh race tests. The shared runner checks one root module (`.`). Declared nested modules fail explicitly and need separate effective quality gates. Library-only modules are supported; executable output stays in temporary storage. Tests and tool caches remain outside production dependencies. `make fmt-all` is reserved for a deliberate full formatting change. Service checks use `GOWORK=off`; unknown packages or modules fail. Empty future components need no placeholder packages. [Linter rules](.golangci.yml) constrain errors, imports and clock access; [architecture rules](.go-arch-lint.yml) cover the module root, including public contract/bootstrap packages.

Use [Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/) and [Semantic Versioning](https://semver.org/spec/v2.0.0.html). Stable incompatible changes increment major, compatible additions minor and fixes patch. Our pre-1.0 convention uses patch for compatible fixes and minor for additions or breaking changes. Mark breaking behavior and migration explicitly.

Release repositories independently. Use annotated `vX.Y.Z` tags for actual releases; published nested Go modules prefix tags with the logical module subdirectory (excluding a trailing major-version directory), and Go v2+ needs the matching major import path. Published tags/versions stay immutable. Record changes under Unreleased before releasing. Preserve Umir `sha-<full-SHA>` image identities and exact factory component pins; SemVer does not replace them with `latest`. API versions and service releases are separate.

Preserve unrelated local edits, stage reviewed paths and check `git diff --cached` and `git diff --check`. Tagging, publishing and shared-history rewrites require the corresponding user instruction. This standard does not create a release or change the current version.


## Durable storage (wave 2, in progress)

Done on this branch: the copied reference adapters (`internal/adapters/postgres`, `internal/adapters/rabbitmq`), the Serving schema and PostgreSQL store (`internal/adapters/pgstore`), the shared store contract suite (`tests/storecontract`), `make integration` (needs `UMIR_TEST_DATABASE_URL` and `UMIR_TEST_AMQP_URL` from `umir-factory scripts/durable-test-infra.sh env`). Not wired yet: the running service still keeps state in memory, uses a per-process epoch and polls the Auth registry. The sections of this README that describe in-memory state stay true until the remaining wave 2 tasks land.
