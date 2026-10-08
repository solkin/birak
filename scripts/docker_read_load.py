#!/usr/bin/env python3
"""TLS/SigV4 mixed APK/icon workload on a real, disposable indexed filesystem.

Requires Docker, Python 3 and openssl. Both images must already be built.
The optional baseline shares the same data volumes, tested sequentially.
"""
import argparse
from collections import deque
import concurrent.futures
import hashlib
import http.client
import json
from pathlib import Path
import random
import ssl
import subprocess
import tempfile
import threading
import time
import urllib.parse
import xml.etree.ElementTree as ET

from docker_stress import Stand, docker

NS = {'s': 'http://s3.amazonaws.com/doc/2006-03-01/'}

def percentile(values, fraction):
    values = sorted(values)
    return values[min(len(values) - 1, int(len(values) * fraction))] if values else None


def metrics(stand):
    status, body = stand.request(1, 'sync', 'GET', '/metrics')
    assert status == 200
    result = {}
    for line in body.decode().splitlines():
        if not line.startswith('#') and '{' not in line:
            parts = line.split()
            if len(parts) == 2:
                result[parts[0]] = float(parts[1])
    return result


def tail_listing(stand, objects):
    after = f'icons/{objects - 1001:07d}.png'
    path = '/bucket?' + urllib.parse.urlencode({'list-type': 2, 'prefix': 'icons/', 'start-after': after, 'max-keys': 1000})
    start = time.monotonic()
    status, body = stand.request(1, 's3', 'GET', path)
    assert status == 200, (status, body[:200])
    xml = ET.fromstring(body)
    keys = [x.text for x in xml.findall('s:Contents/s:Key', NS)]
    expected = [f'icons/{i:07d}.png' for i in range(objects - 1000, objects)]
    return {'seconds': time.monotonic() - start, 'returned': len(keys), 'complete_tail': keys == expected}


