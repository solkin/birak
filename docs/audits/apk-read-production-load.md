# Нагрузка APK и иконок

Дата: 2026-10-08. Требования: 500000 файлов APK 10–500 MB, 10 TB чтения в
сутки, 300 новых APK в сутки; сопутствующие маленькие иконки скачиваются
значительно чаще. Для индекса проверяется и верхняя граница 1000000 имён
(APK + иконки). 10 TB/сутки — около 116 MB/s, или 110.4 MiB/s полезного
трафика без протокольных накладных расходов. Средняя скорость не ограничивает
пиковую: для production нужен запас и распределение клиентских запросов.

## Изменения

- S3 daemon читает LIST из live-name индекса SQLite постранично; в памяти
  только небольшая страница. `delimiter` перескакивает весь дочерний диапазон.
  Устранён предел первых 100000 объектов в daemon и повторный stat/sort всего
  bucket для каждой страницы. Физическая проверка выбранных кандидатов
  сохраняет фильтрацию отсутствующих файлов, ignore и безопасных symlink.
- Gateway ACK включает индексирование, поэтому S3 PUT сразу появляется в LIST.
  Внешний `cp` появляется в LIST после watcher/scan; GET читает текущий
  filesystem. Пагинация не является снимком всех объектов при конкурентных
  изменениях. Standalone S3 без Catalog сохраняет прежний filesystem fallback.
- Sweep получает metadata пакетами по 256 имён и использует уже прочитанные
  страницы для поиска удалений; изменённые файлы проверяются снова против
  текущего состояния под commit lock. Ошибка обхода по-прежнему запрещает
  deletion pass. Нет списка всех имён в памяти.
- Watcher events хешируются до commit lock; `Publish` тоже вычисляет hash
  staged bytes до захвата lock и передаёт его в индекс. Snapshot перепроверяется
  по generation при применении. Порядок durability сохранён: fsync staged
  bytes, intent, rename + directory fsync, metadata, ACK.
- `FileCount` сохраняет результат по версии индекса вместо COUNT всей таблицы
  на каждом scrape. Новая версия инвалидирует результат; рестарт считает заново.
- Метрики: `birak_active_readers`, `birak_file_read_bytes_total`,
  `birak_reader_revocations_total`. Bytes counter считает чтение файловых
  дескрипторов, включая peer/probe; это не подтверждённый сетевой egress.
- Recovery незавершённой replica publication теперь восстанавливает потерянный
  mtime по durable intent при совпавших SHA256 и размере. Ранее backup,
  сделанный после rename, но до metadata commit, мог превратить старые bytes
  в новую локальную запись. Новый детерминированный тест падает на старом коде
  для двух случаев: первый файл и overwrite индексированного файла. Другие
  bytes не принимаются за старую реплику.

Репликация остаётся filesystem, local ACK, без лидера/большинства. Глобальная
блокировка namespace commit сохранена; 300 новых APK в сутки не требуют
переработки пространства имён ради тысяч записей в секунду.

## Стенд и границы данных

`scripts/docker_read_load.py` создаёт отдельные Docker data/meta volumes и
offline synthetic fixture. Физически созданы 500000 отдельных inode иконок
(большинство 1 KiB, hot subset 1/32/128 KiB) и семь payload-файлов APK:
10/100/500 MiB по две копии и отдельные 500 MiB для filesystem events.
Это реальные обычные файлы, не hard links и не sparse placeholders.

SQLite содержит matching size/mtime/SHA256 и namespace reservations;
файловая система flush до запуска daemon. Это восстановление заранее
индексированного набора, а не первоначальное хеширование 5–250 TB APK.
Payload синтетический; тест не проверяет извлечение иконки из APK.

Чтение: проверяемый TLS с ephemeral CA, HTTP/1.1 keep-alive, signed SigV4,
SHA256 каждой полной `200`-передачи; около четверти повторных icon GET —
conditional requests с проверкой `304`. Одновременно идут 6 uploads по 10 MiB
и 3 изменения mtime отдельного 500 MiB файла, провоцирующие watcher.
Scrub остаётся включён с 8 MiB/s; sweep interval — 5 минут.

