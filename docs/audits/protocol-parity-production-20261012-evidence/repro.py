import base64, concurrent.futures, datetime, hashlib, hmac, http.client, json, os, pathlib, socket, subprocess, time, urllib.parse, zlib

BASE = pathlib.Path(__file__).parent
RESULTS = []

def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]

def sign(method, path, body, headers, host):
    headers = {k.lower(): v for k, v in headers.items()}
    stamp = datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%SZ')
    date = stamp[:8]
    headers.update({'host': host, 'x-amz-date': stamp})
    headers.setdefault('x-amz-content-sha256', hashlib.sha256(body).hexdigest())
    names = sorted(headers)
    query = urllib.parse.urlsplit(path).query
    canonical_query = '&'.join(f'{urllib.parse.quote(k, safe="~-._")}={urllib.parse.quote(v, safe="~-._")}' for k, v in sorted(urllib.parse.parse_qsl(query, keep_blank_values=True)))
    canonical = '\n'.join([method, path.split('?')[0], canonical_query, ''.join(f'{k}:{headers[k]}\n' for k in names), ';'.join(names), headers['x-amz-content-sha256']])
    scope = f'{date}/us-east-1/s3/aws4_request'
    message = '\n'.join(['AWS4-HMAC-SHA256', stamp, scope, hashlib.sha256(canonical.encode()).hexdigest()])
    key = b'AWS4audit-secret'
    for v in [date, 'us-east-1', 's3', 'aws4_request']:
        key = hmac.new(key, v.encode(), hashlib.sha256).digest()
    signature = hmac.new(key, message.encode(), hashlib.sha256).hexdigest()
    headers['authorization'] = f'AWS4-HMAC-SHA256 Credential=audit-access/{scope}, SignedHeaders={";".join(names)}, Signature={signature}'
    return headers

def request(p, method, path, body=b'', headers=None, timeout=2, signed=False):
    host = f'127.0.0.1:{p}'
    headers = dict(headers or {})
    if signed: headers = sign(method, path, body, headers, host)
    c = http.client.HTTPConnection('127.0.0.1', p, timeout=timeout)
    try:
        c.request(method, path, body, headers)
        r = c.getresponse()
        return r.status, r.read()
    finally: c.close()

def run(name):
    directory = BASE / ('repro-' + name)
    root = directory / 'files'
    bucket = root / 'bucket'
    bucket.mkdir(parents=True, exist_ok=True)
    (bucket / 'regular.txt').write_bytes(b'ok')
    os.mkfifo(bucket / 'fifo')
    ports = {g: port() for g in ['s3','webdav','http','sftp']}
    prefix = name.upper()
    env = {k:v for k,v in os.environ.items() if not k.startswith(('BIRAK_', 'SEBASTIAN_'))}
    env.update({f'{prefix}_'+g.upper()+'_ENABLED':'true' for g in ports})
    env.update({f'{prefix}_'+g.upper()+'_LISTEN_ADDR':f'127.0.0.1:{p}' for g,p in ports.items()})
    env.update({f'{prefix}_S3_ACCESS_KEY':'audit-access', f'{prefix}_S3_SECRET_KEY':'audit-secret', f'{prefix}_SFTP_HOST_KEY_PATH':str(directory/'hostkey')})
    if name == 'birak':
        env.update({'BIRAK_SYNC_DIR':str(root), 'BIRAK_META_DIR':str(directory/'meta'), 'BIRAK_NODE_ID':'audit', 'BIRAK_LISTEN_ADDR':f'127.0.0.1:{port()}'})
    else: env['SEBASTIAN_ROOT_DIR']=str(root)
    log = (directory/'daemon.log').open('w')
    daemon = subprocess.Popen([str(BASE/(name+'d'))], env=env, stdout=log, stderr=log)
    try:
        for _ in range(100):
            if daemon.poll() is not None: raise RuntimeError('daemon exited: '+(directory/'daemon.log').read_text())
            try:
                if request(ports['s3'], 'GET', '/bucket/regular.txt', signed=True)[0] == 200: break
            except OSError: time.sleep(.1)
        else: raise RuntimeError('startup timed out')
        payload = b'hello'
        checksum = base64.b64encode(zlib.crc32(payload).to_bytes(4, 'big')).decode()
        wire = b'5\r\nhello\r\n0\r\nx-amz-checksum-crc32:' + checksum.encode() + b'\r\n\r\n'
        status, _ = request(ports['s3'], 'PUT', '/bucket/chunked.txt', wire, {'Content-Encoding':'aws-chunked', 'X-Amz-Content-Sha256':'STREAMING-UNSIGNED-PAYLOAD-TRAILER', 'X-Amz-Decoded-Content-Length':'5', 'X-Amz-Trailer':'x-amz-checksum-crc32'}, signed=True)
        get_status, got = request(ports['s3'], 'GET', '/bucket/chunked.txt', signed=True)
        RESULTS.append({'project':name,'scenario':'signed aws-chunked PutObject','put_status':status,'get_status':get_status,'expected_bytes':len(payload),'stored_bytes':len(got),'stores_encoded_body':got==wire,'body_matches_payload':got==payload})
        for gateway, path in [('s3','/bucket/fifo'), ('webdav','/bucket/fifo'), ('http','/_api/dl/bucket/fifo')]:
            started=time.monotonic()
            try:
                status,_=request(ports[gateway], 'GET', path, timeout=.5, signed=gateway=='s3')
                result={'status':status}
            except (TimeoutError, socket.timeout): result={'blocked':True}
            finally:
                fd=os.open(bucket/'fifo', os.O_RDWR|os.O_NONBLOCK)
                time.sleep(.1)
                os.close(fd)
            RESULTS.append({'project':name,'scenario':gateway+' GET FIFO','elapsed_seconds':round(time.monotonic()-started,3),**result})
        client=subprocess.Popen([str(BASE/'sftp-probe'),f'127.0.0.1:{ports["sftp"]}','/bucket/fifo'],stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True)
        print(name, client.stdout.readline().strip(), flush=True)
        time.sleep(.2)
        try:
            status,_=request(ports['s3'],'PUT','/bucket/after-sftp.txt',b'normal write',timeout=.5,signed=True)
            write_result={'status':status}
        except (TimeoutError,socket.timeout): write_result={'blocked':True}
        finally:
            fd=os.open(bucket/'fifo',os.O_RDWR|os.O_NONBLOCK)
            stdout,_=client.communicate(timeout=5)
            time.sleep(.1)
            os.close(fd)
        RESULTS.append({'project':name,'scenario':'S3 PUT while SFTP OPEN FIFO','sftp_output':stdout.strip(),**write_result})
        if name=='sebastian':
            large=root/'large-bucket'
            large.mkdir()
            for i in range(100001): (large/f'{i:06}.txt').touch()
            status,xml=request(ports['s3'],'GET','/large-bucket?list-type=2&start-after=099999.txt&max-keys=10',timeout=30,signed=True)
            RESULTS.append({'project':name,'scenario':'LIST after first 100000 files','physical_files':100001,'status':status,'response':xml.decode()})
    finally:
        daemon.terminate()
        try: daemon.wait(timeout=12)
        except subprocess.TimeoutExpired:
            daemon.kill(); daemon.wait()
        log.close()

if __name__ == '__main__':
    for name in ['sebastian','birak']: run(name)
    (BASE/'repro-results.json').write_text(json.dumps(RESULTS,ensure_ascii=False,indent=2))
    print(json.dumps(RESULTS,ensure_ascii=False,indent=2))
