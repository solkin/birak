# Birak — File Sync Daemon

Birak синхронизирует файлы между несколькими папками, каждая из которых обслуживается своим демоном. Демоны общаются по HTTP в full-mesh топологии.

## Принцип работы

- Каждый демон рекурсивно следит за своей папкой и всеми вложенными директориями (fsnotify + периодический скан).
- При изменении файла вычисляется SHA256. Файлы сравниваются по хешу, конфликты разрешаются по `mod_time` (свежий побеждает).
- Демоны поллят друг друга по HTTP, скачивая только отличающиеся файлы.
- Удаления передаются через tombstones с настраиваемым TTL.
- Синхронизация может стартовать с непустых папок — при первом запуске демон выполняет полный скан и обмен данными с пирами.
- Поддерживается конфигурируемый список игнорируемых файлов (паттерны glob).

## Быстрый старт

### Сборка

```bash
go build -o birakd ./cmd/birakd
```

### Конфигурация

Создайте YAML-файл для каждой ноды:

**node1.yaml**
```yaml
node_id: "node-1"
sync_dir: "/data/node1/sync"
meta_dir: "/data/node1/meta"
listen_addr: ":9100"
peers:
  - "http://192.168.1.2:9100"
  - "http://192.168.1.3:9100"
ignore:
  - ".DS_Store"
  - "Thumbs.db"
  - "desktop.ini"
  - ".birak-tmp-*"
  - "*.swp"
sync:
  poll_interval: 3s
  batch_limit: 1000
  max_concurrent_downloads: 5
  tombstone_ttl: 168h       # 7 дней
  scan_interval: 5m
  debounce_window: 300ms
```

**node2.yaml**
```yaml
node_id: "node-2"
sync_dir: "/data/node2/sync"
meta_dir: "/data/node2/meta"
listen_addr: ":9100"
peers:
  - "http://192.168.1.1:9100"
  - "http://192.168.1.3:9100"
```

Параметры секций `ignore` и `sync` можно не указывать — будут использованы значения по умолчанию.

### Запуск

```bash
./birakd -config node1.yaml
```

Демон создаст `sync_dir` и `meta_dir` если их нет, откроет SQLite базу в `meta_dir/birak.db` и начнёт синхронизацию.

Остановка — `SIGINT` или `SIGTERM` (Ctrl+C). Демон корректно завершает все операции.

## HTTP API

Три эндпоинта, можно проверить curl-ом:

### GET /changes?since=N&limit=1000

Возвращает список файлов с `version > N`. Используется пирами для синхронизации.

```bash
curl 'http://localhost:9100/changes?since=0&limit=100'
```

```json
[
  {
    "name": "docs/report.txt",
    "mod_time": 1738800000000000000,
    "size": 1024,
    "hash": "a1b2c3d4e5f6...",
    "deleted": false,
    "version": 42
  }
]
```

Поле `name` содержит относительный путь от `sync_dir` (например `docs/report.txt` для файла в поддиректории).

### GET /files/{name...}

Скачивает содержимое файла. Поддерживает вложенные пути.

```bash
curl -O 'http://localhost:9100/files/report.txt'
curl -O 'http://localhost:9100/files/docs/drafts/spec.pdf'
```

### GET /status

Состояние ноды.

```bash
curl 'http://localhost:9100/status'
```

```json
{
  "node_id": "node-1",
  "max_version": 42,
  "file_count": 1500
}
```

## Алгоритм синхронизации

1. Каждое изменение файла (создание, изменение, удаление) обновляет запись в SQLite-таблице `files` с новым монотонным `version`.
2. Пир B поллит пир A: `GET /changes?since=<cursor>`.
3. Для каждого файла в ответе: если SHA256 отличается от локального и `mod_time` входящего файла свежее — скачивает файл.
4. Курсор обновляется — при следующем запросе придут только новые изменения.
5. Initial sync (в том числе нового пира) — тот же запрос с `since=0`.

### Предотвращение циклов

Когда демон записывает файл, полученный от пира, он помечает его в in-memory set `recentlySynced`. Watcher видит событие fsnotify, но пропускает файл, если хеш совпадает с ожидаемым. Пометка автоматически удаляется через 5 секунд.

### Удаления

При удалении файла создаётся tombstone (`deleted=true`). Пиры получают его через `/changes` и удаляют файл локально. Tombstones очищаются после `tombstone_ttl` (по умолчанию 7 дней).

## S3 Gateway

Birak может выступать S3-совместимым шлюзом, предоставляя доступ к файлам в `sync_dir` через стандартный S3 API.

### Принцип