def exercise(stand, fixture, args):
    endpoint = urllib.parse.urlsplit(stand.nodes[1]['s3'])
    icons = [x for x in fixture['objects'].values() if '/icons/' in x['name']]
    apks = [x for x in fixture['objects'].values() if '/apks/' in x['name']]
    guard, done = threading.Lock(), threading.Event()
    samples = {k: deque(maxlen=100000) for k in ('icon_ttfb', 'icon_seconds', 'apk_ttfb', 'apk_seconds')}
    counts = {'icon_gets': 0, 'icon_304': 0, 'apk_gets': 0, 'bytes': 0, 'upload_acks': 0}
    errors, memory, monitor = [], [], []
    start = time.monotonic()
    deadline = start + args.duration

    def connect():
        return http.client.HTTPSConnection(endpoint.hostname, endpoint.port, context=stand.ssl_context, timeout=60)

    def reader(number, large):
        rng, conn, etags = random.Random(number), connect(), {}
        pool = apks if large else icons
        try:
            while time.monotonic() < deadline:
                item = rng.choice(pool)
                path = '/' + item['name']
                headers = stand.headers(1, 's3', 'GET', path)
                if not large and path in etags and rng.randrange(4) == 0:
                    headers['If-None-Match'] = etags[path]
                before = time.monotonic()
                conn.request('GET', path, headers=headers)
                response = conn.getresponse()
                first = time.monotonic() - before
                expected = response.status == 200
                assert expected or (not large and response.status == 304), (response.status, path)
                digest, size = hashlib.sha256(), 0
                while True:
                    chunk = response.read(256 * 1024)
                    if not chunk:
                        break
                    size += len(chunk)
                    digest.update(chunk)
                if expected:
                    assert size == item['size'] and digest.hexdigest() == item['hash'], ('bytes/hash', path, size)
                else:
                    assert size == 0 and response.getheader('ETag') == etags[path], ('conditional', path)
                etags[path] = response.getheader('ETag')
                prefix = 'apk' if large else 'icon'
                with guard:
                    counts[prefix + '_gets'] += 1
                    counts['icon_304'] += int(response.status == 304)
                    counts['bytes'] += size
                    samples[prefix + '_ttfb'].append(first)
                    samples[prefix + '_seconds'].append(time.monotonic() - before)
        except Exception as error:
            with guard:
                errors.append(repr(error))
        finally:
            conn.close()

    def observe():
        while not done.wait(2):
            try:
                monitor.append(metrics(stand))
                memory.append(docker('stats', '--no-stream', '--format', '{{.MemUsage}} {{.CPUPerc}}', stand.nodes[1]['name']))
            except Exception as error:
                with guard:
                    errors.append('monitor: ' + repr(error))

    def disturbances():
        # Events on a 500 MiB file while new icon GETs and large APK reads run.
        # Content stays constant, so every response still has a known hash.
        for _ in range(3):
            if done.wait(max(1, args.duration / 6)):
                return
            try:
                docker('exec', stand.nodes[1]['name'], 'touch', '/data/sync/' + fixture['cold']['name'])
            except Exception as error:
                with guard:
                    errors.append('filesystem event: ' + repr(error))

    def uploads():
        # A short burst exceeds the stated 300/day rate; no APK is buffered whole.
        conn = connect()
        block = bytes(64 * 1024)
        try:
            for i in range(6):
                if done.wait(max(1, args.duration / 8)):
                    break
                path = f'/bucket/uploads/{i:03d}.apk'
                headers = stand.headers(1, 's3', 'PUT', path, payload_hash='UNSIGNED-PAYLOAD')
                headers['Content-Length'] = str(10 * 1024 * 1024)
                conn.request('PUT', path, body=(block for _ in range(160)), headers=headers)
                response = conn.getresponse()
                body = response.read()
                assert response.status == 200, ('upload', response.status, body[:100])
                with guard:
                    counts['upload_acks'] += 1
        except Exception as error:
            with guard:
                errors.append('upload: ' + repr(error))
        finally:
            conn.close()

    workers = args.icon_readers + args.apk_readers
    before = metrics(stand)
    if args.go_client:
        fixture_path = stand.directory / 'read-fixture.json'
        fixture_path.write_text(json.dumps(fixture))
        client_endpoint = stand.nodes[1]['s3']
        command = [args.go_client, '--endpoint', client_endpoint, '--cert', str(stand.tls[0]),
                   '--fixture', str(fixture_path), '--duration', str(args.duration) + 's',
                   '--icon-readers', str(args.icon_readers), '--apk-readers', str(args.apk_readers),
                   '--icon-rps', str(args.icon_rps), '--apk-mib-per-second', str(args.apk_mib_per_second)]
        if args.client_network == 'docker':
            client_endpoint = 'https://' + stand.nodes[1]['name'] + ':9200'
            command = ['docker', 'run', '--rm', '--network', stand.control,
                       '-v', str(Path(args.go_client).resolve()) + ':/readload:ro',
                       '-v', str(stand.tls[0]) + ':/cert.pem:ro',
                       '-v', str(fixture_path) + ':/fixture.json:ro',
                       'python:3.14-alpine', '/readload', '--endpoint', client_endpoint,
                       '--cert', '/cert.pem', '--fixture', '/fixture.json', '--tls-server-name', '127.0.0.1',
                       '--duration', str(args.duration) + 's', '--icon-readers', str(args.icon_readers),
                       '--apk-readers', str(args.apk_readers), '--icon-rps', str(args.icon_rps), '--apk-mib-per-second', str(args.apk_mib_per_second)]
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
            observers = [pool.submit(observe), pool.submit(disturbances), pool.submit(uploads)]
            output, error = process.communicate(timeout=args.duration + 180)
            done.set()
            for future in observers:
                future.result()
        if process.returncode:
            raise AssertionError('Go read client failed: ' + error + output[:2000])
        result = json.loads(output)
        result.update({'client': 'Go; verified TLS HTTP/1.1 keep-alive; ' + args.client_network + ' network', 'upload_acks': counts['upload_acks'],
                       'metric_before': before, 'metric_after': metrics(stand), 'metric_samples': monitor,
                       'memory_cpu_samples': memory, 'duration_target_seconds': args.duration})
        result['errors'].extend(errors)
        return result
    with concurrent.futures.ThreadPoolExecutor(max_workers=workers + 3) as pool:
        observers = [pool.submit(observe), pool.submit(disturbances), pool.submit(uploads)]
        futures = [pool.submit(reader, i, False) for i in range(args.icon_readers)]
        futures += [pool.submit(reader, i + args.icon_readers, True) for i in range(args.apk_readers)]
        for future in futures:
            future.result()
        done.set()
        for future in observers:
            future.result()
    elapsed = time.monotonic() - start
    after = metrics(stand)
    return {**counts, 'elapsed_seconds': elapsed, 'duration_target_seconds': args.duration,
            'mib_per_second': counts['bytes'] / elapsed / (1024 * 1024),
            'equivalent_tb_per_day': counts['bytes'] / elapsed * 86400 / 1e12,
            'icon_requests_per_second': counts['icon_gets'] / elapsed,
            'latencies': {key: {'p50': percentile(values, .5), 'p99': percentile(values, .99),
                                'worst': max(values) if values else None, 'samples': len(values)} for key, values in samples.items()},
            'metric_before': before, 'metric_after': after, 'metric_samples': monitor,
            'memory_cpu_samples': memory, 'errors': errors,
            'notes': 'TLS, keep-alive, every 200 response SHA256-verified; conditional icon GETs; synthetic data; warm-cache workload'}


