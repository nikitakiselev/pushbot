# pushbot — дизайн

Дата: 2026-10-14
Статус: согласован, ожидает ревью спеки

## 1. Назначение

`pushbot` — маленький демон для простого CI/CD в homelab и на личных серверах.
GitHub Actions собирает Docker-образ, пушит его в registry и последним шагом
вызывает HTTP-эндпоинт демона на сервере. Демон раскладывает файлы проекта
в его директорию, пишет `.env` и выполняет шаги деплоя, описанные в
`pushbot.yml` в репозитории проекта.

### Цели

- Без SSH-доступа к серверу из CI.
- Минимум шагов на подключение нового сервиса: на сервере — ничего,
  в репозитории — `pushbot.yml` и один шаг в workflow.
- Прозрачность: лог деплоя виден прямо в логе GitHub Actions, job падает,
  если деплой не удался; action — короткий читаемый bash-скрипт.
- Никаких секретов для аутентификации запросов: GitHub OIDC.
- Один статический бинарник, минимум зависимостей.

### Не цели (v1)

Откат, веб-интерфейс, пользователи, уведомления, GitHub App, деплой на
несколько серверов из одного workflow, сборка на сервере / `git clone`,
ограничение по веткам в `allow`, удаление файлов, исчезнувших из `files`.

## 2. Общая схема

```
GitHub Actions job                      GitHub OIDC                pushbot (сервер)
  build + push image → GHCR                  │                           │
  │ 1. запрос OIDC-токена (aud=pushbot)      │                           │
  │─────────────────────────────────────────>│                           │
  │ 2. JWT                                   │                           │
  │<─────────────────────────────────────────│                           │
  │ 3. POST /v1/deploy: токен + manifest + bundle + env (через Caddy/TLS) │
  │──────────────────────────────────────────────────────────────────────>│
  │                                          │ 4. JWKS (кеш)             │
  │                                          │<──────────────────────────│
  │                                  5. проверки → файлы → .env → шаги   │
  │ 6. поток лога, последняя строка — итог                               │
  │<──────────────────────────────────────────────────────────────────────│
```

Компоненты (оба в репозитории `nikitakiselev/pushbot`):

1. **Демон `pushbot`** — Go-бинарник, `pushbot serve`. Работает под systemd
   от пользователя `pushbot` (в группе `docker`), слушает `127.0.0.1:9800`.
   Наружу с TLS его отдаёт Caddy.
2. **GitHub Action** — `action.yml` в корне репозитория, composite action на
   bash (`yq`, `jq`, `tar`, `curl`). Подключается как
   `uses: nikitakiselev/pushbot@v1`.

## 3. Формат `pushbot.yml`

Файл лежит в репозитории проекта (по умолчанию в корне).

```yaml
name: my-service          # необязательно; по умолчанию — имя репозитория в нижнем регистре

files:                    # необязательно; что положить в директорию проекта
  - docker-compose.yml
  - config/               # директория целиком
  - deploy/compose.prod.yml: docker-compose.yml   # с переименованием

steps:                    # необязательно; по умолчанию — см. ниже
  - run: docker compose pull
  - run: docker compose up -d --remove-orphans
  - run: docker compose exec -T app php artisan cache:clear
```

Минимальный рабочий вариант: `files: [docker-compose.yml]`.

### Поля

- **`name`** — строка, `^[a-z0-9][a-z0-9._-]{0,62}$`. Директория проекта —
  `<projects_root>/<name>`. По умолчанию — часть claim'а `repository` после
  `/`, приведённая к нижнему регистру; если она не проходит проверку
  (например, `.github`), — ошибка 400 с подсказкой задать `name` явно.
- **`files`** — список. Элемент — либо строка `path`, либо map из одной пары
  `src: dest`. Пути относительные (относительно корня репозитория для `src` и
  директории проекта для `dest`), без `..`, не абсолютные. Если `src` —
  директория, её содержимое рекурсивно копируется в `dest` (по умолчанию
  `dest = src`). Если `src` — файл, `dest` — итоговый путь файла (а не
  директория, в которую его положить). Завершающий `/` необязателен.
  `dest`, равный `.env`, — ошибка.
