# Изменения

## Не выпущено

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
