# Quorum mode operation

`storage_mode: quorum` connects the Raft engine to the daemon and S3. A successful
write means verified immutable bytes on a majority of the configured voters and
committed metadata. The default `filesystem` mode retains asynchronous replication.
There is no automatic conversion between their data layouts.

This mode is ready for supervised testing. Production acceptance still requires
workload capacity measurements and fault tests on the intended disks. Replica
repair, fenced collection and logical S3 backup/restore are now implemented. Windows NTFS durability is not supported yet.
See the [maintenance and recovery audit](audits/quorum-maintenance-recovery.md).

## Certificates and first node

Build both commands:

```sh
go build -o birakd ./cmd/birakd
go build -o birakctl ./cmd/birakctl
```

On the administration machine, create a private CA and separate node/operator
certificates. `ca.key` stays off serving nodes. Node certificates expire after
three months; renew them before expiry and restart nodes one at a time while a
majority remains available. Issuance refuses to overwrite files: issue renewed
credentials in a new offline directory containing the existing CA pair.

```sh
./birakctl ca --dir ./pki --cluster apk
./birakctl issue --dir ./pki --cluster apk --role admin --name operator
./birakctl issue --dir ./pki --cluster apk --role node --name n1 --hosts n1.example.net,10.0.0.11
```

Copy only `ca.crt`, `node-n1.crt` and `node-n1.key` to n1, readable by the daemon
account. Create a fresh private storage directory on persistent storage. It holds
both the metadata and immutable generations; do not modify its files externally.
The following configuration uses a separate public S3 TLS certificate:

```yaml
storage_mode: quorum
node_id: n1
log_level: warn
max_upload_bytes: 1073741824
quorum:
  cluster_id: apk
  state_dir: /srv/birak/quorum
  listen_addr: ':9100'
  advertise_addr: n1.example.net:9100
  ca_file: /etc/birak/ca.crt
  cert_file: /etc/birak/node-n1.crt
  key_file: /etc/birak/node-n1.key
  operation_timeout: 2m
  transfer_timeout: 30m
  backup_timeout: 168h
  maintenance_interval: 1m
  scrub_bytes_per_second: 67108864
  min_free_bytes: 1073741824
gateways:
  s3:
    enabled: true
    listen_addr: ':9200'
    access_key: replace-with-access-key
    secret_key: replace-with-random-secret
    tls_cert_file: /etc/birak/s3-fullchain.pem
    tls_key_file: /etc/birak/s3-key.pem
    buckets: [apk, icons]
```

S3 credentials must match on all voters, including nodes without a public S3
listener: followers forward signed requests to the leader, which authenticates
them again. Public plaintext listeners are refused; a loopback plaintext listener
is allowed behind a TLS proxy. Proxies must preserve the signed Host, path and
query without canonicalizing object keys.

```sh
./birakd -config n1.yaml -bootstrap
# Every later start omits -bootstrap:
./birakd -config n1.yaml
```

Bootstrap is accepted exactly once on fresh state. Keep it out of restart units.
Configured buckets are created through consensus after election. For containers,
mount a persistent volume at `quorum.state_dir` in addition to mounting configuration
and certificates; the legacy `/data/sync` and `/data/meta` volumes do not select
quorum storage. The image includes `birakctl` (`--entrypoint birakctl`).

## Admission and promotion

Issue n2's own certificate with `--name n2`. Give it a unique ID, address, fresh
state directory and the same cluster CA. Add seeds to its configuration:

```yaml
quorum:
  # Other fields as above, adjusted for n2.
  seeds:
    - id: n1
      address: n1.example.net:9100
```

Start n2 without bootstrap. A fresh node accepts cluster requests only from its
configured seed identities. Seeds are not votes or automatic membership. In this
example n1 must still be leader when it admits n2; if leadership moved, configure
that leader as a seed before admission.

For each command below, supply the same operator connection flags:

```sh
./birakctl status --cluster apk --node n1 --address n1.example.net:9100 \
  --operator operator --ca ./pki/ca.crt \
  --cert ./pki/admin-operator.crt --key ./pki/admin-operator.key
```

Run `join --member n2 --member-address n2.example.net:9100`, then
`catch-up --member n2`, then `promote --member n2`, with those flags. The target
must be the current leader. `join` creates a learner; it cannot serve S3 or vote.
`catch-up` copies retained generations while ordinary writes continue. Repeat it
if its deadline expires: already certified generations are not transferred again.
Set `--timeout` consistently with the server's `transfer_timeout` for long work.

`promote` durably freezes new publications, checks the final reference set and
copies its delta before changing the voting configuration. Inventory batches are
bound to the receiving process and storage epoch. A restart invalidates previous
certification; existing files are then hashed and flushed again locally. The
freeze survives a leader crash. Resume the same `promote` on the new leader, or
`cancel --member n2` if membership has not changed yet. Cancellation is refused
once the membership change has committed.

