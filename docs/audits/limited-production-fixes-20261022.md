# Исправления после проверки перед ограниченной эксплуатацией

Дата: 2026-10-22. База: `31e0095` (v2.0.4), результат — выпуск
[v2.1.0](../releases/v2.1.0.md). Находки и их воспроизведения — в
[отчёте проверки](limited-production-readiness-20261022.md); здесь — что
изменено, чем подтверждено и что осталось.

## Оценка

Все находки F1–F22 закрыты в коде, поставке или документации. Каждое
исправление поведения закреплено тестом, который падает при откате
исправления (проверено временной мутацией кода). Отступления от
предложенного в проверке и оставленные границы перечислены ниже.

С условиями из раздела «Что остаётся» v2.1.0 готов к ограниченной
эксплуатации на целевой нагрузке.

## Что изменено

| # | Было | Стало | Подтверждение |
| --- | --- | --- | --- |
| F1 | CopyObject обнулял объект с 200 | CopyObject и UploadPartCopy реализованы. Источник читается через read fence, сверяется с записанной контрольной суммой и стейджится рядом с адресатом; публикуется коротким commit, как PUT. Копия на себя ничего не меняет | `copy_test.go`, `copy_source_test.go` (41+16 мутаций). E2E через aws-sdk-go-v2: multipart-копия 20 MiB тремя диапазонами, SHA-256 совпал. Commit lock на 64 MiB: 638 → 44 мс |
| F2 | Пустые и половинчатые учётные данные открывали шлюзы; compose терял секрет | Отказ в старте: шлюз без пары и без `allow_anonymous`, половина пары, `changeme*`; peers без `node_id` или без `cluster_secret`. Шлюзы и S3 тоже закрыты при половине пары. Compose требует все переменные (`:?`) и передаёт секрет и peers | `TestGatewayCredentialsFailClosed` (4 шлюза × 7 случаев), `TestPeersRequireIdentityAndSecret`, `TestAuth_HalfConfiguredKeyPairAdmitsNobody`, `TestCheckBasicAuthHalfConfiguredAdmitsNobody`, `docker compose config` без `.env` → отказ |
| F3 | Нечитаемый файл держал ноду unready навсегда, каталог давал fatal | `EACCES`/`EPERM` — пропуск имени (`unreadable`), никогда не удаление; нечитаемый корень остаётся отказом | `internal/watcher/unreadable_test.go` (sweep, событие, рестарт, `r` без `x`) |
| F4 | Пропавшее вложенное поддерево удалялось на всём кластере | Другая ФС ниже `sync_dir` — явная ошибка sweep, без прохода удалений. Удаления, выведенные из отсутствия, ограничены `sync.max_external_deletions` (1000 за 10 минут): большая группа удерживается целиком, нода unready. Окно бюджета переживает рестарт. Точки монтирования не удаляются | `external_deletions_test.go` (9 тестов). Bind mount Docker Desktop: один `st_dev` на всех уровнях, нода ready |
| F5 | Peer-трафик открытым текстом | `tls_cert_file`/`tls_key_file` для peer API, `peer_ca_file`; предупреждение о `http://` peers вне частных сетей | Две ноды по HTTPS с приватным CA: репликация, `birak_peer_healthy 1`, plain HTTP → 400 |
| F6 | Одинаковый `node_id` молча делил кластер | С peers `node_id` обязателен. Дубликат — ERROR, `duplicate_node_id` и метрика. Свой URL в списке peers распознаётся и пропускается. Односторонние списки peers видны как `unconfigured_pollers` (после того как ответили свои peers) | `peer_identity_test.go` |
| F7 | Удаление дерева держало commit lock целиком | Пакеты по 8–512 файлов с целью ~100 мс на захват; новое содержимое и файлы под writer сохраняются (`ErrTreeChanged`/`ErrBusy`, WebDAV 409/423, UI 409) | `remove_tree_test.go` (число захватов lock, сохранение записанного и открытого), `TestRemovingALargeTreeLetsOtherWritesThrough` |
| F8 | Остановка = grace Docker | Дренаж всех listener'ов параллельно, не дольше 8 с; compose `stop_grace_period: 30s` | `cmd/birakd` тесты остановки |
| F9 | nginx спулил APK на диск, без keepalive, 502 после пересоздания | `proxy_buffering off`, upstream `keepalive` с `resolve`, resolver Docker; пример HAProxy с маршрутизацией по `/readyz` | `nginx -t` и `haproxy -c` на образах 1.27.5 и 3.0.5. E2E: SigV4 PUT/GET 3 MB через TLS, `.apk` Content-Type, после пересоздания контейнера 200 через 8 с без reload |
| F10 | Медленный клиент держал ресурсы бесконечно | Таймаут простоя 5 минут на тело и ответ во всех шлюзах и peer API; движущаяся передача не ограничена | `internal/gateway/idle_test.go` (застрявшие загрузка и скачивание, медленная загрузка дольше трёх таймаутов, долгий commit после тела) |
| F11 | Access-лог без ротации и полей | `duration_ms`, `remote`, `forwarded_for`; `access_log: false`; compose ротирует (`local`, 5×100 МБ); обрывы клиентов не ERROR | `TestLogMiddlewareRecordsTimingRemoteAndCounts`, `abort_test.go` (WebDAV), UI-тесты |
| F12 | Не было места на диске, счётчиков, версии; `/metrics` только по секрету | `birak_disk_*`, `birak_gateway_requests_total`, `birak_build_info`, `-version`, `/status.version`, `metrics_token` (Bearer). Семейства сгруппированы; возрасты считаются от старта до первого успеха | `metrics_test.go`, `TestMetricsTokenOpensOnlyMetrics` |
| F13 | Листинг большого каталога в UI: ~90 МБ на запрос | Имена партиями, `Lstat` только для страницы, не больше 4 листингов одновременно | 300 тыс. файлов: 3.78 → 0.24 с на страницу; 8 параллельных: maxrss 674 → 148 МиБ |
| F14 | Потеря meta проходила молча, индекс без прогресса | WARN о новой БД на привязанном томе; прогресс sweep каждые 30 с | `reindex_visibility_test.go` |
| F15 | `prefix/` создавал файл и ломал загрузки | Маркер создаёт каталог; HEAD/GET/DELETE маркера | `folders_test.go`, LIST не изменился |
| F16 | Грубый mtime на реплике — полный rehash каждый sweep | Точность mtime тома измеряется при старте; сравнения с её учётом (sweep, карантин, ETag) | `mtime_granularity_test.go` (watcher, server), `fileops/mtime_test.go` |
| F17 | EIO только логировался | Карантин и repair, `birak_read_errors_total` | `read_errors_test.go` |
| F18 | Пустые и неверные peers принимались | Пустые элементы отбрасываются, нужен `http(s)://host`; `node_id` без пробелов; `BIRAK_IGNORE` триммится | `TestPeerListIsValidated` |
| F19 | Presigned без потолка; `.BIRAK` открывал служебный каталог | `X-Amz-Expires` 1..604800 и дата scope = `X-Amz-Date`; все написания `.birak` и scratch-префиксов зарезервированы; NUL отклоняется всеми шлюзами | `sigv4_test.go`, `TestReservedNamesInEverySpelling`, `TestList_VisibleNameMatchesGateway` |
| F20 | Отклонения S3 и UI | 412/304/416 в XML без ложного `Content-Length`; коды NoSuchBucket, KeyTooLongError, ParentIsObject, ObjectExistsAsDirectory, NameCollision, SlowDown; идемпотентный Complete с проверкой digest; условные PUT/Complete под commit lock; 413 до чтения тела; Content-Type по безопасному списку + `nosniff`; ETag записи = ETag HEAD. UI: загрузка потоком в том, 413, ошибки без путей, 400 на неверные имена | `conformance_test.go`, `completion_test.go`, `conditional_test.go`, UI `upload_test.go`, `errors_test.go` |
| F21 | Runbook и версия | README: обновление по нодам, бэкап снапшотом, восстановление ноды из peers, inotify. `find-aws-chunked` в образе | Скрипт проверен на macOS sh и busybox: находит signed и unsigned framing, пропускает обычные файлы и `.birak` |
| F22 | CI без закрепления; reconcile перечитывал страницы | Actions по SHA, govulncheck v1.9.0, `permissions: contents: read`, `latest` только для старшего тега; позиция reconcile сохраняется при прерывании | `TestReconcileResumesAfterAFullQueue` |

