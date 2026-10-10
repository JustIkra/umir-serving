# Изменения

## Не выпущено

- Добавлены публичные contract/bootstrap, порты склада и Runtime с ProposalStore в памяти.
- Добавлены подготовка и подтверждение неизменяемых предложений, сериализация по proposalID, pending и атомарная замена предложения.
- Добавлены каталог, меню, стоп-лист и рекомендации по фактическим партиям; чтения каталога сохраняют evaluatedAt/stateVersion.
- Разделены списания заказа и распределения меню; записанные результаты списания просрочки сохраняют все факты.
- Изменены сигнатуры `GetDish` в serving contract/application/bootstrap: метод возвращает `DishDetails` с метаданными захвата и вложенным `Dish`. Go-клиентам нужно читать определение через поле `Dish`.
- Доказанный отказ склада, включая InvalidArgument и command identity conflict, запускает очистку pending; неизвестный исход сохраняет pending. Проверенный `OrderCommitted` остаётся `OrderConfirmed`, даже если очистка маркера временно не удалась.
- Dockerfile включает production-пакеты; добавлены закреплённые build/lint/architecture/race gates.
- HTTP-каркас сохраняет healthz. Производственное подключение портов и восстановление pending после перезапуска относятся к транспортному этапу.
- Quality checks reject unsupported nested modules and support library-only builds without source-tree executables. Pure-core clock restrictions also cover time.Since and time.Until.
- Health checks preserve response-cleanup failures; health-response encoding errors are handled explicitly.
- Added Go development, architecture and Git/release rules with pinned quality tooling.

## [0.1.0] - 2026-10-10

- Добавлены запускаемый каркас, Dockerfile и проверки GitHub Actions.
- Добавлена публикация образов из main по SHA коммита.