Сравнение до/после идёт последовательно на тех же volumes. Рабочий набор
помещается в RAM, поэтому steady-state цифры относятся к warm cache. Таймер
останавливает новые запросы через 60 секунд, затем дожидается in-flight APK;
скорость вычислена по всему времени, включая drain. Результаты не заменяют
24-часовой soak, холодное хранилище и измерение внешнего uplink.

## Уже подтверждённое сравнение

Первый Python TLS client на macOS, 24 icon readers + 8 APK readers:

| Показатель | До | После |
| --- | --- | --- |
| Startup scan 500007 физических файлов | 21.182 s | 5.147 s |
| LIST последних 1000 icons | 0 ключей, неполный результат | 1000 ключей, полный результат |
| Время LIST | 428.6 ms | 27.8 ms |
| Суммарное чтение APK + icons | 110.35 MiB/s | 121.09 MiB/s |
| Icon TTFB p99 | 27.28 ms | 25.52 ms |
| Прочитанные/проверенные bytes | 8.36 GB | 8.63 GB |
| Ошибки bytes/hash/transport | 0 | 0 |

Эта суммарная скорость не является скоростью APK отдельно. Независимый Go
client через macOS published ports измерил APK около 7.3–7.6 TB/day при
нагрузке icons. Такой результат не проходит порог 120 MiB/s APK и сохранён
как неуспешная bandwidth acceptance. Отдельный прогон внутри Linux bridge
выделяет daemon/storage из пути published ports Docker Desktop.

## Linux bridge: максимальная warm-cache нагрузка

Go client находится в отдельном контейнере в control bridge, обходит macOS
published ports. TLS сертификат проверяется against ephemeral CA и ожидаемое
имя `127.0.0.1`; проверка не отключается. HTTP/1.1 keep-alive, 24 icon workers,
8 APK workers, offered icon rate 1000/s. VM общая для server/client, 8 CPU,
14.6 GiB RAM. Прогон — PASS; все 200 responses проверены SHA256, ошибки — 0.

| Показатель | До | После |
| --- | --- | --- |
| Startup scan | 22.391 s | 5.176 s |
| LIST tail | 0 ключей / 477.3 ms | 1000 ключей / 27.2 ms |
| APK throughput | 2067.7 MiB/s | 2004.6 MiB/s |
| Суммарный throughput | 2101.5 MiB/s | 2037.9 MiB/s |
| Icon requests/s, включая drain | 846.8 | 834.9 |
| Icon TTFB p50 / p99 | 10.15 / 107.62 ms | 10.29 / 107.21 ms |
| Icon worst TTFB | 310.0 ms | 274.5 ms |
| APK / icon responses | 589 / 51540 | 580 / 51043 |
| Upload ACK | 6 | 6 |
| Errors | 0 | 0 |

Throughput после изменений не увеличился; цель — корректность большого LIST,
стоимость scan и отсутствие хеширования APK под commit lock. Разница bulk
throughput около 3% в последовательном прогоне не устанавливает регрессию
или улучшение SLA. Нагрузка около 2 GiB/s насыщает CPU/traffic внутри VM;
p99 icons в таком режиме около 107 ms. Эту цифру нельзя выдавать за latency
при нормальной нагрузке 10 TB/day. Для неё отдельно проверен профиль
240 MiB/s APK + 1000 offered icon requests/s.

## Рабочий профиль APK и иконок

Candidate, тот же Linux bridge и проверяемый TLS, APK offered rate 240 MiB/s,
icons 1000/s, 8 APK + 24 icon workers. Окно новых запросов — 60 секунд;
полное время с завершением больших APK — 75.032 секунды. PASS:

| Показатель | Результат |
| --- | --- |
| APK throughput, включая drain | 220.71 MiB/s |
| Суммарное чтение | 252.24 MiB/s |
| Icon responses | 59507, включая 14223 conditional 304 |
| Icon requests/s в 60-секундном окне / с drain | около 992 / 793 |
| Icon TTFB p50 / p99 / worst | 0.556 / 4.854 / 209.2 ms |
| Icon полное время p99 | 5.007 ms |
| APK responses / upload ACK | 73 / 6 |
| Ошибки hash/size/transport | 0 |

