#!/usr/bin/env python3
"""Isolated, disposable four-daemon filesystem acceptance stand.

Build the image first, then run this script. All data is in newly created Docker
volumes; the script never mounts production directories. HTTP is bound to localhost.
"""
import argparse
import concurrent.futures
import hashlib
import hmac
import json
import os
from pathlib import Path
import random
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid


def docker(*args, check=True):
    result = subprocess.run(["docker", *args], text=True, capture_output=True)
    if check and result.returncode:
        raise RuntimeError(f"docker {args[0]} failed ({result.returncode}): {result.stderr.strip()}")
    return result.stdout.strip()


class Stand:
    def __init__(self, image, directory):
        self.image, self.directory = image, Path(directory)
        self.prefix = "birak-fs-" + uuid.uuid4().hex[:10]
        self.nodes, self.volumes, self.networks = {}, [], []
        self.secret = uuid.uuid4().hex
        self.latencies, self.failures = [], 0
        self.guard = threading.Lock()
        self.control, self.cluster = self.prefix + "-control", self.prefix + "-cluster"
        for network in (self.control, self.cluster):
            docker("network", "create", network)
            self.networks.append(network)

    def start(self, index, connect=True, peers=True, max_upload_bytes=134217728,
              scan_interval="1s", scrub_rate=67108864):
        name, volume = f"{self.prefix}-n{index}", f"{self.prefix}-data{index}"
        meta_volume = f"{self.prefix}-meta{index}"
        for owned in (volume, meta_volume):
            docker("volume", "create", owned)
            if owned not in self.volumes:
                self.volumes.append(owned)
        # Match both declared Dockerfile VOLUME targets explicitly. A parent
        # /data mount would be hidden by anonymous /data/sync and /data/meta.
        docker("run", "--rm", "-v", volume + ":/data/sync", "-v", meta_volume + ":/data/meta",
               "python:3.14-alpine", "python", "-c",
               "import os; os.chown('/data/sync',1000,1000); os.chown('/data/meta',1000,1000)")
        cfg = self.directory / f"n{index}.yaml"
        cfg.write_text("\n".join([
            f"node_id: n{index}", "sync_dir: /data/sync", "meta_dir: /data/meta", "log_level: warn",
            "listen_addr: ':9100'", f"cluster_secret: {self.secret}", f"max_upload_bytes: {max_upload_bytes}",
            *(["peers:", *[f"  - http://n{other}-peer:9100" for other in range(1, 5) if other != index]] if peers else ["peers: []"]),
            "sync:", "  poll_interval: 100ms", "  debounce_window: 20ms", f"  scan_interval: {scan_interval}",
            "  repair_interval: 100ms", "  reconcile_interval: 1s", "  max_concurrent_downloads: 4",
            f"  scrub_bytes_per_second: {scrub_rate}", "gateways:", "  s3:", "    enabled: true",
            "    listen_addr: ':9200'", "    access_key: test", "    secret_key: docker-acceptance-only",
        ]) + "\n")
        tls = getattr(self, "tls", None)
        if tls:
            with cfg.open("a") as f:
                f.write("    tls_cert_file: /tls-cert.pem\n    tls_key_file: /tls-key.pem\n")
        docker("run", "-d", "--name", name, "--network", self.control,
               "-p", "127.0.0.1::9100", "-p", "127.0.0.1::9200",
               "-v", volume + ":/data/sync", "-v", meta_volume + ":/data/meta", "-v", str(cfg) + ":/config.yaml:ro",
               *(["-v", str(tls[0]) + ":/tls-cert.pem:ro", "-v", str(tls[1]) + ":/tls-key.pem:ro"] if tls else []),
               self.image, "-config", "/config.yaml")
        ports = json.loads(docker("inspect", name))[0]["NetworkSettings"]["Ports"]
        self.nodes[index] = {"name": name, "volume": volume, "meta_volume": meta_volume,
                             "sync": "http://127.0.0.1:" + ports["9100/tcp"][0]["HostPort"],
                             "s3": ("https" if tls else "http") + "://127.0.0.1:" + ports["9200/tcp"][0]["HostPort"]}
        if connect:
            self.connect(index)
        self.wait(lambda: self.request(index, "sync", "GET", "/healthz")[0] == 200, "process liveness")

    def connect(self, index):
        docker("network", "connect", "--alias", f"n{index}-peer", self.cluster, self.nodes[index]["name"])
        self.refresh_endpoints(index)

    def restart(self, index):
        docker("start", self.nodes[index]["name"])
        self.refresh_endpoints(index)

    def refresh_endpoints(self, index):
        # Some Linux engines reallocate dynamically published ports when the
        # default bridge changes. Read the current bindings after every network
        # mutation, not only before attaching the replication network.
        ports = json.loads(docker("inspect", self.nodes[index]["name"]))[0]["NetworkSettings"]["Ports"]
        for kind, port in (("sync", "9100/tcp"), ("s3", "9200/tcp")):
            scheme = "https" if kind == "s3" and getattr(self, "tls", None) else "http"
            current = scheme + "://127.0.0.1:" + ports[port][0]["HostPort"]
            if current != self.nodes[index][kind]:
                print(json.dumps({"event": "docker_port_remapped", "node": index,
                      "kind": kind, "before": self.nodes[index][kind], "after": current}), flush=True)
            self.nodes[index][kind] = current

    def disconnect(self, index):
        docker("network", "disconnect", self.cluster, self.nodes[index]["name"])
        self.refresh_endpoints(index)

    def headers(self, index, kind, method, path, body=b"", payload_hash=None):
        endpoint = self.nodes[index][kind]
        headers = {}
        if kind == "sync":
            headers["X-Birak-Secret"] = self.secret
        else:
            stamp = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
            day, host = stamp[:8], urllib.parse.urlsplit(endpoint).netloc
            digest = payload_hash or hashlib.sha256(body).hexdigest()
            signed = "host;x-amz-content-sha256;x-amz-date"
            parsed = urllib.parse.urlsplit(path)
            query = urllib.parse.urlencode(sorted(urllib.parse.parse_qsl(parsed.query, keep_blank_values=True)),
                                           quote_via=urllib.parse.quote, safe="~-._")
            canonical = f"{method}\n{parsed.path}\n{query}\nhost:{host}\nx-amz-content-sha256:{digest}\nx-amz-date:{stamp}\n\n{signed}\n{digest}"
            scope = day + "/us-east-1/s3/aws4_request"
            message = "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hashlib.sha256(canonical.encode()).hexdigest()
            key = b"AWS4docker-acceptance-only"
            for part in (day, "us-east-1", "s3", "aws4_request"):
                key = hmac.new(key, part.encode(), hashlib.sha256).digest()
            signature = hmac.new(key, message.encode(), hashlib.sha256).hexdigest()
            headers.update({"X-Amz-Date": stamp, "X-Amz-Content-Sha256": digest,
                            "Authorization": f"AWS4-HMAC-SHA256 Credential=test/{scope}, SignedHeaders={signed}, Signature={signature}"})
        return headers

    def request(self, index, kind, method, path, body=b""):
        endpoint = self.nodes[index][kind]
        headers = self.headers(index, kind, method, path, body)
        request = urllib.request.Request(endpoint + path, body if method in ("PUT", "POST") else None,
                                         headers=headers, method=method)
        try:
            with urllib.request.urlopen(request, timeout=15, context=getattr(self, "ssl_context", None)) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()

    def put(self, node, key, body):
        start = time.monotonic()
        status, response = self.request(node, "s3", "PUT", "/bucket/" + key, body)
        if status != 200:
            raise AssertionError(f"PUT node={node} key={key}: {status} {response!r}")
        with self.guard:
            self.latencies.append(time.monotonic() - start)

    def wait(self, predicate, label, timeout=120):
        deadline, last = time.monotonic() + timeout, None
        while time.monotonic() < deadline:
            try:
                if predicate():
                    return
            except (OSError, urllib.error.URLError, ValueError) as error:
                last = error
            time.sleep(0.1)
        raise AssertionError(f"timeout: {label}; last error={last}")

    def manifest(self, index):
        records, after = [], ""
        while True:
            status, body = self.request(index, "sync", "GET", "/manifest?limit=1000&after=" + urllib.parse.quote(after))
            if status != 200:
                raise AssertionError(f"manifest {index}: {status}")
            page = json.loads(body)
            if not page:
                return records
            for entry in page:
                entry.pop("version", None)
            records.extend(page)
            after = page[-1]["name"]

    def converged(self):
        manifests = [self.manifest(index) for index in self.nodes]
        return all(manifest == manifests[0] for manifest in manifests[1:]) and all(
            json.loads(self.request(index, "sync", "GET", "/status")[1])["repairs"]["total"] == 0
            for index in self.nodes)

    def checkpoint(self, index, destination):
        node = self.nodes[index]
        docker("run", "--rm", "-v", node["volume"] + ":/source/sync:ro",
               "-v", node["meta_volume"] + ":/source/meta:ro", "-v", destination + ":/backup",
               "python:3.14-alpine", "python", "-c",
               "import shutil; from pathlib import Path; "
               "assert Path('/source/sync/bucket/doomed').read_bytes()==b'must remain deleted after restoring old data'; "
               "assert Path('/source/meta/birak.db').is_file(); "
               "shutil.copytree('/source','/backup',dirs_exist_ok=True)")

    def close(self, logs):
        for index, node in self.nodes.items():
            logs[f"n{index}"] = docker("logs", "--tail", "100", node["name"], check=False)
            raw = docker("inspect", node["name"], check=False)
            if raw:
                info = json.loads(raw)[0]
                logs[f"n{index}-state"] = json.dumps({"state": info["State"],
                    "ports": info["NetworkSettings"]["Ports"], "sync": node["sync"], "s3": node["s3"]})
            docker("rm", "--volumes", "-f", node["name"], check=False)
        for volume in self.volumes:
            docker("volume", "rm", volume, check=False)
        for network in reversed(self.networks):
            docker("network", "rm", network, check=False)