- **Бакеты** — это папки первого уровня в `sync_dir`. Бакет `photos` = директория `sync_dir/photos/`.
- **Объекты** — файлы внутри бакетов. Ключ `2024/img.jpg` в бакете `photos` = файл `sync_dir/photos/2024/img.jpg`.
- Создание/удаление бакетов = создание/удаление директорий. Бакеты не задаются в конфиге.
- Изменения через S3 API подхватываются watcher-ом и синхронизируются на другие ноды.

### Конфигурация

```yaml
gateways:
  s3:
    enabled: true
    listen_addr: ":9200"
    access_key: "admin"       # опционально
    secret_key: "secret123"   # опционально
```

Если `access_key` и `secret_key` не указаны, авторизация отключена.

### Поддерживаемые операции

| Операция | Описание |
|----------|----------|
| `ListBuckets` | Список бакетов (GET /) |
| `CreateBucket` | Создание бакета (PUT /{bucket}) |
| `DeleteBucket` | Удаление пустого бакета (DELETE /{bucket}) |
| `HeadBucket` | Проверка существования бакета (HEAD /{bucket}) |
| `ListObjectsV2` | Листинг объектов с prefix/delimiter (GET /{bucket}) |
| `PutObject` | Загрузка файла (PUT /{bucket}/{key}) |
| `GetObject` | Скачивание файла (GET /{bucket}/{key}) |
| `DeleteObject` | Удаление файла (DELETE /{bucket}/{key}) |
| `HeadObject` | Метаданные файла (HEAD /{bucket}/{key}) |

### Использование с AWS CLI

```bash
# Настройка
aws configure set aws_access_key_id admin
aws configure set aws_secret_access_key secret123

# Создать бакет
aws --endpoint-url http://localhost:9200 s3 mb s3://photos

# Загрузить файл
aws --endpoint-url http://localhost:9200 s3 cp image.jpg s3://photos/2024/image.jpg

# Скачать файл
aws --endpoint-url http://localhost:9200 s3 cp s3://photos/2024/image.jpg ./local.jpg

# Листинг
aws --endpoint-url http://localhost:9200 s3 ls s3://photos/

# Удалить
aws --endpoint-url http://localhost:9200 s3 rm s3://photos/2024/image.jpg
```

## WebDAV Gateway

Birak может выступать WebDAV-сервером, предоставляя доступ к файлам в `sync_dir` через стандартный протокол WebDAV. Совместим с macOS Finder, Windows Explorer, Linux davfs2, Cyberduck, rclone и другими WebDAV-клиентами.

### Принцип

- WebDAV — это расширение HTTP. Корень WebDAV-сервера = `sync_dir`.
- Файлы и директории доступны напрямую по путям (без концепции бакетов, как в S3).
- Изменения через WebDAV подхватываются watcher-ом и синхронизируются на другие ноды.
- Блокировки (LOCK/UNLOCK) — stub: возвращается fake token, достаточный для работы Finder и Windows Explorer.

### Конфигурация

```yaml
gateways:
  webdav:
    enabled: true
    listen_addr: ":9300"
    username: "user"           # опционально
    password: "secret123"      # опционально
```

Если `username` и `password` не указаны, авторизация отключена (доступ без пароля).

### Поддерживаемые операции

| Метод | Описание |
|-------|----------|
| `OPTIONS` | Список поддерживаемых методов, DAV compliance class 1, 2 |
| `PROPFIND` | Листинг директории / свойства файла (Depth: 0, 1) |
| `PROPPATCH` | Изменение свойств (stub — подтверждает без сохранения) |
| `GET` | Скачивание файла |
| `HEAD` | Метаданные файла |
| `PUT` | Загрузка файла (атомарная запись через temp file) |
| `DELETE` | Удаление файла или директории |
| `MKCOL` | Создание директории |
| `MOVE` | Перемещение / переименование |
| `COPY` | Копирование файла или директории |
| `LOCK` | Блокировка (stub — fake token) |
| `UNLOCK` | Разблокировка (stub) |

### Подключение

**macOS Finder:**
1. Finder → Go → Connect to Server (⌘K)
2. Введите: `http://localhost:9300`
3. При настроенной авторизации — введите логин и пароль

**Linux (davfs2):**
```bash
sudo mount -t davfs http://localhost:9300 /mnt/birak
```

**Cyberduck / rclone:**
```bash
rclone config  # тип: webdav, url: http://localhost:9300
rclone ls birak:/
rclone copy localfile.txt birak:/path/to/file.txt
```

## Структура проекта