## Ревью

Ревью всего изменения нашло восемь мест. Исправлены шесть:

- ложные `unconfigured_pollers` до первого ответа своих peers;
- частичное удаление дерева при writer, открытом после начала;
- пустые каталоги после отказанной копии;
- 400 вместо 413 в UI;
- дублирование правил видимости в UI, где заодно отличалось написание scratch-префиксов;
- форматирование на каждый запрос.

Остальные два:

- **Ложное.** SFTP-сессии дренируются параллельно с HTTP тем же бюджетом.
- **Оставлено сознательно.** Удаление, которое syncer видит при проверке имени перед изменением от peer, не проходит через бюджет. Оно касается только имён, которые меняет peer.

## Проверки

- `go test -race -count=1 ./...` на macOS arm64 — все 15 пакетов; `go vet ./...`;
  `GOOS=windows go build ./...`.
- govulncheck v1.9.0 на Go 1.26.10: 0 достижимых уязвимостей в исходниках и в
  Linux-бинаре.
- Четырёхнодовая Docker-приёмка (`scripts/docker_stress.py`, как в CI): все
  проверки, включая разделение сети, SIGKILL, восстановление старого backup и
  bitrot. 6931 PUT, 6545 независимо проверенных объектов, сходимость 20.9 с,
  PUT p50/p99 31/94 мс.
- TLS APK/иконки (`scripts/docker_read_load.py`): 10000 иконок, без ошибок,
  icon p99 25 мс.
- Совместимость: кластер v2.0.4 + v2.1.0 реплицирует запись и удаление в обе
  стороны. Откат v2.1.0 → v2.0.4 → v2.1.0 на тех же данных после multipart,
  маркера папки и PUT: обе версии ready, данные читаются.

## Что остаётся

- WebDAV MOVE/COPY большого дерева индексируются одним commit. Разделить их
  нельзя: наполовину перемещённое дерево видно клиентам и peers.
- Host-side удаление больше 1000 файлов за 10 минут требует рестарта с
  повышенным лимитом. Это намеренная цена защиты.
- Скан ОС-пакетов образа в CI не добавлен; образ обновляет пакеты Alpine при
  сборке (`apk upgrade`).
- Нагрузка на целевых дисках и uplink, отказ питания и восстановление вашим
  backup-инструментом по-прежнему требуют отдельной проверки
  ([production-readiness](../production-readiness.md)).
- Sebastian использует тот же код шлюзов и, по чтению кода, имеет те же
  дефекты CopyObject, presigned и HTTP UI; в этой работе не менялся.

## Воспроизведение

```sh
go test -race -count=1 -timeout 1500s ./...
docker build -t birak:filesystem-production-test .
python3 scripts/docker_stress.py --objects 1000 --writers 12 --duration 20 --large-mib 32
GOTOOLCHAIN=go1.26.10 go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```