- **`steps`** — список шагов. Шаг — map с единственным ключом `run`
  (строка, допускается многострочная). Неизвестные ключи — ошибка. Если
  `steps` отсутствует — используются шаги по умолчанию:
  `docker compose pull`, `docker compose up -d --remove-orphans`.
  Явно пустой список `steps: []` — ошибка валидации.
- Неизвестные ключи верхнего уровня — ошибка.

## 4. Конфиг демона

`/etc/pushbot/config.yml`:

```yaml
listen: 127.0.0.1:9800
projects_root: /srv/apps
allow:
  - nikitakiselev/*
```

Необязательные ключи и значения по умолчанию:

| Ключ | По умолчанию | Смысл |
|---|---|---|
| `state_dir` | `/var/lib/pushbot` | служебные данные и логи |
| `audience` | `pushbot` | ожидаемый `aud` в OIDC-токене |
| `deploy_timeout` | `15m` | таймаут одного деплоя (включая ожидание блокировки) |
| `max_bundle_size` | `50MB` | лимит сжатого архива |
| `max_unpacked_size` | `200MB` | лимит распакованного объёма |
| `log_retention` | `20` | сколько логов хранить на проект |
| `oidc_issuer` | `https://token.actions.githubusercontent.com` | только для тестов |

### `allow`

Элемент — `owner/repo-glob`. `owner` — литерал (без масок), `repo-glob` —
маска в синтаксисе Go `path.Match` (`*`, `?`, `[...]`).

При старте демон получает числовые ID владельцев через
`GET https://api.github.com/users/<owner>` (работает и для организаций),
с повторами до ~1 минуты. Успешный результат сохраняется в
`<state_dir>/owners.json`. Если GitHub недоступен — используется кеш;
если кеша нет — демон завершается с ошибкой (systemd перезапустит).

Запрос разрешён, если `repository_owner_id` из токена совпадает с ID
владельца правила **и** имя репозитория (часть `repository` после `/`)
подходит под `repo-glob`.

## 5. GitHub Action

Пример workflow проекта:

```yaml
on:
  push:
    branches: [main]

permissions:
  contents: read
  packages: write     # пуш образа в GHCR (включает read для pull на сервере)
  id-token: write     # OIDC-токен для pushbot

jobs:
  deploy:
    runs-on: ubuntu-latest
    environment: production
    steps:
      - uses: actions/checkout@v4
      - uses: docker/login-action@v3
        with: { registry: ghcr.io, username: "${{ github.actor }}", password: "${{ github.token }}" }
      - uses: docker/build-push-action@v6
        with: { push: true, tags: "ghcr.io/${{ github.repository }}:${{ github.sha }}" }

      - uses: nikitakiselev/pushbot@v1
        with:
          url: https://deploy.example.com
          env: |
            IMAGE_TAG=${{ github.sha }}
            DB_PASSWORD=${{ secrets.DB_PASSWORD }}
```

### Входы

| Вход | Обязателен | По умолчанию | Смысл |
|---|---|---|---|
| `url` | да | — | базовый URL демона |
| `env` | нет | пусто | строки `KEY=VALUE` для `.env` |
| `config` | нет | `pushbot.yml` | путь к манифесту относительно корня репо |
| `registry-auth` | нет | `true` | передать `github.actor` и `github.token` для pull из GHCR |
| `audience` | нет | `pushbot` | audience для OIDC-токена |

### Поведение

1. Проверить наличие `yq`, `jq`, `curl`; проверить, что
   `ACTIONS_ID_TOKEN_REQUEST_URL` задан — иначе понятная ошибка
   «добавьте `permissions: id-token: write`».
2. Прочитать `src` из `files` манифеста через `yq` (строка — сама строка,
   map — её ключ). Упаковать их в `bundle.tgz` (`tar -czf`), пути
   относительно корня репозитория. Отсутствующий файл — ошибка до запроса.
   Если `files` пуст — архив не отправляется.
3. Получить OIDC-токен с `audience`.
4. `POST <url>/v1/deploy` через `curl -N` с multipart: `manifest`
   (содержимое файла), `bundle`, `env`, при `registry-auth` —
   `registry_user` и `registry_token`. Ответ транслируется в лог job'а
   и сохраняется во временный файл.
5. HTTP-код не 200 — вывести тело и завершиться с кодом 1.
   Иначе код 0 только если последняя строка начинается с `PUSHBOT: success`.

## 6. API демона

### `POST /v1/deploy`

