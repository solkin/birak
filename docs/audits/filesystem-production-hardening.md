# Приёмка filesystem после отказа от quorum

Дата: 2026-10-08. Стратегия: независимые локальные filesystem-реплики,
асинхронная сходимость, бессрочная история удалений, произвольный уход и возврат
нод. Лидер и большинство не участвуют в доступности локальной записи.

## Найденные отказы и изменения

1. **Коллизии физического имени.** На APFS `App.apk` и `app.apk` могли дать два
   успешных применения и две строки SQLite при одном перезаписанном файле.
   Добавлены атомарные durable reservations каждого компонента пути по
   Unicode NFC + case folding и проверка физического написания до мутации.
   Политика едина на Linux/Windows/macOS; tombstone не освобождает написание.
   Миграция legacy-коллизий прерывается целиком. Конфликтующие независимые
   написания требуют ручного разрешения, остаются в очереди и сохраняют байты.
2. **Backup с потерянным mtime.** Старая копия с новыми датами получала новый
   clock и отменяла более новые удаления. Scan теперь проверяет hash и size
   неизменённых индексированных файлов, восстанавливает recorded mtime с
   fsync и сохраняет metadata/version. Explicit gateway intents сохраняют
   обычную семантику изменений. Требование согласованного backup data + metadata
   остаётся: иной hash или неизвестный индексу файл считается локальным вводом.
3. **Исчерпание clock.** Peer мог прислать предельный int64 и сделать следующую
   локальную запись непередаваемой, затем невозможной. Канонический decimal
   `big_clock` продолжает порядок без фиксированного integer ceiling. Он
   сохранён в SQLite, JSON, tombstones, conflict copies и namespace ordering.
   Неканонические значения отклоняются до публикации. Peer protocol повышен до
   4, чтобы старые бинарники не игнорировали расширение.
4. **Уязвимости SSH-зависимости.** Обновлены `x/crypto` до v0.56.0, `x/text` до
   v0.41.0 и связанные зависимости; минимум Go 1.26. Проверка govulncheck
   добавлена в CI. Linux/macOS и Windows-target scans: 0 reachable и 0 imported
   package findings; есть module advisory в неиспользуемом OpenPGP package.
5. **Слишком ранний readiness новой реплики.** Empty clone сохраняет marker 0
   до полного manifest comparison, успешного polling и применения очереди
   хотя бы одного источника. Частичная загрузка и рестарт marker не меняют.
   Admission 1 сохраняется и не отзывается из-за peer outages. Seed с локальными
   файлами и standalone допускаются как самостоятельные источники. Это не
   quorum и не доказательство глобальной свежести; gateway HTTP порты доступны,
   поэтому балансировщик обязан использовать `/readyz`.
6. **Наблюдаемость срока проверки данных.** Добавлены gauges admission, scrub
   budget, возраста последнего завершённого scrub и directory fsync best effort.
   Последний выставлен для Windows, где directory durability слабее Unix.

Сохранены quarantine/read fencing, integrity recovery с fsync, отказ от
редиректов с peer secret, S3 TLS и проверки удаления bucket. Экспериментальные
Raft/quorum/immutable-generation пути, CLI и зависимости удалены ранее в этом
же наборе изменений. Чужое quorum-хранилище автоматически не конвертируется.

## Автоматические проверки

| Проверка | Результат |
| --- | --- |
| macOS arm64 / APFS: полный `go test -race -count=1 -timeout 900s ./...` | PASS |
| Linux arm64 / Go 1.26 bookworm, UID 1000: тот же полный набор | PASS; 1010 успешных tests/subtests |
| Linux ENOSPC на отдельном tmpfs 16 MiB | PASS в полном race-наборе |
| Новые regression tests clocks/names/admission/restore | PASS; HTTP extended-clock и усиленные restore assertions дополнительно проверены после полного набора |
| `go vet ./...`, `go mod verify`, `git diff --check` | PASS |
| Linux amd64 и Windows amd64 `go build ./...` | PASS |
| Docker non-root production image, Go 1.26 Alpine | PASS |
| `govulncheck ./...` и Windows-target scan | 0 достижимых уязвимостей |

Опциональный `TestBenchSyncUnderLoad` запускается отдельно. Skip дочернего
helper `TestReplacementRecoveryChild` в полном наборе штатный; реальные
parent fault-сценарии его запускают сами. Полный Linux-набор реально выполнял
процессы daemon под race detector; это не только compile check.

## Реальные локальные инстансы

Стенд — [../../scripts/docker_stress.py](../../scripts/docker_stress.py), Python
stdlib + Docker CLI. Четыре отдельных `birakd`, самостоятельные SQLite и data
volumes, non-root UID 1000. Два bridge network: cluster link можно отключать,
оставляя контрольные S3/admin порты на localhost. S3 запросы подписаны SigV4;
peer secret генерируется для каждого прогона. Docker acceptance добавлен в CI
и release gate; отчёт с daemon logs сохраняется как artifact. Созданные стендом ресурсы
удаляются в `finally`; данные проекта не используются.

```sh
docker build -t birak:filesystem-production-test .
python3 scripts/docker_stress.py --objects 1000 --writers 12 --duration 45 \
  --large-mib 32 --report /tmp/birak-docker-acceptance.json
```

Первый сетевой/нагрузочный прогон (его restore step исключён после проверки
mount targets):

- 1000 исходных объектов 8 KiB; 12 writers, 45 секунд, 2922 успешных PUT.
- Независимые записи на разделённых сторонах, overwrite/delete и большой объект
  около 32 MiB; admitted node остаётся ready во время изоляции.
