# Выравнивание Birak и Sebastian и готовность к production

Исправления и повторная оценка — в [отчёте после исправлений](protocol-parity-production-fixes-20261012.md). Ниже сохранён исходный результат проверки до изменений.

Проверка на 12 октября 2026 года охватывает общие возможности двух проектов: S3, WebDAV, SFTP, HTTP UI, multipart, файловую публикацию, восстановление после сбоев, аутентификацию и выпускные проверки. Базовые протоколы выровнены хорошо; защита от отказов и условия выпуска существенно различаются. Общего допуска к production сейчас нет: оба проекта подтверждают сохранение неверных байтов при `aws-chunked`, а Sebastian дополнительно требует обновления зависимостей и исправления нескольких отказов файлового слоя.

Birak проверен на `e2a58613065d3e5846765edd00fcb5cbeaea6e59`, тег `v2.0.2`; Sebastian — на `130200c71a7216ea511d8ff95a14fe2c5892eb57`. Рабочие деревья перед проверкой были чистыми. Код серверов не изменён. Локальные испытания выполнены на macOS arm64/APFS, на закреплённых Go 1.26.9 и Go 1.25.6 соответственно. Воспроизведения запускали настоящие daemon-процессы с одноразовыми данными и тестовыми ключами на loopback.

## Насколько совпадают общие возможности

Обе копии `docs/protocol-parity.md` побайтово совпадают. После замены импортов и служебных имён полностью совпадают, в частности, SigV4, wire-протокол SFTP, WebDAV locks, разрешение путей, scratch-регистрация, дерево recovery и асинхронный logger. Это свидетельство общей реализации, но не отдельное доказательство её корректности.

| Возможность | Выравнивание | Практический вывод |
| --- | --- | --- |
| Чтение и запись между S3, WebDAV, SFTP, UI и диском | Хорошее на штатных сценариях | Общие protocol matrix и regression suites проходят в обоих проектах |
| Multipart parts, restart, checksums, TTL и лимиты | Почти одинаковая реализация | Оба проверяют MD5 и конкретный SHA256, восстанавливают части и скрывают staging; streaming encoding требует исправления |
| COPY/MOVE с заменой назначения | Хорошее | Общие private staging, journals, identity/checksum recovery, overwrite и special COPY проверки |
| Защита namespace и symlink escape | Хорошее для проверенных путей | Служебные каталоги и scratch скрыты, разрешённые файловые aliases учитываются; защита чтения специальных файлов отстаёт в Sebastian |
| S3 LIST и большие каталоги | Существенный разрыв | Daemon Birak использует SQLite catalog; Sebastian молча теряет хвост после 100000 собранных записей |
| SFTP публикация | Различие предусмотрено контрактом | Birak публикует поколение на CLOSE и отменяет его при disconnect; Sebastian пишет в inode сразу и сохраняет частичные байты и hard links |
| Контроль целостности и потери тома | Существенный разрыв | Birak имеет storage sentinel, read fencing, scrub и quarantine; Sebastian имеет process lease, но не проверяет сохранение привязки пути к исходному тому |
| Auth и TLS | Базовая auth совпадает | SigV4, Basic Auth и SSH общие; Birak дополнительно умеет TLS для S3. Подписи presigned URL попадают в логи обоих проектов |
| Условия выпуска | Существенный разрыв | Оба требуют race tests и vet; Birak также требует govulncheck и Docker acceptance. Sebastian не проверяет известные уязвимости |
| Наблюдаемость | Различие архитектуры | Birak имеет readyz/status/metrics для состояния storage и репликации; Sebastian ограничивается gateway-проверками и логами |

Переносить в Sebastian всю watcher/SQLite/replication архитектуру не требуется. Нужны общие меры: безопасное открытие файлов, корректная пагинация, контроль исходного storage root и актуальные зависимости.

## Подтверждённые проблемы

