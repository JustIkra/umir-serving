# umir-serving

Сервис обслуживания одного ресторана.

Компоненты: OrdersService реализует подготовку и подтверждение предложений; MenuService предоставляет каталог, меню и стоп-лист; RecommendationsService ранжирует доступные блюда по фактическому распределению партий.

Меню и рецептуры, подготовка предложений заказа, подтверждение оставшихся блюд, стоп-лист и рекомендации по продаже. Serving владеет временными предложениями и обращается к warehouse для окончательной проверки и оформления.

Продукты, блюда и рецептуры хранятся в warehouse вместе с событиями. MenuService читает и регистрирует определения через порт склада.

Основной документ бизнес-контрактов находится в [docs фабрики](https://github.com/JustIkra/umir-factory/blob/main/docs/business-core.md). При расхождении описаний контрактов приоритет имеет docs. Структура Go-пакетов и границы зависимостей описаны в [архитектуре Go](https://github.com/JustIkra/umir-factory/blob/main/docs/go-architecture.md).

## Текущее состояние

Работает HTTP-каркас с `GET /healthz` и команда проверки контейнера `/server healthcheck`.
`internal/application.OrdersService` подготавливает неизменяемые предложения без резервирования, повторно проверяет их при подтверждении и сериализует попытки по `proposalID`. Он разрешает повтор команды по её типу и отпечатку, сохраняет незавершённый `pendingCommandID`, возвращает уже оформленный заказ и атомарно заменяет уменьшенное предложение. Для замены и подтверждения используются результаты порта warehouse; расчёты FEFO и сроки годности остаются ответственностью склада. `MenuService` передаёт каталожные операции через собственный serving contract, строит меню и стоп-лист из складских оценок. `RecommendationsService` ранжирует доступные блюда по срокам фактически используемых партий и возвращает причины с количеством на порцию.
Публичные порты принадлежат serving. `bootstrap.NewInMemory` собирает Runtime с собственным пустым ProposalStore и переданными портами склада. `Options` требует ненулевые Now, NewID и все три порта, включая отсутствие typed nil. Конструктор не вызывает часы или генератор ID и не запускает goroutine. Бизнес-операции доступны через Runtime. `cmd/server` предоставляет healthz; подключение Runtime к процессу serving требует следующего этапа транспортного адаптера.
`GetDish` возвращает `contract.DishDetails`: `EvaluatedAt`, `StateVersion` и вложенный `Dish`. MenuService сохраняет метаданные захвата склада и копирует рецепт.
Часовой пояс — `Europe/Moscow`; количество ингредиентов хранится целыми тысячными базовой единицы.

## Структура бизнес-ядра

- `internal/domain` содержит модель предложения и глубокое копирование её срезов.
- `internal/application` задаёт `ProposalStore` и содержит `OrdersService`, `MenuService` и `RecommendationsService`.
- `internal/adapters/memory` содержит `ProposalStore` с копированием данных, проверкой pending identity и атомарной заменой.
- Публичный `contract` содержит значения входов/выходов и небольшие порты `OrdersWarehouse`, `CatalogWarehouse` и `MenuWarehouse`. Serving владеет этими портами; production-код не импортирует `umir-warehouse`.
- Публичный `bootstrap.NewInMemory` собирает memory `ProposalStore` и application services в `Runtime`; `Options` принимает часы, генератор ID и собственные serving-порты склада. Runtime предоставляет методы задач заказов, каталога, меню, стоп-листа и рекомендаций.
- `cmd/server` остаётся точкой запуска HTTP-каркаса. Build stage Dockerfile копирует `cmd`, `contract`, `bootstrap` и `internal`; тестовая поддержка в образ не копируется.

Тестовая сборка межсервисного взаимодействия находится в отдельном Go-модуле фабрики `tests/business`. Её bridge преобразует serving contract DTO в контракты warehouse и вызывает warehouse bootstrap Runtime. Она связывает оба сервиса в одном тестовом процессе без импорта чужих `internal` пакетов. Production serving не зависит от Go-модуля warehouse.

Тестовые фикстуры и `testsupport` держите вне production-пакетов. Не помещайте демо- или seed-данные в `internal/domain`, `internal/application`, публичные пакеты или адаптеры.

## Бизнес-правила

- Рецептура описывает одну порцию. Продукт встречается в ней один раз; количество положительное.
- GetMenu возвращает только доступные для одной порции блюда: ID и название, без количества порций. Полный GetDishCatalog нужен внутренним расчётам; GetProducts и GetDish предоставляют определения через порт warehouse.
- Позиции заказа обрабатываются в исходном порядке с общим временным остатком. Доступное число порций уменьшается до целого; при нуле позиция удаляется с объяснением дефицита.
- Подготовка не резервирует и не расходует ингредиенты заказа. Перед расчётом warehouse может записать наступившие списания просрочки. Предложение подтверждается действием «Оформить оставшиеся блюда».
- При подтверждении warehouse повторно проверяет принятый состав. При уменьшении доступности нужно новое предложение и подтверждение. Рост остатков не увеличивает согласованный заказ автоматически.
- Подтверждение и замена одного proposalID выполняются последовательно, включая ответ склада. Warehouse фиксирует не более одного заказа на предложение. При неизвестном результате вызова serving повторяет тот же запрос и до разрешения попытки не создаёт замену.
- Перед новым оформлением warehouse восстанавливает очередь, записывает наступившие BatchExpired и применяет их к проекции. При сбое возвращается InventoryUnavailable; новый заказ и его расход не записываются. Меню и OrdersService собственной проверки дат не содержат.
- OrderConfirmed означает установленную запись OrderPlaced. Уже записанный заказ возвращается до подготовки очереди. Потеря ответа о записи остаётся технической неопределённостью до разрешения исходной попытки.
- Блюдо входит в стоп-лист, когда нельзя приготовить одну порцию. Независимая доступность нескольких блюд не гарантирует их совместную доступность в заказе.
- EvaluateMenu в warehouse независимо оценивает все блюда на одном неизменяемом состоянии с общей парой evaluatedAt/stateVersion. Serving использует оценки для меню, стоп-листа и рекомендаций; чистый складской расчёт может выполняться вне сериализации изменений.
- Рекомендации используют распределение FEFO для одной порции каждого доступного блюда и сортируются по ближайшему сроку реально расходуемой партии, затем dishID. Объяснение содержит продукт, партию, количество и срок.
- Расчёт рекомендаций не расходует ингредиенты. Системная подготовка склада перед запросом может записать просрочку; при её сбое запрос возвращает InventoryUnavailable. Генерация заказов и поставок от рекомендаций не зависит.

Serving хранит неизменное OrderProposal с метаданными незавершённого подтверждения и замены. Позиции и объяснения дефицитов входят в предложение; факт оформления хранит warehouse. Часы и генератор ID передаются функциями. Отдельные интерфейсы для каждого Service не требуются. Эти уточнения относятся к проекту тестового каркаса с одним экземпляром serving; восстановление незавершённого подтверждения после перезапуска при сетевом вызове нужно определить перед реализацией API.

## Границы портов и подтверждения

- Каждый serving Runtime владеет своим `ProposalStore` и сериализует полный `ConfirmOrder` по `proposalID`. Один процесс serving — один владелец предложений; совместное использование предложений несколькими экземплярами требует отдельной распределённой сериализации.
- Сначала проверяйте записанный результат команды и заказа через `OrdersWarehouse`. Если результат уже есть, верните его до чтения `ProposalStore` и подготовки актуальных остатков.
- Перед вызовом склада сохраняйте `pendingCommandID`. Пока исход вызова неизвестен, разрешён точный повтор с тем же составом; другая команда не подтверждает и не заменяет предложение. Ответ об неизвестном исходе не снимает pending.
- Доказанные отказы до записи, включая `ErrInvalidArgument`, `ErrInventoryUnavailable` и `ErrCommandIdentityConflict`, запускают очистку pending-маркера. Если очистка не сработала, повтор исходной попытки может выполнить её ещё раз; неизвестный исход pending сохраняет. Если склад уже вернул проверенный `OrderCommitted`, serving возвращает `OrderConfirmed`, даже если очистка маркера не удалась; следующий lookup записанного заказа повторяет её.
- Сохраняйте новое предложение и ссылку `supersededBy` на прежнее атомарно. Возвращайте `RequiresConfirmation` только после успешного сохранения замены. При ошибке сохранения прежнее предложение остаётся активным.
- Порт склада возвращает согласованные оценки с `stateVersion` и `evaluatedAt`. Serving не проверяет сроки годности самостоятельно и не расходует остатки при подготовке меню, заказа или рекомендаций.

`Products`, `DishCatalog` и `DishDetails` содержат метаданные захвата из склада. Serving копирует recipe внутри `DishDetails` и сортирует Items каталога. `Allocation` заказа содержит productID, batchID и quantity. `MenuAllocation` дополнительно содержит название продукта, единицу и срок для объяснений рекомендации. Записанный `ExpiryResult` сохраняет продукт, причину, срок и время обработки.

ProposalStore существует только в памяти одного Runtime. После перезапуска неподтверждённые предложения и pending-метаданные теряются. Сетевое восстановление pending и защита от поздней записи требуют отдельного контракта и приёмки перед подключением HTTP.

## Готовность ядра в памяти

Приёмка должна подтвердить следующие группы сценариев:

- **Порядок меню и заказа:** блюда доступны независимо на одном снимке, а заказ обрабатывает позиции в исходном порядке с общим временным остатком. Меню показывает только доступные блюда без количества порций; стоп-лист объясняет дефицит на одну порцию.
- **Сроки во время подготовки:** warehouse обрабатывает срок, наступивший до или во время подготовки, до оценки остатков. При ошибке обязательного списания serving возвращает техническую ошибку и не сохраняет предложение или заказ.
- **Изоляция значений:** изменение данных после возврата меню, оценки или предложения не меняет сохранённое состояние; последующая команда использует новую версию склада.
- **Неизвестный исход подтверждения:** serving повторяет ту же команду и сохраняет pending до определённого ответа. Уже записанный заказ возвращается без второго расхода; конфликт версии приводит к новой оценке склада.
- **Замена предложения:** снижение остатков создаёт новое предложение на подтверждение; рост остатков не увеличивает принятый состав. Сбой атомарного сохранения замены оставляет старое предложение активным.
- **Причины рекомендаций:** порядок следует фактическому FEFO-распределению одной порции; объяснение указывает продукт, партию, количество и срок. Одинаковый ближайший срок разрешается по `dishID`, а сам расчёт не расходует ингредиенты.

Событийный склад и проверка полных границ команд, включая snapshot после всей команды, описаны в [основном контракте](https://github.com/JustIkra/umir-factory/blob/main/docs/business-core.md). В in-memory интеграционной приёмке также проверьте неизвестный исход записи, точный повтор, копирование снимка чтения и запрет на публикацию частично применённой команды.

## Разработка

Требуется Go 1.26.5. Из каталога `umir-serving` установите закреплённые инструменты и выполните полный gate:

```sh
make tools
make check
GOWORK=off go vet ./...
TZ=Europe/Moscow PORT=18082 go run ./cmd/server
```

При запуске через Go healthz доступен на `http://127.0.0.1:18082/healthz`.

`make check` проверяет форматирование, сборку, закреплённые golangci-lint/go-arch-lint и полные race-тесты с `GOWORK=off`. Версии инструментов находятся в `.go-quality-versions`, правила — в `.golangci.yml` и `.go-arch-lint.yml`. Для форматирования отдельных файлов используйте `make fmt FILES='path/to/file.go'`; список нескольких путей разделяется новой строкой. Межсервисные сценарии запускаются через `umir-checks business --local` из фабрики. Численный RPS порог этот этап не задаёт.

## Совместимость и версии

Публичная поверхность — экспортируемые значения и порты `contract`, конструктор и методы `bootstrap`, состояния подтверждения и причины ошибок. Сигнатуры `CatalogWarehouse.GetDish`, `MenuService.GetDish` и `Runtime.GetDish` теперь возвращают `contract.DishDetails`; существующим Go-клиентам нужно переносить доступ к определению в поле `Dish`. HTTP-каркас поддерживает `GET /healthz`; бизнес-маршруты ещё не определены.

Используется SemVer. До 1.0 совместимые исправления получают patch, новые возможности или несовместимые изменения — minor с описанием миграции. После 1.0 несовместимые изменения требуют major. Изменения записываются в CHANGELOG под «Не выпущено» до выпуска. Теги корневого модуля имеют вид `vX.Y.Z`; уже выпущенные теги не изменяются. Образы и сборка фабрики сохраняют точные SHA.

Сборка отдельного контейнера:

```sh
docker compose up --build -d --wait
```

Локальный Compose публикует тот же порт 18082. В общей сборке фабрики сервис доступен только внутри сети на `serving:8080`.

| Настройка | Значение в контейнере |
| --- | --- |
| `PORT` | `8080` |
| `TZ` | `Europe/Moscow` |

Общая сборка и команды запуска описаны в [README фабрики](https://github.com/JustIkra/umir-factory#readme).

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