- Заголовок: `Authorization: Bearer <OIDC JWT>`.
- Тело: `multipart/form-data`:
  - `manifest` — обязательно, содержимое `pushbot.yml`, до 64 КБ;
  - `bundle` — необязательно, tar.gz с файлами из `files`;
  - `env` — необязательно, до 64 КБ;
  - `registry_user`, `registry_token` — необязательно, оба или ни одного.

Ошибки до начала деплоя — HTTP-код и текстовое тело:

| Код | Когда |
|---|---|
| 400 | битый multipart/архив, ошибка в манифесте или `env`, недопустимые пути |
| 401 | токен невалиден: подпись, `iss`, `aud`, `exp`/`nbf`, повтор `jti` |
| 403 | репозиторий не в `allow` или имя занято другим репозиторием |
| 413 | превышен лимит размера |
| 503 | демон останавливается |

Если проверки прошли — `200`, `Content-Type: text/plain; charset=utf-8`,
потоковый ответ (chunked). Служебные строки демона начинаются с `==> `.
Последняя строка — итог: `PUSHBOT: success (<длительность>)` или
`PUSHBOT: failed: <причина>`.

```
==> nikitakiselev/my-service@a1b2c3d (run #9876543210) → /srv/apps/my-service
==> files: docker-compose.yml, config/ (3 files)
==> .env: IMAGE_TAG, DB_PASSWORD
==> [1/2] docker compose pull
 ...
==> [2/2] docker compose up -d --remove-orphans
 ...
PUSHBOT: success (14.2s)
```

Если шаг молчит 30 секунд, демон пишет `==> still running [i/n] (<время>)`,
чтобы промежуточные прокси не закрыли соединение по простою.

### `GET /healthz`

`200 ok`.

## 7. Конвейер деплоя

1. **Аутентификация** (раздел 8). Ошибка → 401/403.
2. **Разбор запроса**: лимиты размеров, валидация манифеста (раздел 3),
   разбор `env` (ниже). Ошибка → 400/413.
3. **Распаковка** `bundle` во временную директорию в `<state_dir>/tmp`:
   разрешены только обычные файлы и директории (symlink'и, hardlink'и,
   устройства — ошибка), пути после очистки не выходят за корень, лимиты
   на распакованный объём. Каждый `src` из `files` должен присутствовать.
   Ошибка → 400.
4. **Владение именем**: `<state_dir>/projects.json` хранит
   `name → {repository_id, repository, claimed_at}`. Имя свободно — его
   занимает текущий репозиторий (если директория уже существует, в лог
   пишется предупреждение). Занято другим `repository_id` → 403. Файл
   пишется атомарно (temp + rename). Освободить имя — вручную отредактировать
   файл (описано в README).
5. С этого момента — ответ `200` и поток лога.
6. **Блокировка проекта**: один деплой на `name` одновременно. Ожидающий
   запрос пишет `==> waiting for previous deploy...`. Ожидание входит в
   `deploy_timeout`. Разные проекты деплоятся параллельно.
7. **Директория проекта** создаётся при отсутствии (`0750`, владелец `pushbot`).
8. **Файлы**: копирование из временной директории в директорию проекта
   согласно `files`, с перезаписью. Ничего не удаляется.
9. **`.env`**: всегда перезаписывается целиком (при пустом `env` — пустой
   файл), права `0600`.
10. **Шаги**: по очереди, каждый — `/bin/sh -e -c '<run>'` в директории
    проекта, в своей группе процессов. Ненулевой код → остановка, итог
    `failed: step i/n exited with code X`.
11. **Уборка**: удалить временную директорию и временный `DOCKER_CONFIG`,
    записать строку итога в journald, ротировать логи проекта.

### Формат `env`

Строки `KEY=VALUE`. Пустые строки и строки, начинающиеся с `#`, игнорируются.
`KEY` — `^[A-Za-z_][A-Za-z0-9_]*$`. `VALUE` — всё после первого `=` как есть.
Значения, содержащие `'` или перевод строки, в v1 отклоняются (400).
В `.env` значения пишутся в одинарных кавычках (`KEY='value'`), чтобы
docker compose не интерполировал `$` внутри них.

### Окружение шагов

Окружение демона не наследуется. Шаги получают только:

- `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`,
  `HOME=<state_dir>`;
