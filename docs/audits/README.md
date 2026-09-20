# Аудиты репликации

Каждый файл — один заход: что проверялось, что нашлось, что исправлено, чем
подтверждено и какие границы остались. Порядок ниже хронологический; читать
имеет смысл с конца, там текущее состояние.

| Заход | О чём |
| --- | --- |
| [sync-production-readiness](sync-production-readiness.md) | Первый аудит: отказы, найденные до начала работ |
| [sync-reliability-fixes](sync-reliability-fixes.md) | Их устранение и подтверждающие тесты |
| [replication-second-review](replication-second-review.md) | Воспроизведённые отказы второго захода |
| [replication-followup-fixes](replication-followup-fixes.md) | Их устранение |
| [replication-third-review](replication-third-review.md) | Третий заход |
| [replication-recovery-hardening](replication-recovery-hardening.md) | Восстановление после прерванных замен |
| [replication-namespace-conflicts](replication-namespace-conflicts.md) | Конфликты файл/директория и conflict copies |
| [replication-repair-scheduling](replication-repair-scheduling.md) | Планировщик очереди: медленные передачи, отмена, бюджет |
| [replication-repair-validation](replication-repair-validation.md) | Проверка ответов peer и переполнение диска |
| [replication-pagination-and-cache](replication-pagination-and-cache.md) | Повторы страниц, backoff, устаревший HTTP-кеш |
| [replication-partition-and-release-gates](replication-partition-and-release-gates.md) | Разделение сети на трёх нодах, условия допуска |
| [sync-replay-readiness-and-page-limits](sync-replay-readiness-and-page-limits.md) | Replay после рестарта, readiness при активной записи, границы ответа peer |
| [sync-simplification-round-1](sync-simplification-round-1.md) | Границы и стоимость, скраб с бюджетом, readiness ноды; затем единый путь применения |
| [load-stand-and-crash-consistency](load-stand-and-crash-consistency.md) | Нагрузочный стенд, стоимость записи, сериализация, проверка аварийным завершением |

Замысел, из которого выросли последние три захода, — в
[../sync-simplification-plan.md](../sync-simplification-plan.md): что именно
делает синхронизацию сложной и какие изменения это меняют.
