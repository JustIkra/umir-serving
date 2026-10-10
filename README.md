# umir-serving

Сервис обслуживания одного ресторана.

Планируемые компоненты: MenuService, OrdersService, StopListService и RecommendationsService.

Меню и рецептуры, подготовка предложений заказа, подтверждение оставшихся блюд, стоп-лист и рекомендации по продаже. Serving владеет временными предложениями и обращается к warehouse для окончательной проверки и оформления.

## Текущее состояние

Работает HTTP-каркас с `GET /healthz` и команда проверки контейнера `/server healthcheck`.
Бизнес-команды и их приёмочные тесты ещё не реализованы.
Часовой пояс — `Europe/Moscow`; количество ингредиентов в планируемом ядре хранится целыми тысячными базовой единицы.

## Планируемые бизнес-правила

- Рецептура описывает одну порцию. Продукт встречается в ней один раз; количество положительное.
- Позиции заказа обрабатываются в исходном порядке с общим временным остатком. Доступное число порций уменьшается до целого; при нуле позиция удаляется с объяснением дефицита.
- Подготовка не резервирует и не расходует склад. Предложение подтверждается действием «Оформить оставшиеся блюда».
- При подтверждении warehouse повторно проверяет принятый состав. При уменьшении доступности нужно новое предложение и подтверждение. Рост остатков не увеличивает согласованный заказ автоматически.
- Блюдо входит в стоп-лист, когда нельзя приготовить одну порцию. Независимая доступность нескольких блюд не гарантирует их совместную доступность в заказе.
- Рекомендации используют распределение FEFO для одной порции каждого доступного блюда и сортируются по ближайшему сроку реально расходуемой партии, затем dishID. Объяснение содержит продукт, партию, количество и срок.
- Рекомендации не расходуют склад. Генерация заказов и поставок от них не зависит.

## Разработка

Требуется Go 1.26.5.

```sh
go test -race ./...
go vet ./...
TZ=Europe/Moscow PORT=18082 go run ./cmd/server
```

При запуске через Go healthz доступен на `http://127.0.0.1:18082/healthz`.

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