- переменные из `env` (исходные значения);
- метаданные из claim'ов токена: `PUSHBOT_PROJECT`, `PUSHBOT_REPOSITORY`,
  `PUSHBOT_SHA`, `PUSHBOT_REF`, `PUSHBOT_ACTOR`, `PUSHBOT_RUN_ID`,
  `PUSHBOT_RUN_URL` (`https://github.com/<repository>/actions/runs/<run_id>`);
- `DOCKER_CONFIG=<временная директория>`, если переданы `registry_*`:
  в ней `config.json` с `auths` для `ghcr.io`. Без `registry_*` переменная
  не задаётся и docker использует `~/.docker` пользователя `pushbot`.

## 8. Аутентификация (GitHub OIDC)

Проверка через `github.com/coreos/go-oidc/v3`:

1. Подпись по JWKS issuer'а (discovery + кеш, перезагрузка ключей при
   незнакомом `kid` — делает библиотека).
2. `iss == oidc_issuer`, `aud` содержит `audience`.
3. `exp`, `nbf` с допуском 60 секунд.
4. `jti` ещё не встречался: in-memory кеш до `exp` токена. При рестарте
   демона кеш теряется — допустимо, токены живут минуты.
5. Правило `allow` (раздел 4) по `repository_owner_id` и `repository`.

Используемые claim'ы: `repository`, `repository_id`, `repository_owner`,
`repository_owner_id`, `ref`, `sha`, `actor`, `run_id`, `jti`.

Привязка имени проекта идёт по `repository_id` (неизменяемому), а не по имени.

## 9. Модель безопасности

- Возможность пушить в разрешённый репозиторий = выполнение произвольных
  команд на сервере от пользователя `pushbot`. Группа `docker` фактически
  эквивалентна root. Это осознанный компромисс для личного использования;
  основная защита — OIDC и `allow`.
- Секретов для аутентификации нет ни в GitHub, ни на сервере.
- `registry_token` — временный `github.token`, живёт до конца job'а,
  на диск попадает только во временный `DOCKER_CONFIG` на время деплоя.
- Демон слушает только `127.0.0.1`; TLS обеспечивает Caddy.
- Значения из `env` (длиной от 4 символов) маскируются как `***` в логах
  на сервере и в journald. В поток к Actions они уходят как есть — секреты
  там маскирует GitHub.
- PR из форков OIDC-токен по умолчанию не получают.

## 10. Нештатные ситуации

- **Клиент отключился** (job отменён): деплой доводится до конца, вывод
  пишется только в лог на сервере.
- **Таймаут**: SIGTERM группе процессов текущего шага, через 10 секунд
  SIGKILL; итог `failed: timeout`.
- **Остановка демона** (SIGTERM): новые запросы получают 503, демон ждёт
  завершения текущих деплоев и выходит. В unit'е: `KillMode=mixed`,
  `TimeoutStopSec=16min`.
- Контейнеры, запущенные `docker compose up -d`, живут под `dockerd` и
  не затрагиваются остановкой демона.

## 11. Логи и состояние

```
<state_dir>/
  owners.json                        # кеш ID владельцев из allow
  projects.json                      # привязка name → repository_id
  logs/<name>/<YYYYMMDD-HHMMSS>-<sha7>.log
  tmp/                               # распаковка архивов, DOCKER_CONFIG
```

- Лог деплоя — тот же поток, что уходит клиенту, с маскировкой `env`.
  Хранятся последние `log_retention` на проект.
- journald: одна строка на деплой (проект, репозиторий, sha, итог,
  длительность) плюс ошибки демона.

## 12. Установка и релизы

- GoReleaser: бинарники `linux/amd64`, `linux/arm64` в GitHub Releases по
  тегу `v1.x.y`; плавающий тег `v1` для action.
- `install.sh` (~30 строк, читаемый): скачать бинарник в
  `/usr/local/bin`, создать системного пользователя `pushbot` (home —
  `/var/lib/pushbot`, группа `docker`), директории `/srv/apps` и
  `/var/lib/pushbot`, заготовку `/etc/pushbot/config.yml` (не перезаписывая
  существующую), unit `/etc/systemd/system/pushbot.service`,
  `systemctl enable --now pushbot`, вывести пример блока Caddy.
