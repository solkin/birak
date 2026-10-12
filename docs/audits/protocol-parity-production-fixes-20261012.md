# Исправления общего файлового слоя Birak и Sebastian

Все шесть проблем предыдущего межпроектного аудита исправлены в рабочих деревьях обоих проектов. Дополнительно ужесточены подтверждения multipart при ошибках directory fsync и защита фоновой очистки при потере storage root. По проверенному профилю оба проекта являются кандидатами для ограниченного Linux production. Клиенты должны отправлять S3 payload без aws-chunked encoding; такое кодирование теперь явно отклоняется, а не сохраняется как содержимое файла.

Проверка выполнена 12 октября 2026 года на macOS/APFS и Linux arm64 в Docker под UID 1000. Кандидат Birak основан на `e2a58613065d3e5846765edd00fcb5cbeaea6e59`, Sebastian — на `130200c71a7216ea511d8ff95a14fe2c5892eb57`. Это оценка локальных изменений поверх этих коммитов; ранее зелёный удалённый CI не является проверкой нового рабочего дерева. Точные hashes исходников и локальные результаты сохранены в evidence directory.

## Исправленные проблемы

| Проблема | Итоговое поведение | Регрессии |
| --- | --- | --- |
| F1 Сохранение aws-chunked обрамления | Оба S3 gateways возвращают 501 NotImplemented для Content-Encoding aws-chunked и STREAMING-* markers до чтения body или создания состояния. Прежние объекты и части сохраняются | Signed PutObject и UploadPart, варианты encoding/markers, проверка отсутствия body consumption |
| F2 Уязвимые Go и SSH в Sebastian | Go 1.26.9, x/crypto v0.56.0 и x/sys v0.47.0 выровнены. Docker и CI используют тот же toolchain; оба CI требуют source и release-binary vulnerability checks | Scanner исходников и бинарников из реальных образов, builds Linux и Windows |
| F3 Неполный S3 LIST | В обоих filesystem paths нет предела 100000 объектов. Heap хранит только max-keys плюс один кандидат, каталоги читаются пакетами, CommonPrefixes дедуплицируются в пределах кандидатов | 100001 физических файлов, V1 marker, V2 continuation, приоритет token над start-after, порядок объектов и prefixes, cancellation |
| F4 Зависание на FIFO | Sebastian использует nonblocking open и проверяет тип descriptor. Truncate выполняется после проверки regular file. SFTP сохраняет прямую запись в inode | FIFO readers и SFTP OPEN в обоих проектах, отсутствие удержания namespace mutex, реальные GET через S3/WebDAV/UI |
| F5 Запись в подменённый root | Sebastian удерживает root descriptor и проверяет inode до операций и перед ACK. Новые запросы получают 503 при смене root. Birak сохраняет sentinel и watcher fencing | Отсутствующий root, замена, возвращение оригинала, retarget symlink; настоящий daemon и три gateways; защита scratch, abort и TTL cleanup |
| F6 Подписи в access log | Auth query fields и URL userinfo удаляются из логируемого URL без изменения исходного запроса. uploadId и partNumber сохраняются | Успешный запрос и отказ auth, дубликаты и регистр полей, неизменность signed request |
| Directory fsync частей | Ошибки metadata/parent/part fsync возвращаются клиенту. Новая часть синхронизируется до удаления предшественника. При отказе первого flush ранее подтверждённая часть сохраняется | Fault injection directory I/O, metadata и parent flush, сохранение predecessor, создание отсутствующего root |

Фоновая очистка проверяет storage fence перед удалением scratch; Abort, завершение и TTL cleanup multipart удаляют staging через общий файловый контроллер. Старый процесс не получает право чистить staging нового тома. Windows сохраняет явно описанную политику best effort directory flush.

Birak сохраняет индексированный daemon LIST. Его fallback теперь использует тот же ограниченный набор кандидатов, что и Sebastian. Полная filesystem страница Sebastian всё ещё требует обхода подходящего namespace: память ограничена, но стоимость глубокой страницы растёт с каталогом. Это исправление полноты результата, а не обещание постоянного времени ответа.

Матрица [protocol parity](/Users/ivsolkin/Projects/birak/docs/protocol-parity.md) остаётся одинаковой в обоих проектах. В ней отдельно описаны portable spelling, индексированное LIST, native S3 TLS, привязка root и предусмотренные различия SFTP. Поддержка непроверенного streaming decoding не добавлялась: полный aws-chunked decoder и проверка подписей chunks требуют отдельной реализации.

## Результаты проверок