def run(args, report):
    with tempfile.TemporaryDirectory(prefix="birak-docker-config-") as directory:
        stand = Stand(args.image, directory)
        started = time.monotonic()
        try:
            for index in (1, 2, 3):
                stand.start(index, max_upload_bytes=max(134217728, args.large_mib * 1024 * 1024 * 2))
            for index in stand.nodes:
                stand.wait(lambda i=index: stand.request(i, "sync", "GET", "/readyz")[0] == 200, "initial admission")
            status, body = stand.request(1, "s3", "PUT", "/bucket")
            assert status == 200, (status, body)
            stand.put(1, "doomed", b"must remain deleted after restoring old data")
            stand.put(1, "overwrite", b"old version")
            for i in range(args.objects):
                stand.put(1, f"seed-{i:06d}", hashlib.sha256(str(i).encode()).digest() * 256)
            stand.wait(stand.converged, "initial replication")
            print("initial replication verified", flush=True)

            # Save a consistent old node while stopped, on a separate owned volume.
            docker("stop", stand.nodes[2]["name"])
            backup = stand.prefix + "-backup"
            docker("volume", "create", backup)
            stand.volumes.append(backup)
            stand.checkpoint(2, backup)
            stand.restart(2)
            stand.wait(lambda: stand.request(2, "sync", "GET", "/readyz")[0] == 200, "restart")

            stand.disconnect(3)
            assert stand.request(3, "sync", "GET", "/readyz")[0] == 200, "partition revoked admission"
            stand.put(1, "shared", b"majority-side write")
            stand.put(3, "shared", b"isolated-node write")
            assert stand.request(1, "s3", "DELETE", "/bucket/doomed")[0] == 204
            stand.put(1, "overwrite", b"new version")

            deadline = time.monotonic() + args.duration
            stress_latency_start = len(stand.latencies)
            counts = [0] * args.writers
            def writer(worker):
                rng, serial = random.Random(worker), 0
                while time.monotonic() < deadline:
                    node = rng.choice((1, 2, 3))
                    key = f"load-{worker:02d}-{serial:06d}"
                    body = hashlib.sha256(key.encode()).digest() * 256
                    stand.put(node, key, body)
                    if serial % 5 == 0:
                        status, _ = stand.request(node, "s3", "DELETE", "/bucket/" + key)
                        assert status == 204, status
                    serial += 1
                counts[worker] = serial
            with concurrent.futures.ThreadPoolExecutor(max_workers=args.writers) as pool:
                list(pool.map(writer, range(args.writers)))
            stress_latencies = list(stand.latencies[stress_latency_start:])
            # Large-object transfers while one node is isolated.
            large = b"large-object-verified\n" * (args.large_mib * 1024 * 1024 // 22)
            stand.put(3, "large-isolated", large)
            assert stand.request(3, "s3", "GET", "/bucket/large-isolated")[1] == large
            stand.connect(3)
            healed = time.monotonic()
            stand.wait(stand.converged, "partition convergence", timeout=300)
            report["partition_convergence_seconds"] = time.monotonic() - healed
            print("partition stress converged", flush=True)

            # Verify real independent bytes, not just equality of metadata.
            winner = stand.manifest(1)
            for record in winner:
                if record["deleted"]:
                    continue
                path = "/" + urllib.parse.quote(record["name"], safe="/")
                for node in (1, 2, 3):
                    status, body = stand.request(node, "s3", "GET", path)
                    assert status == 200 and hashlib.sha256(body).hexdigest() == record["hash"], (node, record)
            print("every live blob independently verified on three replicas", flush=True)

            stand.put(1, "App.apk", b"case-sensitive original")
            stand.wait(stand.converged, "case baseline")
            status, _ = stand.request(2, "s3", "PUT", "/bucket/app.apk", b"must be refused")
            assert status >= 400, "case alias was acknowledged"
            for node in (1, 2, 3):
                assert stand.request(node, "s3", "GET", "/bucket/App.apk")[1] == b"case-sensitive original"

            # An empty fourth instance must stay outside the LB until it catches up.
            stand.start(4, connect=False, max_upload_bytes=max(134217728, args.large_mib * 1024 * 1024 * 2))
            assert stand.request(4, "sync", "GET", "/readyz")[0] == 503, "cold node admitted early"
            docker("update", "--cpus", "0.5", stand.nodes[4]["name"])
            stand.connect(4)
            def partial_clone():
                status = json.loads(stand.request(4, "sync", "GET", "/status")[1])
                return status["file_count"] > 0 and not status["local"]["replica_initialized"]
            stand.wait(partial_clone, "partial fresh-node catch-up")
            stand.disconnect(4)
            docker("kill", "--signal", "KILL", stand.nodes[4]["name"])
            stand.restart(4)
            stand.wait(lambda: json.loads(stand.request(4, "sync", "GET", "/status")[1])["local"]["last_scan_ms_ago"] >= 0,
                       "partial-clone local scan")
            assert stand.request(4, "sync", "GET", "/readyz")[0] == 503, "partial restart bypassed admission"
            docker("update", "--cpus", "0", stand.nodes[4]["name"])
            stand.connect(4)
            stand.wait(lambda: stand.request(4, "sync", "GET", "/readyz")[0] == 200, "new-node admission", timeout=300)
            stand.wait(stand.converged, "four-node convergence", timeout=300)
            print("partial-clone restart and four-node admission verified", flush=True)

            # ACKed bytes survive actual daemon SIGKILL and process restart.
            docker("kill", "--signal", "KILL", stand.nodes[3]["name"])
            stand.restart(3)
            stand.wait(lambda: stand.request(3, "sync", "GET", "/readyz")[0] == 200, "SIGKILL restart")
            stand.wait(stand.converged, "SIGKILL convergence", timeout=300)

            # Roll back node 2, deliberately losing all data modification times.
            docker("stop", stand.nodes[2]["name"])
            docker("run", "--rm", "-v", backup + ":/backup:ro",
                   "-v", stand.nodes[2]["volume"] + ":/data/sync",
                   "-v", stand.nodes[2]["meta_volume"] + ":/data/meta", "python:3.14-alpine", "python", "-c",
                   "import os,shutil\nfrom pathlib import Path\n"
                   "for name in ('sync','meta'):\n"
                   " root=Path('/data')/name\n"
                   " for p in root.iterdir():\n"
                   "  if p.is_dir(): shutil.rmtree(p)\n"
                   "  else: p.unlink()\n"
                   " shutil.copytree(Path('/backup')/name,root,dirs_exist_ok=True)\n"
                   " for p in [root,*root.rglob('*')]: os.chown(p,1000,1000)\n"
                   "for p in Path('/data/sync').rglob('*'):\n"
                   " if p.is_file(): os.utime(p,ns=(1893456000000000000,1893456000000000000))\n"
                   "assert Path('/data/sync/bucket/doomed').read_bytes()==b'must remain deleted after restoring old data'\n"
                   "assert Path('/data/sync/bucket/overwrite').read_bytes()==b'old version'\n")
            stand.restart(2)
            stand.wait(stand.converged, "old backup with lost timestamps", timeout=300)
            for node in stand.nodes:
                assert stand.request(node, "s3", "GET", "/bucket/doomed")[0] == 404, "restore resurrected deletion"
                assert stand.request(node, "s3", "GET", "/bucket/overwrite")[1] == b"new version", "restore replaced current object"
            print("old backup with lost timestamps verified", flush=True)

            # Real bitrot injection: change bytes while retaining size and mtime.
            stand.put(1, "corruption", b"healthy!" * 8)
            stand.wait(stand.converged, "corruption baseline")
            # BusyBox cp/touch discard nanoseconds, which would turn the fault
            # into a legitimate changed-mtime write. Preserve exact st_mtime_ns.
            # Pause only this daemon while editing its own test volume so its
            # watcher never observes the intermediate changed timestamp.
            docker("pause", stand.nodes[3]["name"])
            try:
                docker("run", "--rm", "-v", stand.nodes[3]["volume"] + ":/data/sync", "python:3.14-alpine",
                       "python", "-c",
                       "import os; p='/data/sync/bucket/corruption'; f=os.open(p,os.O_RDWR); "
                       "s=os.fstat(f); os.pwrite(f,bytes(s.st_size),0); "
                       "os.utime(f,ns=(s.st_atime_ns,s.st_mtime_ns)); os.fsync(f); "
                       "assert os.fstat(f).st_mtime_ns==s.st_mtime_ns; os.close(f)")
            finally:
                docker("unpause", stand.nodes[3]["name"])
            stand.wait(lambda: stand.request(3, "s3", "GET", "/bucket/corruption") == (200, b"healthy!" * 8),
                       "integrity detection + equal-state repair", timeout=300)
            stand.wait(stand.converged, "post-corruption convergence", timeout=300)
            print("same-size same-mtime corruption repaired", flush=True)
            report["docker_memory_snapshot"] = docker("stats", "--no-stream", "--format", "{{.Name}} {{.MemUsage}}",
                                                       *[n["name"] for n in stand.nodes.values()])

            ordered = sorted(stress_latencies)
            report.update({"status": "passed", "nodes": 4, "seed_objects": args.objects,
                           "concurrent_writers": args.writers, "stress_duration_seconds": args.duration,
                           "stress_puts": sum(counts), "verified_live_objects": len([r for r in winner if not r["deleted"]]),
                           "put_p50_seconds": ordered[len(ordered) // 2], "put_p99_seconds": ordered[int(len(ordered) * .99)],
                           "latency_scope": "stress PUT requests only",
                           "elapsed_seconds": time.monotonic() - started,
                           "checks": ["independent SHA256 reads", "isolated-node ACKs", "partition convergence", "case collision refusal",
                                      "cold node admission", "partial-clone SIGKILL + admission", "SIGKILL restart",
                                      "old backup + lost timestamps", "no resurrection", "bitrot detection + repair"]})
        finally:
            stand.close(report.setdefault("logs", {}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", default="birak:filesystem-production-test")
    parser.add_argument("--objects", type=int, default=500)
    parser.add_argument("--writers", type=int, default=8)
    parser.add_argument("--duration", type=int, default=30)
    parser.add_argument("--large-mib", type=int, default=16)
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    if min(args.objects, args.writers, args.duration, args.large_mib) <= 0:
        parser.error("objects, writers, duration and large-mib must all be positive")
    report = {"status": "failed", "image": args.image}
    try:
        run(args, report)
    except BaseException as error:
        report["error"] = repr(error)
        raise
    finally:
        Path(args.report).write_text(json.dumps(report, indent=2))
        print(json.dumps({k: v for k, v in report.items() if k != "logs"}, indent=2), flush=True)
