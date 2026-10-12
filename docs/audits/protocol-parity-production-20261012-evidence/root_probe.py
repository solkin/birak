from repro import *
results=[]
for name in ['sebastian','birak']:
    directory=BASE/('volume-'+name);root=directory/'files';(root/'bucket').mkdir(parents=True)
    (root/'bucket'/'old.txt').write_bytes(b'old volume')
    p=port();prefix=name.upper();env={k:v for k,v in os.environ.items() if not k.startswith(('BIRAK_','SEBASTIAN_'))}
    env.update({f'{prefix}_S3_ENABLED':'true',f'{prefix}_S3_LISTEN_ADDR':f'127.0.0.1:{p}',f'{prefix}_S3_ACCESS_KEY':'audit-access',f'{prefix}_S3_SECRET_KEY':'audit-secret'})
    if name=='birak':env.update({'BIRAK_SYNC_DIR':str(root),'BIRAK_META_DIR':str(directory/'meta'),'BIRAK_NODE_ID':'volume-audit','BIRAK_LISTEN_ADDR':f'127.0.0.1:{port()}'})
    else:env['SEBASTIAN_ROOT_DIR']=str(root)
    log=(directory/'daemon.log').open('w');daemon=subprocess.Popen([str(BASE/(name+'d'))],env=env,stdout=log,stderr=log)
    try:
        for _ in range(100):
            try:
                if request(p,'GET','/bucket/old.txt',signed=True)[0]==200:break
            except OSError:time.sleep(.1)
        else:raise RuntimeError('startup')
        root.rename(directory/'original-volume');(root/'bucket').mkdir(parents=True)
        status,body=request(p,'PUT','/bucket/new.txt',b'new acknowledged bytes',signed=True)
        results.append({'project':name,'scenario':'root replaced after startup','status':status,'published_on_replacement_volume':(root/'bucket'/'new.txt').exists(),'original_preserved':(directory/'original-volume'/'bucket'/'old.txt').read_bytes()==b'old volume','response':body.decode()})
        request(p,'GET','/bucket/new.txt?X-Amz-Signature=audit-bearer-token&X-Amz-Credential=audit-access',signed=False)
    finally:
        daemon.terminate()
        try:daemon.wait(timeout=12)
        except subprocess.TimeoutExpired:daemon.kill();daemon.wait()
        log.close()
    results.append({'project':name,'scenario':'request query logging','signature_logged':'audit-bearer-token' in (directory/'daemon.log').read_text()})
(BASE/'volume-results.json').write_text(json.dumps(results,ensure_ascii=False,indent=2));print(json.dumps(results,ensure_ascii=False,indent=2))
