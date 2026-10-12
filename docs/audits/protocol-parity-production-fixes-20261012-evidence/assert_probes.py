import json, pathlib, sys, xml.etree.ElementTree as ET
root=pathlib.Path(sys.argv[1])
rows=json.loads((root/'repro-results.json').read_text())
for row in rows:
    scenario=row['scenario']
    if scenario=='signed aws-chunked PutObject':
        assert row['put_status']==501 and not row['stores_encoded_body'],row
    elif scenario.endswith('GET FIFO'):
        assert not row.get('blocked') and row['status']>=400,row
    elif scenario=='S3 PUT while SFTP OPEN FIFO':
        assert row.get('status')==200 and not row.get('blocked'),row
    elif scenario=='LIST after first 100000 files':
        xml=ET.fromstring(row['response']);ns={'s':'http://s3.amazonaws.com/doc/2006-03-01/'}
        assert row['status']==200 and xml.find('s:Contents/s:Key',ns).text=='100000.txt',row
        assert xml.find('s:IsTruncated',ns).text=='false',row
for row in json.loads((root/'volume-results.json').read_text()):
    if row['scenario']=='root replaced after startup':
        assert row['status']>=500 and not row['published_on_replacement_volume'] and row['original_preserved'],row
    else:
        assert not row['signature_logged'],row
print('All 15 daemon probe observations passed acceptance assertions')