- README: те же шаги вручную, блок Caddy:

  ```
  deploy.example.com {
      reverse_proxy 127.0.0.1:9800 {
          flush_interval -1
      }
  }
  ```

## 13. Структура кода

```
cmd/pushbot/          # main: serve, version
internal/config/      # конфиг демона
internal/manifest/    # разбор и валидация pushbot.yml
internal/auth/        # OIDC, allowlist, получение ID владельцев
internal/bundle/      # безопасная распаковка tar.gz
internal/state/       # projects.json, owners.json
internal/runner/      # выполнение шагов: группа процессов, таймаут, окружение, поток
internal/deploy/      # оркестрация конвейера
internal/server/      # HTTP-обработчики, потоковая отдача
action.yml
install.sh
packaging/pushbot.service
.goreleaser.yml
```

Зависимости: `gopkg.in/yaml.v3`, `github.com/coreos/go-oidc/v3`.
Остальное — стандартная библиотека.

## 14. Тестирование

Docker в тестах не нужен: шаги — обычный shell (`echo`, `touch`, `exit 1`).

- **Unit (табличные)**: валидация манифеста (имена, `..`, абсолютные пути,
  `.env` в `files`, неизвестные ключи, `steps: []`); распаковка злонамеренных
  архивов (`../`, symlink'и, hardlink'и, превышение объёма); `allow`
  (маски, ID владельца); владение именем; разбор и запись `env`
  (кавычки, отклонение `'` и переводов строк, права `0600`); маскировка;
  runner (порядок, остановка на ошибке, окружение, таймаут убивает группу
  процессов, сообщения `still running`).
- **OIDC**: своя пара ключей и фейковый issuer на `httptest` (discovery +
  JWKS), `oidc_issuer` указывает на него. Случаи: неверные `aud`, `iss`,
  `exp`, чужая подпись, повтор `jti`, неразрешённый владелец.
- **Интеграционный**: полный `POST /v1/deploy` через `httptest` с тестовым
  токеном — проверка файлов во временной `projects_root`, `.env`, потока
  логов и итоговой строки; параллельные деплои одного проекта сериализуются.
- **E2E в CI репозитория pushbot** (только push и PR из того же репо):
  собрать бинарник, запустить `pushbot serve` на раннере с `allow` на
  текущий репозиторий и временной `projects_root`, вызвать `uses: ./` с
  `url: http://127.0.0.1:9800` и тестовым манифестом, проверить результат.
  Проверяет весь путь с настоящими токенами GitHub.

## 15. Метрики (добавлено после v1.0.0)

Ключ `metrics_listen` (по умолчанию пусто — выключено) поднимает отдельный HTTP-сервер
с `GET /metrics` в формате Prometheus (`prometheus/client_golang`). Отдельный адрес —
чтобы метрики не публиковались через reverse proxy вместе с API деплоя.

Метрики: `pushbot_deploys_total{project,result}` (`success`/`failed`/`timeout`),
`pushbot_deploy_duration_seconds{project}`, `pushbot_deploy_in_progress{project}`,
`pushbot_last_deploy_success{project}`, `pushbot_last_deploy_timestamp_seconds{project}`,
`pushbot_requests_rejected_total{code}`, `pushbot_build_info{version}`, плюс `go_*` и
`process_*`. Сервер метрик останавливается после основного, чтобы метрики были
доступны, пока демон ждёт завершения деплоев.

## 16. Принятые решения

| Вопрос | Решение | Почему |
|---|---|---|
| Где собирается образ | в GitHub Actions, push в registry | сервер не собирает и не хранит исходники |
| Как файлы попадают на сервер | архив из Actions в запросе | не нужен доступ к git и deploy-ключи |
| `.env` | из GitHub Secrets через вход `env` | ноль ручных действий на сервере |
| Аутентификация | GitHub OIDC + `allow` | нет хранимых секретов; GitHub App избыточен: нам не нужны ни доступ сервера к GitHub, ни вебхуки, а секреты App всё равно получить не может |
| TLS | Caddy, демон на `127.0.0.1` | Caddy уже стоит |
| Язык | Go | один статический бинарник |
| Формат шагов | список `run` + отдельный `files` | явно, файлы перечисляются один раз |
| Клиент | composite action на bash | клиенту нужен только список `files`, `yq` есть на раннерах |
| Удаление файлов | никогда | в директории проекта лежат данные контейнеров |