| Проверка | Результат |
| --- | --- |
| Полные `go test -race -count=1 -timeout 900s ./...` на macOS | Оба проекта прошли |
| Полные race suites на Linux arm64, UID 1000 | Оба проекта прошли; Birak дополнительно получил отдельный tmpfs 16 MiB для ENOSPC |
| Финальные проверки изменённых fileops, gateway, multipart и daemon regression | Пройдены на macOS и Linux после последней правки maintenance |
| `go vet`, Linux amd64 и Windows amd64 builds | Оба проекта прошли; исполнение на NTFS отдельно не подтверждается сборкой |
| Source и фактические Docker binary vulnerability scans | 0 достижимых уязвимостей в обоих проектах. В required module остаётся запись об OpenPGP, который не импортируется |
| Исходные 15 daemon probe наблюдений с acceptance assertions | Пройдены на macOS и Linux; 501 вместо неверного upload ACK, полный LIST, FIFO без зависания, отказ на смену root и чистые логи |
| Четыре Linux реплики Birak | SHA256, isolated ACK, partition convergence, case collision, cold admission, partial clone SIGKILL, restart, старый backup без mtime, no resurrection и bitrot repair прошли |
| TLS read workload Birak | 10000 иконок, synthetic APK, 20 секунд: 3992 icon requests, 556 APK requests, 6 upload ACK, 0 errors; проверяемый TLS, hashes, conditional GET и полный LIST tail |

Первый четырёхнодовый прогон: 6117 PUT, 5891 независимо проверенный живой объект, сходимость за 32.53 s. Повтор на образе после исправления упаковки: 7044 PUT, 6631 проверенный объект, сходимость за 42.40 s; сохранён в `birak-docker-final-acceptance.json`. Последующие правки касаются maintenance fencing; изменённые пакеты проверены отдельно и итоговые образы пересобраны и просканированы.

TLS профиль является короткой проверкой корректности в warm cache и внутренней Docker сети. Его throughput thresholds были нулевыми, как в CI smoke profile. Он не устанавливает production throughput SLA, суточный egress, cold I/O или внешний uplink.

У исходного Docker binary Birak флаги `-s -w` удаляли symbol table. govulncheck v1.8.0 в этом случае переходит к консервативной оценке required modules и отмечал неиспользуемый OpenPGP. Symbol tables сохранены, `-trimpath` оставлен; CI build flags совпадают с Docker. Итоговый binary scan различает linked code и отсутствующий пакет и завершается с exit code 0.

## Условия применения

Оба кандидата подходят для ограниченного deployment на проверенном Linux filesystem, с установленными upload limits, TLS и authentication на ingress, мониторингом ошибок и проверенным backup/restore. Это вывод по исправленным контрактам и выполненным испытаниям, а не универсальная гарантия для любого хранилища.

Перед включением трафика проверить профиль S3 клиентов: aws-chunked получает 501. Клиентские версии, ранее отправлявшие это кодирование, требуют настройки или адаптации. Если такие загрузки уже получали успешный ответ старого сервера, сравнить сохранённые объекты с исходными hashes и при необходимости загрузить исходные bytes заново: сервер не может безопасно определить намерение клиента по произвольному содержимому существующего объекта.

Sebastian связывает root с inode на время процесса. Перед каждым стартом должен присутствовать правильный mount: перезапуск устанавливает новую привязку к настроенному каталогу. Эта модель не хранит внешний идентификатор ожидаемого тома. Birak дополнительно связывает data и metadata persistent sentinel и остаётся асинхронной репликацией: локальный ACK не означает наличие нескольких физических копий.

Для конкретного production остаются длительная нагрузка на целевых дисках и сети, восстановление вашим backup-инструментом, требуемые SLO, физический power loss и native NTFS при использовании Windows. SIGKILL проверяет восстановление процесса и сохраняет page cache; он не заменяет проверку отключения питания.

## Материалы проверки

[Evidence directory](/Users/ivsolkin/Projects/birak/docs/audits/protocol-parity-production-fixes-20261012-evidence) содержит native/Linux race output, maintenance reruns, vulnerability scans, Docker acceptance JSON, daemon probe JSON и manifest исходников. Для воспроизведения regression suite использовать закреплённый Go 1.26.9 в обоих проектах:

```sh
GOTOOLCHAIN=go1.26.9 go test -race -count=1 -timeout 900s ./...
GOTOOLCHAIN=go1.26.9 go vet ./...
govulncheck ./...
```

Docker scenarios повторяются штатными `scripts/docker_stress.py` и `scripts/docker_read_load.py` Birak. Параметры прогона и результаты сохранены вместе с отчётом. Временные daemon-процессы и контейнеры испытаний остановлены.