- Сходимость manifest/repair после соединения: 52.739 секунды.
- 3336 живых объектов прочитаны на каждой из трёх реплик с независимым SHA256.
- Подключение четвёртой ноды: readiness 503 до загрузки, затем 200 и сходимость.
- SIGKILL/restart подтверждённой реплики.
- Полное время 253.253 секунды. VM: 8 CPU, около 14.6 GiB RAM, Linux arm64.

Этот прогон делил VM с Linux race-проверкой. Его p50/p99 смешивают seed/stress/
large-object фазы, поэтому не используются как SLA.

Повторный recovery-прогон дополнительно убивает частично загруженный clone,
проверяет сохранение readiness 503 после локального скана без peer network и
только затем завершает admission. Также инъецирует другие байты с прежними
размером и mtime и проверяет integrity detection/repair до исходных bytes.

При первом запуске этого дополнительного сценария BusyBox `cp -p`/`touch -r`
обнулили наносекунды mtime. Нода правильно классифицировала изменение bytes +
mtime как внешний ввод, и ожидание repair не прошло. Исправленный стенд
сохраняет точный `st_mtime_ns` через Python utility-container и на время
инъекции приостанавливает только проверяемый daemon.

Дополнительно обнаружено, что declared Dockerfile VOLUME для `/data/sync`
и `/data/meta` скрывают parent `/data` volume. Поэтому прежний utility-container
не видел реальные bytes и backup; прежняя Docker-проверка restore исключена.
Стенд теперь явно монтирует отдельные sync/meta volumes во всех контейнерах,
проверяет старые bytes и наличие SQLite перед backup и после offline restore,
а затем проверяет результат на четырёх работающих нодах. Утилиты используют
Python image без скрывающих вложенных VOLUME. Оба ошибочных запуска не
учитываются как успешная проверка bitrot/restore.

Исправленный полный прогон — **PASS**, 218.846 секунды:

| Показатель | Результат |
| --- | --- |
| Ноды / исходные объекты / писатели | 4 / 1000 / 12 |
| Stress PUT за 20 секунд | 2347 |
| S3 stress PUT p50 / p99 | 71.273 / 303.924 мс |
| Сходимость после соединения сети | 31.272 с |
| Живые объекты, независимо прочитанные на трёх репликах | 2875 |
| Memory snapshot daemon в конце | 24.00–36.18 MiB |

Проверка partial clone SIGKILL + admission, SIGKILL основной реплики,
реальный restore старого data/meta, отсутствие resurrection и same-size /
exact-same-mtime bitrot repair — PASS. В логах восстановленной ноды есть
correction mtime report, у повреждённой ноды — quarantine report. После
repair manifest одинаковы на четырёх нодах, очереди пусты. Snapshot памяти
не является доказательством отсутствия утечек; отдельные leak tests входят
в полный race-набор.

Сырые локальные результаты этого захода: `/tmp/birak-production-20261008/`:
`docker-final-volumes.json` с daemon logs, `linux-race.jsonl`, `macos-race.log`,
`linux-load-and-regressions.log`, vulnerability reports. Старые Docker reports
с ошибочной инъекцией сохранены отдельно и не используются как подтверждение
backup/bitrot. Исправленный стенд удалил свои containers, networks и named
volumes; utility containers используют image без вложенных VOLUME.

## Отдельный нагрузочный benchmark

Linux Go 1.26, UID 1000, отдельный Docker data volume (не tmpfs для данных).
Опции: 1000 файлов 64 KiB, 2 больших файла 32 MiB, 12 writers, 20 секунд,
scrub 64 MiB/s. `TestBenchSyncUnderLoad` — PASS за 56.30 секунды.

| Показатель | Результат |
| --- | --- |
| Durable publish floor | 1.284 мс |
| Первая индексация 1002 файлов | 1.85 с, 542 файла/с |
| Idle write p50 / p99 | 4.350 / 11.841 мс |
| Loaded write p50 / p99 | 80.461 / 215.938 мс |
| Large overwrite p50 / p99 | 107.283 / 227.020 мс |
| Commit lock held average | 2.250 мс |
| Успешные записи под нагрузкой | 2669 за 20 с, 133/с |
| Readiness failures | 0 |
| Сходимость после возврата peer | 9.746 с, 2922 файла |

В этом же Linux-проходе `ExtendedClockReplicates`, `InitialAdmission`,
`ReplicaCaseCollision`, `RestoreWithoutTimestamps` выполнены под `-race`: PASS.
Физический unindexed case alias дополнительно проверен на APFS; в обычном
case-sensitive Linux этот конкретный filesystem fixture неприменим.

## Что этот стенд не доказывает

SIGKILL не сбрасывает page cache и не является power loss. Контейнеры делят
один Linux kernel и Docker VM; это не независимые физические fault domains.
Windows cross-build и scanner не выполняют тесты на NTFS. Размеры/число файлов
этого прогона не устанавливают готовность 10 TB, сроки скана или production
latency. Бюджет scrub нужно выбирать по допустимому сроку обнаружения: 10 TB
при 8 MiB/s — около 14 дней на цикл без конкуренции за I/O.

Репликация остаётся asynchronous LWW с local ACK: потеря всех доступных копий
не восстанавливается; потеря единственной неподелившейся ноды может потерять
ACK. Tombstones и reservations растут бессрочно ради возвращения старых нод.
Commit остаётся сериализованным на томе. Эти свойства входят в выбранную
стратегию, а не закрываются наличием зелёных тестов.
