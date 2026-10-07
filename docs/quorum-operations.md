# Quorum mode operation

`storage_mode: quorum` connects the Raft engine to the daemon and S3. A successful
write means verified immutable bytes on a majority of the configured voters and
committed metadata. The default `filesystem` mode retains asynchronous replication.
There is no automatic conversion between their data layouts.

This mode is ready for supervised testing. Production acceptance still requires
background replica repair, safe garbage collection, recovery tooling, and tests
on the intended disks and workload. Windows NTFS durability is not supported yet.
See the [implementation audit](audits/quorum-service-s3.md).

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

Generations, completion receipts, part metadata and tombstones are retained;
there is no garbage collector or multipart expiry in quorum mode yet. The legacy
multipart TTL settings apply only to filesystem mode. `max_active_uploads`
bounds active records, and `max_concurrent_part_uploads` bounds all concurrent
S3 PUT/POST handlers in quorum mode (zero selects a conservative cap of 16).
Cluster data/inventory transfers have a separate cap of eight per node; Raft
control messages do not share those slots. Each uploaded object/part is bounded
by the configured size, timeouts and multipart limits. Incoming Raft snapshots
are capped at 1 GiB; metadata capacity at the target key count must be measured.

All active S3 operations go through the leader. Reads verify the whole generation
before exposing bytes, including HEAD/range requests; this has a significant I/O
cost for APKs. Background repair, follower reads and large-scale listing/index
optimizations remain future work. A learner does not receive a continuous backup
of bytes automatically: schedule operational catch-up yourself until continuous
replica maintenance is implemented. TLS protects transport; data at rest uses the
host filesystem and its access controls.