220.71 MiB/s соответствует примерно 20 TB/day при непрерывной такой скорости,
то есть запасу около 2× к средней цели APK. Это экстраполяция короткого
warm-cache прогона. Не было физической передачи 20 TB или суточного soak.
1000 icon requests/s — выбранный проверочный профиль, точный production rate
пока не задан. Worst latency сохранена, чтобы p99 не скрывал отдельные задержки.

## Индекс и регрессии

Linux Go 1.26: миллион synthetic metadata names вставлены за 4.615 s;
последняя страница из 1000 записей — 2.118 ms, 1000 повторных чтений cached
count — 23.333 µs. Это проверка SQLite range seek/count, без миллиона физических
APK и без стоимости их первичного хеширования.

Полный Linux race-набор с ENOSPC на отдельном 16 MiB tmpfs прошёл. Отдельно
четыре daemon-контейнера с большим объектом 500 MiB прошли: 2861 PUT за 20 s,
3286 проверенных живых объектов, сходимость после разделения за 39.34 s,
PUT p50/p99 43.7/337.3 ms. Проверены изоляция, clone admission и SIGKILL,
старый offline backup с потерянными mtime, отсутствие воскрешения и bitrot repair.

Первая полная macOS race-проверка выявила указанное окно replica intent.
После исправления пять повторов реального backup restore прошли на macOS;
полные `go test -race -count=1 -timeout=900s ./...` прошли на macOS/APFS и
Linux/Go 1.26 под UID 1000 с отдельным ENOSPC tmpfs. `go vet`, Windows cross-build
и Docker build также прошли. Windows build не заменяет исполнение на NTFS.

Финальный четырёхнодовый повтор на пересобранном образе также PASS:
1728 PUT, 2380 проверенных объектов, сходимость 47.06 s, все restore/clone/
SIGKILL/bitrot checks успешны. Этот повтор шёл одновременно с полными
race-наборами, поэтому его PUT latency/throughput не сравниваются с отдельным
нагрузочным прогоном. Измерения рабочего APK/icon профиля выше выполнялись
отдельно от тяжёлой валидации.

CI расширен TLS correctness smoke для APK/icons; production throughput
порог на shared runner отключён. Локально тот же smoke на финальном образе
прошёл: 10000 физических icons + семь APK payloads, полный LIST tail,
verified TLS, hash/size/304, concurrent uploads/events. Удалённый запуск CI
в этой работе не выполнен.

Релизная проверка выявила отдельную ошибку выбора toolchain: CI по `go.mod`
использовал точный Go 1.26.0 и нашёл достижимые уязвимости стандартной
библиотеки. Ранее локальный scan запускался с другим Go и не доказывал
безопасность именно CI/Docker binary. Для v2.0.1 Go 1.26.9 закреплён в
`go.mod` и Docker; проверки повторяются на этом toolchain, vulnerability
gate сохраняется перед публикацией. Первый тег v2.0.0 не опубликовал образ.

## Воспроизводимость

```sh
docker build -t birak:read-load-candidate .
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/birak-readload ./scripts/readload
python3 scripts/docker_read_load.py --objects 500000 --duration 60 \
  --icon-readers 24 --apk-readers 8 --icon-rps 1000 \
  --go-client /tmp/birak-readload --client-network docker \
  --apk-mib-per-second 240 \
  --report /tmp/birak-read-load.json
BIRAK_SCALE_NAMES=1000000 go test ./internal/store -run TestProductionIndexScale -v -count=1
```

Использовать `GOARCH=amd64` на Linux amd64 Docker host. Пороги этого локального
стенда — 120 MiB/s суммарно и 120 MiB/s APK; для CI correctness smoke пороги
равны нулю, потому что runner не устанавливает production SLA. CI smoke
проверяет TLS, hash, conditional GET, concurrent upload/events и LIST.

Производственная эксплуатация требует измерить actual storage и uplink,
пики и icon request rate. 1 Gbit/s uplink почти полностью занят уже средней
полезной скоростью 10 TB/day, без запаса на icons, replication и overhead.