def main(args, report):
    for image_name in (args.image, args.baseline):
        if image_name:
            docker('image', 'inspect', image_name)
    with tempfile.TemporaryDirectory(prefix='birak-read-config-') as directory:
        directory = Path(directory)
        cert, key = directory / 'cert.pem', directory / 'key.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'ec', '-pkeyopt', 'ec_paramgen_curve:P-256',
                        '-noenc', '-days', '1', '-subj', '/CN=127.0.0.1', '-addext', 'subjectAltName=IP:127.0.0.1',
                        '-keyout', str(key), '-out', str(cert)], check=True, capture_output=True)
        key.chmod(0o644)  # Ephemeral test key read by the container's uid 1000.
        stand = Stand(args.baseline or args.image, directory)
        stand.tls = (cert, key)
        stand.ssl_context = ssl.create_default_context(cafile=str(cert))
        options = {'connect': False, 'peers': False, 'max_upload_bytes': 1024 * 1024 * 1024,
                   'scan_interval': '5m', 'scrub_rate': 8 * 1024 * 1024}
        try:
            stand.start(1, **options)
            stand.wait(lambda: stand.request(1, 'sync', 'GET', '/readyz')[0] == 200, 'empty seed readiness')
            docker('stop', stand.nodes[1]['name'])
            node = stand.nodes[1]
            seed_script = Path(__file__).resolve().with_name('seed_read_fixture.py')
            subprocess.run(['docker', 'run', '--rm', '--user', '1000:1000',
                            '-v', node['volume'] + ':/data/sync', '-v', node['meta_volume'] + ':/data/meta',
                            '-v', str(seed_script) + ':/seed.py:ro', 'python:3.14-alpine',
                            'python', '/seed.py', '--objects', str(args.objects)], check=True)
            fixture = json.loads(docker('run', '--rm', '-v', node['meta_volume'] + ':/meta:ro',
                                        'python:3.14-alpine', 'cat', '/meta/read-fixture.json'))
            report['fixture'] = {key: value for key, value in fixture.items() if key not in ('objects', 'cold')}
            images = [('baseline', args.baseline), ('candidate', args.image)] if args.baseline else [('candidate', args.image)]
            for phase, image_name in images:
                if phase == 'candidate' and args.baseline:
                    docker('rm', '--volumes', '-f', stand.nodes[1]['name'])
                    stand.image = image_name
                    stand.start(1, **options)
                else:
                    stand.restart(1)
                ready_start = time.monotonic()
                stand.wait(lambda: stand.request(1, 'sync', 'GET', '/readyz')[0] == 200, phase + ' full indexed scan', timeout=900)
                result = {'image': image_name, 'startup_scan_seconds': time.monotonic() - ready_start,
                          'tail_listing': tail_listing(stand, args.objects)}
                print(json.dumps({'phase': phase, **result}), flush=True)
                result['reads'] = exercise(stand, fixture, args)
                report[phase] = result
                assert not result['reads']['errors'], result['reads']['errors']
                if phase == 'candidate':
                    assert result['tail_listing']['complete_tail'], 'LIST omitted keys beyond 100000'
                    assert result['reads']['mib_per_second'] >= args.min_mib_per_second, 'measured egress below acceptance target'
                    if args.go_client:
                        assert result['reads']['apk_mib_per_second'] >= args.min_apk_mib_per_second, 'APK egress below acceptance target'
                print(json.dumps({'phase': phase, 'mib_per_second': result['reads']['mib_per_second'],
                                  'icon_p99': result['reads']['latencies']['icon_ttfb']['p99']}), flush=True)
                docker('stop', stand.nodes[1]['name'])
            report['status'] = 'passed'
        finally:
            stand.close(report.setdefault('daemon_logs', {}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--image', default='birak:read-load-candidate')
    parser.add_argument('--baseline')
    parser.add_argument('--objects', type=int, default=500000)
    parser.add_argument('--duration', type=int, default=60)
    parser.add_argument('--icon-readers', type=int, default=24)
    parser.add_argument('--apk-readers', type=int, default=8)
    parser.add_argument('--min-mib-per-second', type=float, default=120)
    parser.add_argument('--go-client', help='path to native scripts/readload binary')
    parser.add_argument('--client-network', choices=('host','docker'), default='host')
    parser.add_argument('--icon-rps', type=int, default=1000)
    parser.add_argument('--apk-mib-per-second', type=float, default=0)
    parser.add_argument('--min-apk-mib-per-second', type=float, default=120)
    parser.add_argument('--report', required=True)
    args = parser.parse_args()
    if args.duration < 10 or args.objects < 1001 or min(args.icon_readers, args.apk_readers) < 1:
        parser.error('duration >=10, objects >=1001 and positive reader counts required')
    report = {'status': 'failed'}
    try:
        main(args, report)
    except Exception as error:
        report['error'] = repr(error)
        raise
    finally:
        Path(args.report).write_text(json.dumps(report, indent=2))