The same workflow adds n3 and further nodes. Majority is always `floor(N/2)+1`:
1/1, 2/2, 2/3, 3/4, 3/5. In particular, the intermediate two-voter cluster needs
both nodes to acknowledge writes. Losing contact does not shrink membership.

## Removal and recovery

Run `remove --member n2` against the leader. Before removal commits, all retained
generations must be certified on every remaining voter. This conservative drain
can require restoring other unavailable nodes first. To remove the leader, run
`transfer --member <other-voter>` first and address subsequent commands to the
new leader. Membership authorization is checked on every cluster request, so
removal also revokes reused TLS connections at nodes that applied the new config.

`status` reports leader, members, committed configuration state and any freeze.
`ready` succeeds only on a leader able to commit a barrier and access its original
storage volume, with no membership freeze. `snapshot` persists a metadata recovery
point; it is **not a complete backup** without generation files.

Restart a failed process using its original state. Never restore an old Raft
backup as an existing voter or bootstrap an isolated former voter. Full-volume
rollback cannot be reliably detected by identity files alone. Disaster recovery
with a new identity and controlled re-admission still needs dedicated tooling.

## S3 scope and limits

Supported paths include bucket create/list/head/delete, object PUT/GET/HEAD/DELETE,
CopyObject, multi-delete, ListObjects V1/V2 with prefix/delimiter/pagination,
ranges, conditional reads and conditional PUT. Keys are opaque logical strings:
case, repeated slashes and dot segments do not become filesystem paths.

Multipart initiation and part acknowledgements are replicated. Completion commits
the final object, closes the upload and records its result atomically. Retrying
the same ordered part list returns the recorded result without overwriting a
later object or resurrecting a deletion. Part overwrites and aborts fence a
concurrent completion. SHA-256 and Content-MD5 are verified before publication.
An ordinary PUT/DELETE retry is a new S3 operation; S3 has no general operation-ID
contract here. After a timeout, the previous operation may have committed.

Versioning, ACLs, custom object metadata, tagging, server-side encryption,
UploadPartCopy, additional checksum headers, stored HTTP headers other than Content-Type, and response overrides are not
implemented. Unsupported selections return an error. Clients using automatic
CRC/checksum or streaming signing extensions need compatible request settings;
SDK interoperability acceptance remains outstanding. Multipart upload listing
with a delimiter and `encoding-type=url` listings are also not supported.

Idle uploads and completion receipts expire according to `multipart.upload_ttl`
(default 24h when zero), checked at `multipart.cleanup_interval` (default 1h when
zero). Closed-upload part metadata is removed by that same worker. Expiry races
are guarded by committed indices. A completion retry after receipt expiry returns
`NoSuchUpload`; it cannot resurrect an old object. Physical bytes are reclaimed by
the explicit collection procedure below. `max_active_uploads`
bounds active records, and `max_concurrent_part_uploads` bounds all concurrent
S3 PUT/POST handlers in quorum mode (zero selects a conservative cap of 16).
Cluster data/inventory transfers have a separate cap of eight per node; Raft
control messages do not share those slots. Each uploaded object/part is bounded
by the configured size, timeouts and multipart limits. Incoming Raft snapshots
are capped at 1 GiB; metadata capacity at the target key count must be measured.

All active S3 operations go through the leader. Reads verify the whole generation
before exposing bytes, including HEAD/range requests; this has a significant I/O
cost for APKs. Followers and learners now repair missing live generations
automatically. A separate scrub checks full hashes with a persisted cursor, so
restarts resume a long scan. Listing allocates a bounded page but still scans the
in-memory key index. TLS protects transport; data at rest uses the host filesystem
and its access controls. A continuously updated replica also replicates deletions;
keep independent backups for recovery from mistaken or malicious application writes.


## Upgrade from experimental v2

The current format is `birak-quorum-v3`. **Stop all nodes before upgrading**;
mixed wire formats are refused in both directions. Keep an offline copy of the
complete old state, then run on each stopped node using its existing identity:

```sh
./birakctl upgrade-v2 --dir /srv/birak/quorum --cluster apk --node n1
```

This acquires the node lease and durably replaces both identity markers. Raft
logs/snapshots from v2 are replayed without losing completed operations. Repeat
the same command after an interrupted marker update, then start all nodes with
new binaries and without `-bootstrap`. Do not edit identity files manually or
roll back the binary on v3 state. v1 and filesystem-mode migration are unsupported.

## Maintenance and collection

Every node, including a non-voting learner, runs two independent jobs. Repair
prioritizes the newest live references, checks presence and copies missing data
within a per-pass budget. Scrub hashes the retained generations and replaces
corruption only with bytes matching the expected hash and size. Detection revokes
existing readers of the damaged inode; bytes already delivered cannot be recalled.
The scrub cursor is advisory state in `scrub-progress.json`, saved every ten
seconds of work (with one-file granularity) and on clean cancellation. A crash
can repeat work since the last checkpoint; an interrupted file is checked again. Invalid/unwritable progress is an observable maintenance failure.

