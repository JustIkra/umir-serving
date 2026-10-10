# Изменения

## Не выпущено

- Quality checks reject unsupported nested modules and support library-only builds without source-tree executables. Pure-core clock restrictions also cover time.Since and time.Until.

- Health checks preserve response-cleanup failures; health-response encoding errors are handled explicitly.

- Added Go development, architecture and Git/release rules with pinned quality tooling.

## [0.1.0] - 2026-10-10

- Добавлены запускаемый каркас, Dockerfile и проверки GitHub Actions.
- Добавлена публикация образов из main по SHA коммита.

