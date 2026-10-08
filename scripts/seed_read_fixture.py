#!/usr/bin/env python3
"""Offline synthetic read fixture for explicitly mounted disposable volumes."""
import argparse
import base64
import ctypes
import hashlib
import json
import os
from pathlib import Path
import sqlite3
import time

p = argparse.ArgumentParser()
p.add_argument('--objects', type=int, required=True)
a = p.parse_args()
if not 1000 <= a.objects <= 2000000:
    p.error('objects must be between 1000 and 2000000')
root, meta = Path('/data/sync'), Path('/data/meta')
assert (meta / 'birak.db').is_file(), 'initialize the daemon before seeding'
assert not (root / 'bucket/icons').exists(), 'fixture volumes must be fresh'
for name in ('bucket/icons', 'bucket/apks'):
    (root / name).mkdir(parents=True)
conn = sqlite3.connect(meta / 'birak.db')
conn.execute('PRAGMA journal_mode=WAL')
conn.execute('PRAGMA synchronous=FULL')
assert conn.execute("SELECT value FROM node_meta WHERE key='portable_names_v1'").fetchone() == ('1',)
assert conn.execute('SELECT COUNT(*) FROM files').fetchone()[0] == 0
conn.executemany('INSERT OR IGNORE INTO namespace_names(folded,name) VALUES (?,?)',
                 [(s, s) for s in ('bucket', 'bucket/icons', 'bucket/apks')])
rows, names, hot = [], [], {}
version = 0
start = time.monotonic()
png = base64.b64decode('iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+j6ioAAAAASUVORK5CYII=')
block = hashlib.sha256(b'disposable apk read fixture').digest() * 2048

def register(name, payload=None, size=None):
    global version
    path = root / name
    digest = hashlib.sha256()
    with path.open('wb') as f:
        if payload is not None:
            f.write(payload)
            digest.update(payload)
        else:
            remaining = size
            while remaining:
                part = block[:min(remaining, len(block))]
                f.write(part)
                digest.update(part)
                remaining -= len(part)
            os.fsync(f.fileno())
        f.flush()
        info = os.fstat(f.fileno())
    version += 1
    rows.append((name, info.st_mtime_ns, info.st_size, digest.hexdigest(), 0, version, info.st_mtime_ns))
    names.append((name, name))
    return {'name': name, 'size': info.st_size, 'hash': digest.hexdigest()}

def flush():
    conn.executemany('INSERT INTO files(name,mod_time,size,hash,deleted,version,clock) VALUES (?,?,?,?,?,?,?)', rows)
    conn.executemany('INSERT INTO namespace_names(folded,name) VALUES (?,?)', names)
    conn.execute("UPDATE node_meta SET value=? WHERE key='last_version'", (str(version),))
    conn.commit()
    rows.clear()
    names.clear()

for i in range(a.objects):
    # Preserve separate inodes: hard links would distort corruption semantics.
    size = (1024, 32768, 131072)[i % 3] if i < 96 else 1024
    item = register(f'bucket/icons/{i:07d}.png', payload=png + bytes(size - len(png)))
    if i < 96:
        hot[item['name']] = item
    if len(rows) == 5000:
        flush()
    if (i + 1) % 100000 == 0:
        print(json.dumps({'seeded_icons': i + 1, 'elapsed_seconds': time.monotonic() - start}), flush=True)
for copy in range(2):
    for mib in (10, 100, 500):
        item = register(f'bucket/apks/{mib:03d}-{copy}.apk', size=mib * 1024 * 1024)
        hot[item['name']] = item
cold = register('bucket/apks/cold-touch.apk', size=500 * 1024 * 1024)
flush()
fixture = {'icons': a.objects, 'indexed_files': version, 'objects': hot, 'cold': cold,
           'seed_seconds': time.monotonic() - start, 'synthetic_payloads': True}
(meta / 'read-fixture.json').write_text(json.dumps(fixture))
conn.close()
# Flush this filesystem before restarting the daemon. There are no live clients
# or ACKs while the fixture is being generated; this is offline test setup.
fd = os.open(root, os.O_RDONLY)
libc = ctypes.CDLL(None, use_errno=True)
if libc.syncfs(fd) != 0:
    raise OSError(ctypes.get_errno(), 'syncfs test fixture')
os.close(fd)
print(json.dumps({k: v for k, v in fixture.items() if k not in ('objects', 'cold')}), flush=True)