`quorum.scrub_bytes_per_second` budgets each job independently (default 64 MiB/s);
a single object can burst over that rate. Verification and temporary copies
add physical I/O beyond the logical byte budget. Repair runs at
`quorum.maintenance_interval` (default one minute). A backlog and slow Internet
still produce lag; learner admission is not proof of a current backup. Promotion
checks the complete frozen boundary. Each generation fetch has a bounded
`transfer_timeout`, including background work. `min_free_bytes` reserves at least
1 GiB by default; concurrent uploads reserve known length, or the configured
maximum for unknown-length bodies. Metadata, temporary repair copies and multipart
assembly require additional space. ENOSPC/fsync failure never turns into an ACK.

`birakctl status` reports local repair/scrub outcomes, cursor, free/reserved space,
active backup and collection fence. `Replication.Pending` counts missing files
remaining in the latest sampled reference set; it is not a bitrot check. `birakctl metrics` or the operator-mTLS
`GET /v1/admin/metrics` endpoint exposes Prometheus gauges. Alert on failed checks,
stale successful scrub completion, repair failures, storage errors, low space,
missing majority and an unexpectedly persistent collection fence. These are local
observations, not a certificate that all remote replicas are presently healthy.

Use the same connection/certificate flags as other operator commands:

```sh
./birakctl collect --cluster apk --node n1 --address n1.example.net:9100 \
  --operator operator --ca pki/ca.crt --cert pki/admin-operator.crt \
  --key pki/admin-operator.key --grace 24h --timeout 30m
```

Run collection in a maintenance window and schedule it according to churn and
space usage. It **pauses new writes and membership changes** while scanning;
reads of existing local generations can continue. Raft first freezes publication
and advances the proof epoch. Only a node with that exact applied fence can sweep
its own live set. A local gate excludes blob ACKs during deletion; late pre-freeze
proofs remain invalid after thaw. Live references, active readers, backup pins
and recently created files are retained. Grace concerns unreferenced file age,
not a promise to retain overwritten object versions for that duration.

The result contains per-node deletions and errors. An unreachable node retains
its garbage; inspect `Errors` even after HTTP 200. Repeat collection after it
returns. If a leader or operator connection dies, `status.Collection` can remain
set: repeat `collect` on the new leader to resume the same boundary, or use
`end-collection` to thaw without additional deletion. Both require a majority.
Delayed remote sweeps cannot delete later publications. Old generic core operation
receipts survive compaction for idempotency but do not retain their historical
bytes. New S3 requests use application guards/receipts instead of accumulating
permanent core receipts. A Raft snapshot is metadata recovery, not a full backup.

## Logical S3 backup and disaster recovery

Export a consistent boundary of **completed S3 objects and buckets** from the
current leader. Writes can continue; reference pins protect the selected local
bytes from collection. Only one export runs per node. The stream hashes each blob
and seals the metadata. The client publishes a private archive only after the
server's completion trailer, file fsync and directory fsync, and refuses overwrite.

```sh
./birakctl backup --cluster apk --node n1 --address n1.example.net:9100 \
  --operator operator --ca pki/ca.crt --cert pki/admin-operator.crt \
  --key pki/admin-operator.key --file /backup/apk-2026-10-07.tar --timeout 168h
./birakctl restore --file /backup/apk-2026-10-07.tar \
  --dir /srv/birak/restored --cluster apk-restored --node restored-1 \
  --address restored-1.example.net:9100 --timeout 168h
```

Archives are limited to ten million bucket/object records; metadata is held in
RAM during export and restore. This limit is not a tested capacity guarantee.
The server's independent `backup_timeout` defaults to seven days. Provide enough
space and protect the archive at rest: it contains plaintext object data. SHA-256
detects accidental damage; the archive is not an authenticated signature against
an attacker able to replace both data and checksums.

Restore is offline into a **nonexistent** destination and a **different cluster
ID**. It verifies every object and metadata seal, rejects extra/missing/unsafe
archive records, builds a fresh single-voter Raft state without network listeners,
and closes it before removing a durable incomplete marker. Failed or interrupted
restores refuse daemon startup; retry into a new destination. Existing state is
never overwritten. The advertised address must match the later daemon config.

Create new CA/node/operator credentials for the restored cluster, configure that
state directory/identity/address and start **without `-bootstrap`**. Check S3
contents, then join/promote new nodes normally. Upload IDs, completion retry
receipts, deleted/history versions, original LastModified timestamps, old Raft
membership and old node identities are not restored. Object bytes, keys, ETags
and Content-Type are retained. Practise this procedure on a separate cluster;
never restart copied old voter disks as independent members of a live cluster.