P1 обозначает исправление до общего production-допуска. Для проблем, зависящих от содержимого или устройства тома, условие срабатывания указано отдельно. P2 допускает временную эксплуатационную меру, но требует закрытия перед широким использованием соответствующей возможности.

### F1 P1 Оба проекта сохраняют обрамление aws-chunked вместо файла

Подписанный SigV4 `PutObject` с `Content-Encoding: aws-chunked`, `X-Amz-Content-Sha256: STREAMING-UNSIGNED-PAYLOAD-TRAILER`, decoded length 5 и корректным CRC32 trailer получил `200` в обоих проектах. Последующий GET также получил `200`, но вернул 46 байт chunk framing и trailer вместо пяти байт `hello`.

Причина: streaming marker не является конкретным SHA256 и не включает проверку digest, после чего `r.Body` напрямую копируется в объект. HTTP transfer decoding не снимает отдельный слой `aws-chunked`. Отсутствие проверки streaming-подписи уже отмечено в исходнике, но последствия шире: сервер подтверждает объект с неправильным содержимым. TLS это не исправляет.

Места: [Birak PUT](/Users/ivsolkin/Projects/birak/internal/gateway/s3/handlers.go:977), [Sebastian PUT](/Users/ivsolkin/Projects/sebastian/internal/gateway/s3/handlers.go:956), [общая SigV4 реализация](/Users/ivsolkin/Projects/birak/internal/gateway/s3/sigv4.go:20). AWS определяет это кодирование для PutObject и UploadPart с trailing checksums: [описание AWS](https://docs.aws.amazon.com/AmazonS3/latest/userguide/checking-object-integrity-upload.html).

Минимальное исправление — явно отклонять неподдерживаемое content encoding и streaming markers до публикации, сохраняя прежний объект. Полная поддержка требует decoding, проверки decoded length, trailer checksum и подписей для signed streaming. Аналогичный поток UploadPart необходимо проверить отдельно: подтверждённое воспроизведение относится к PutObject, а общий путь WritePart также читает body без decoder.

### F2 P1 Sebastian закрепляет уязвимые Go и SSH библиотеку

`govulncheck ./...` с Go 1.25.6 сообщил **34 записи с достижимыми символами**; Linux amd64 target дал тот же результат и exit code 3. Birak с Go 1.26.9 сообщил **0** и exit code 0; одна запись относится к required module без найденного вызова уязвимого кода.

Для Sebastian есть прямые вызовы, соответствующие production-функциям: `ssh.NewServerConn` с x/crypto v0.49.0, `http.ServeContent` и `xml.Unmarshal` для завершения multipart. Например, [GO-2026-6355](https://pkg.go.dev/vuln/GO-2026-6355) описывает deadlock SSH до v0.56.0, [GO-2026-6609](https://pkg.go.dev/vuln/GO-2026-6609) — чрезмерную стоимость Range до Go 1.26.9, [GO-2026-6088](https://pkg.go.dev/vuln/GO-2026-6088) — stack exhaustion XML. Число 34 является результатом статического scanner, а не числом подтверждённых атак: часть записей зависит от платформы и конфигурации, включая TLS и HTTP/2.

Места: [go.mod](/Users/ivsolkin/Projects/sebastian/go.mod:3), [CI](/Users/ivsolkin/Projects/sebastian/.github/workflows/test.yaml:17), [Dockerfile](/Users/ivsolkin/Projects/sebastian/Dockerfile:1). CI выбирает точный Go из go.mod; Docker использует плавающий `golang:1.25-alpine`, поэтому протестированный toolchain и toolchain образа могут различаться. Состав уже опубликованного образа этим source scan не установлен.

Выровнять поддерживаемый patched toolchain в go.mod, CI и Docker; обновить SSH библиотеку как минимум до исправленной v0.56.0 и добавить обязательный vulnerability gate перед выпуском. Выбранная для Birak комбинация Go 1.26.9/x/crypto v0.56.0 прошла текущую проверку. Итоговый release binary тоже должен проходить проверку.

### F3 P1 Sebastian возвращает неполный S3 LIST как завершённый список

В bucket создан 100001 обычный файл `000000.txt`–`100000.txt`. Запрос `GET /large-bucket?list-type=2&start-after=099999.txt&max-keys=10` получил `200`, `KeyCount=0`, `IsTruncated=false`, хотя файл `100000.txt` существует.

`collectObjects` сначала ограничивает весь собранный набор 100000 записями, затем pagination применяет marker только к этому набору. Предупреждение остаётся в daemon log и не передаётся клиенту. Обход всех страниц V1/V2 также не сможет дойти до хвоста. Это нарушает inventory, backup и sync, использующие LIST.

Места: [лимит](/Users/ivsolkin/Projects/sebastian/internal/gateway/s3/handlers.go:30), [остановка обхода](/Users/ivsolkin/Projects/sebastian/internal/gateway/s3/handlers.go:546), [поглощение ошибки](/Users/ivsolkin/Projects/sebastian/internal/gateway/s3/handlers.go:617). Birak daemon подключает [Catalog](/Users/ivsolkin/Projects/birak/cmd/birakd/main.go:248); [индексированная пагинация](/Users/ivsolkin/Projects/birak/internal/gateway/s3/catalog.go:34) устраняет этот предел для daemon. Fallback без Catalog по-прежнему использует ограниченный filesystem walk.

Для Sebastian нужна полная пагинация с ограниченным рабочим набором или отдельный локальный listing index. Просто увеличить число 100000 недостаточно. До исправления нельзя использовать сервер для полного перечисления больших buckets.

### F4 P1 Sebastian блокирует файловые операции при открытии FIFO через SFTP

При наличии FIFO в обслуживаемом каталоге SFTP OPEN на чтение вызывает блокирующий `os.OpenFile` внутри общего `fileops.Do`. Пока у FIFO нет другого конца, удерживается root mutex. В воспроизведении обычный S3 PUT другого файла не смог завершиться; после открытия второго конца FIFO операции продолжились. Отмена сетевого запроса сама по себе не прерывает файловый open.

GET этого FIFO через S3, WebDAV и UI также зависал до появления другого конца. Birak во всех трёх GET сразу вернул 500, SFTP OPEN ответил STATUS, а параллельный S3 PUT получил 200. Клиенты не создают FIFO штатным upload API; условие отказа — специальный файл, попавший в root через локальный filesystem.

Места: [Sebastian SFTP OPEN](/Users/ivsolkin/Projects/sebastian/internal/gateway/sftp/handlers.go:395), [Sebastian GET](/Users/ivsolkin/Projects/sebastian/internal/gateway/s3/handlers.go:861), [Birak безопасный reader](/Users/ivsolkin/Projects/birak/internal/fileops/reader.go:61).

Перенести самостоятельную защиту от блокирующего открытия специальных файлов, без необходимости watcher hooks: nonblocking open там, где он нужен, затем проверка типа открытого descriptor. Предварительного stat недостаточно из-за замены файла между stat и open. До исправления необходимо обеспечивать root только с обычными файлами и каталогами.

### F5 P1 Sebastian подтверждает записи после подмены storage root

После запуска daemon исходный root переименован в `original-volume`, а по прежнему пути создан новый каталог с bucket. Sebastian подтвердил PUT кодом 200 и записал bytes в новый root, продолжая держать process lease в старом root. Birak ответил 500 и не опубликовал файл. Исходные данные в обоих случаях сохранены.

Это воспроизведение подмены каталога, а не настоящий unmount. Оно подтверждает отсутствие проверки идентичности root в Sebastian. На deployment со съёмным, сетевым или заменяемым томом такой отказ может подтверждать запись на underlying/replacement storage; после возврата исходного тома эта запись пропадёт из видимого namespace.

Места: [lease только при старте](/Users/ivsolkin/Projects/sebastian/cmd/sebastiand/main.go:58), [проверка перед mutation](/Users/ivsolkin/Projects/sebastian/internal/fileops/fileops.go:34), [Birak storage sentinel](/Users/ivsolkin/Projects/birak/internal/watcher/watcher.go:719).

Sebastian требуется собственная storage identity или удерживаемый root descriptor с проверяемыми правилами публикации. До этого использование заменяемых томов требует внешнего fail closed контроля доступности mount, а сам daemon не даёт такой гарантии.

### F6 P2 Оба проекта записывают подпись presigned URL в лог

Общий middleware пишет `r.URL.String()` целиком, включая `X-Amz-Signature` и credential scope. Диагностический запрос с тестовым signature marker оставил этот marker в журнале обоих процессов. Запись происходит и для неуспешной авторизации; для успешных запросов используется тот же middleware без редактирования query.

Места: [Birak logger](/Users/ivsolkin/Projects/birak/internal/gateway/gateway.go:315), [Sebastian logger](/Users/ivsolkin/Projects/sebastian/internal/gateway/gateway.go:304). Читатель логов может получить действующую ссылку на операцию с правами presigned URL до окончания её срока.

Убирать authentication query fields из access log, сохраняя path и нужные диагностические параметры. До изменения доступ к raw логам должен соответствовать доступу к данным; ingestion в общие журналы расширяет круг владельцев bearer-ссылок.

## Какие различия нужно уточнить в контракте

Текущая одинаковая матрица хорошо описывает SFTP publication, ignore rules и очистку пустых родителей. Она не отражает новые ограничения portable names в Birak: case/Unicode spelling резервируется с историей удалений, тогда как Sebastian следует свойствам своего filesystem. Это оправдано репликацией, но означает разные допустимые ключи и поведение переименования.

Также стоит явно указать индексированное S3 LIST в Birak и задержку видимости новых внешних filesystem-записей до indexing, наличие native S3 TLS только в Birak, защиту потери root и условия release security checks. Различия документировать после выбора поведения; исправления общих protocol failures переносить с регрессиями в оба проекта.

В обоих multipart stores отдельный helper directory sync игнорирует ошибки Open/Sync/Close при публикации metadata и parts. Финальный объект проходит более строгий fileops.Publish, но ACK промежуточных parts не является одинаково строгой гарантией directory durability при I/O failure. Это риск по чтению кода; отказ питания или fault injection этого helper в данной проверке не воспроизводился. Для заявленной долговечности частей нужно отделить явно поддерживаемый best effort от настоящего EIO и отражать его в результате или наблюдаемости.

## Выполненные проверки и выпуск

| Проверка | Birak | Sebastian |
| --- | --- | --- |
| `go test -race -count=1 -timeout 900s ./...` на закреплённом Go | Прошёл весь набор; root package 102.379 s | Прошёл весь набор; root package 2.241 s |
| `go vet ./...` на закреплённом Go | Прошёл | Прошёл |
| Native daemon build и `GOOS=windows GOARCH=amd64 go build ./...` | Прошли | Прошли |
| govulncheck macOS и Linux amd64 source targets | 0 с достижимыми символами | 34 с достижимыми символами |
| Настоящие daemon probes | Подтверждены F1, защита FIFO/root и F6 | Подтверждены F1–F6 |
| GitHub CI текущего HEAD | Test и Docker workflow success | Test workflow success |

Для Birak [release workflow текущего SHA](https://github.com/solkin/birak/actions/runs/37832195273) и [Test](https://github.com/solkin/birak/actions/runs/37831129791) завершились успешно. Для Sebastian [Test текущего SHA](https://github.com/solkin/sebastian/actions/runs/37740835180) успешен, а последний из пяти полученных Docker runs относится к предыдущему SHA `f36cbbe`, [run](https://github.com/solkin/sebastian/actions/runs/36620515553). Зелёный Test Sebastian не включает vulnerability gate.

Docker daemon на локальной машине недоступен, поэтому Linux-контейнеры и нагрузочный стенд повторно здесь не запускались. Для оценки Birak используются подтверждённые CI jobs текущего SHA и границы уже существующей [production приёмки](/Users/ivsolkin/Projects/birak/docs/production-readiness.md:16). Короткий warm-cache throughput не доказывает суточный SLA, внешнюю сеть, cold I/O или восстановление после отключения питания. Windows build не заменяет исполнение на NTFS. Опциональные scale/benchmark/ENOSPC сценарии полного набора не входят автоматически в локальный запуск без соответствующих условий окружения.

## Условия допуска к production

**Birak ближе к ограниченному Linux production**, поскольку имеет storage fencing, контроль целостности, readiness/metrics, актуальный toolchain и реальную release acceptance. Для общего S3 deployment сначала закрыть F1 и F6. До F1 допустим только явно ограниченный клиентский профиль, который не отправляет aws-chunked; это эксплуатационное ограничение, а не исправление сервера. ACK по-прежнему подтверждает локальную публикацию, а не сохранность на нескольких репликах: потеря единственной копии до asynchronous replication остаётся выбранной моделью.

**Sebastian в текущем состоянии общего production-допуска не получает.** Первый шаг — patched Go/SSH и security gate, затем F1, корректный LIST и безопасное открытие файлов. Для deployment с заменяемым томом обязательна F5. При общем S3 API нельзя считать маленький regression fixture доказательством корректности больших catalogs.

Приёмка исправлений должна проверять сохранение старого объекта при неподдерживаемом/повреждённом streaming body; полноту страниц V1/V2 на наборе более 100000 ключей; отсутствие блокировки обычных операций при FIFO и его замене между проверкой и open; отказ ACK при смене root; отсутствие signature в логах; vulnerability scan выбранного release binary. После этого остаются deployment-проверки на целевых дисках, backup/restore, длительная нагрузка и требуемые SLO.

## Воспроизводимые материалы

В [evidence directory](/Users/ivsolkin/Projects/birak/docs/audits/protocol-parity-production-20261012-evidence) сохранены исходные JSON результатов daemon probes, Linux vulnerability output, GitHub CI jobs и диагностические программы. Они используют только временный root, loopback и искусственные credentials. `repro.py` создаёт 100001 пустой файл; выполнять его из временного каталога.

```sh
birak_repo=/Users/ivsolkin/Projects/birak
sebastian_repo=/Users/ivsolkin/Projects/sebastian
audit_tmp=$(mktemp -d)
cp "$birak_repo/docs/audits/protocol-parity-production-20261012-evidence/repro.py" "$audit_tmp/"
cp "$birak_repo/docs/audits/protocol-parity-production-20261012-evidence/root_probe.py" "$audit_tmp/"
cp "$birak_repo/docs/audits/protocol-parity-production-20261012-evidence/sftp-probe.go.txt" "$audit_tmp/sftp-probe.go"
cd "$birak_repo"
GOTOOLCHAIN=go1.26.9 go build -o "$audit_tmp/birakd" ./cmd/birakd
cd "$sebastian_repo"
GOTOOLCHAIN=go1.25.6 go build -o "$audit_tmp/sebastiand" ./cmd/sebastiand
GOTOOLCHAIN=go1.25.6 go build -o "$audit_tmp/sftp-probe" "$audit_tmp/sftp-probe.go"
python3 "$audit_tmp/repro.py"
python3 "$audit_tmp/root_probe.py"
```

Диагностические скрипты записывают наблюдения, а не устанавливают желаемое поведение assertions. Для повторного запуска выбирать новый временный каталог. Все запущенные для проверки daemon-процессы остановлены.