```
birak/
  cmd/birakd/main.go              — точка входа, CLI, graceful shutdown
  internal/
    config/config.go              — парсинг YAML-конфигурации
    store/store.go                — SQLite: таблица files + cursors
    store/store_test.go           — unit-тесты store
    watcher/watcher.go            — fsnotify + debounce + periodic scan
    watcher/cleanup_test.go       — unit-тесты очистки директорий
    server/server.go              — HTTP API для peer-синхронизации
    syncer/syncer.go              — polling, conflict resolution, downloads
    gateway/gateway.go            — интерфейс Gateway
    gateway/s3/s3.go              — S3 Gateway: роутинг, авторизация
    gateway/s3/handlers.go        — обработчики S3 операций
    gateway/s3/s3_test.go         — unit-тесты S3 Gateway
    gateway/webdav/webdav.go      — WebDAV Gateway: роутинг, авторизация
    gateway/webdav/handlers.go    — обработчики WebDAV операций
    gateway/webdav/webdav_test.go — unit-тесты WebDAV Gateway
  integration_test.go             — интеграционные тесты (2-3 ноды)
```

## Тесты

```bash
# Все тесты
go test -v -timeout 120s ./...

# Только unit-тесты store
go test -v ./internal/store/

# Только S3 Gateway тесты
go test -v ./internal/gateway/s3/

# Только WebDAV Gateway тесты
go test -v ./internal/gateway/webdav/

# Только интеграционные
go test -v -timeout 120s -run TestIntegration
```

Интеграционные тесты поднимают 2-3 реальных ноды на localhost с HTTP-серверами и проверяют:
- Создание, изменение, удаление файлов
- Синхронизацию вложенных директорий (включая глубокую вложенность)
- Синхронизацию в 3-нодном full-mesh
- Conflict resolution (newer wins)
- Начальную синхронизацию непустых папок
- Массовую синхронизацию (20 файлов)
- Игнорирование системных файлов (.DS_Store и т.д.)
- Удаление файлов в поддиректориях с очисткой пустых папок
- Очистку директорий на исходной ноде при удалении файлов

S3 Gateway тесты покрывают:
- Авторизацию (V2, V4, без авторизации, неверные ключи)
- Операции с бакетами (создание, удаление, листинг, HeadBucket)
- Операции с объектами (PutObject, GetObject, HeadObject, DeleteObject)
- Листинг с prefix/delimiter (виртуальные директории)
- Вложенные ключи, перезапись, пустые файлы, большие файлы
- Игнорируемые файлы, защиту от path traversal
- Жизненный цикл сервера (Start/Stop)

WebDAV Gateway тесты покрывают:
- Авторизацию (Basic Auth: без авторизации, корректные/некорректные креды, отключённая авторизация)
- PROPFIND (корень, файлы, Depth: 0/1, фильтрацию ignored файлов, валидность XML)
- Операции с файлами (GET, HEAD, PUT новый/перезапись, автосоздание родительских директорий)
- Удаление файлов и директорий, запрет удаления корня
- MKCOL (создание директорий, дубли, отсутствие родителя)
- MOVE (переименование, перезапись, запрет перезаписи с Overwrite: F)
- COPY (файлы, рекурсивное копирование директорий)
- LOCK/UNLOCK (fake tokens)
- PROPPATCH (stub)
- Корректную отдачу index.html без редиректа
- Защиту от path traversal
- URL-кодирование путей с пробелами и спецсимволами

## Параметры конфигурации

| Параметр | По умолчанию | Описание |
|----------|-------------|----------|
| `node_id` | `node-1` | Уникальный ID ноды |
| `sync_dir` | `./sync` | Папка для синхронизации |
| `meta_dir` | `./meta` | Папка для SQLite базы (метаинформация) |
| `listen_addr` | `:9100` | Адрес HTTP-сервера |
| `peers` | `[]` | Список URL пиров |
| `ignore` | `.DS_Store`, `Thumbs.db`, `desktop.ini`, `.birak-tmp-*` | Glob-паттерны игнорируемых файлов |
| `sync.poll_interval` | `3s` | Интервал polling пиров |
| `sync.batch_limit` | `1000` | Макс. записей за один запрос |
| `sync.max_concurrent_downloads` | `5` | Параллельных скачиваний на пир |
| `sync.tombstone_ttl` | `168h` | Время жизни tombstones (7 дней) |
| `sync.scan_interval` | `5m` | Интервал периодического скана |
| `sync.debounce_window` | `300ms` | Окно дедупликации событий fsnotify |
| `gateways.s3.enabled` | `false` | Включить S3 Gateway |
| `gateways.s3.listen_addr` | `:9200` | Адрес S3 Gateway |
| `gateways.s3.access_key` | _(пусто)_ | Ключ доступа (опционально) |
| `gateways.s3.secret_key` | _(пусто)_ | Секретный ключ (опционально) |
| `gateways.webdav.enabled` | `false` | Включить WebDAV Gateway |
| `gateways.webdav.listen_addr` | `:9300` | Адрес WebDAV Gateway |
| `gateways.webdav.username` | _(пусто)_ | Имя пользователя (опционально) |
| `gateways.webdav.password` | _(пусто)_ | Пароль (опционально) |
