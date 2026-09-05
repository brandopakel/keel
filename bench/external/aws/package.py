#!/usr/bin/env python3
"""Build an AWS DLT Locust upload archive. Does not provision or upload."""
import argparse,json,zipfile,hashlib
from pathlib import Path
p=argparse.ArgumentParser();p.add_argument('--host',required=True);p.add_argument('--port',type=int,default=6379);p.add_argument('--tls',action='store_true');p.add_argument('--out',required=True);a=p.parse_args()
if not a.host or any(c in a.host for c in '/@\r\n') or not 1<=a.port<=65535:p.error('invalid hostname/port; credentials belong in runtime environment')
out=Path(a.out);out.parent.mkdir(parents=True,exist_ok=True)
with zipfile.ZipFile(out,'w',zipfile.ZIP_DEFLATED) as archive:
    for name in ['locustfile.py','resp_client.py']:
        archive.writestr(name,Path(__file__).with_name(name).read_bytes())
    archive.writestr('target.json',json.dumps({'host':a.host,'port':a.port,'tls':a.tls}))
out.with_suffix('.sha256').write_text(hashlib.sha256(out.read_bytes()).hexdigest()+'  '+out.name+'\n')
print(out)
