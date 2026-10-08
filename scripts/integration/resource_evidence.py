"""Strict nonsecret Linux build/container receipts, also checked at delivery."""
import hashlib
import json
import math
import os
from pathlib import Path
import re
import tempfile

BINARIES={'structure.test':'./internal/filescanner','transfer.test':'./internal/filetransfer'}
RESOURCE_FILES=('resource.log','resource-list.log','resource.log.proof.json','resource-list.log.proof.json',
                'disk-full.log','disk-full-list.log','disk-full.log.proof.json','disk-full-list.log.proof.json','resource-build.json')
PROOF_FIELDS={'source_commit','owner','exit_code','cleanup','container_id','image_id','memory','nano_cpus','tmpfs',
              'binary_name','binary_sha256','started_at','ended_at','seconds'}


def digest(path):return hashlib.sha256(Path(path).read_bytes()).hexdigest()


def validate_build(data,source,owner,root,verify_binaries):
    fields={'schema_version','source_commit','owner','goos','goarch','cgo_enabled','go_flags','go_work','binaries'}
    if (not isinstance(data,dict) or set(data)!=fields or data['schema_version']!='file_resource_build_v1' or
        data['source_commit']!=source or data['owner']!=owner or not re.fullmatch('[a-f0-9]{32}',owner) or
        data['goos']!='linux' or data['goarch'] not in {'arm64','amd64'} or data['cgo_enabled']!='0' or
        data['go_flags']!='-mod=readonly -buildvcs=false' or data['go_work']!='off'):
        raise ValueError('resource_build_identity_invalid')
    rows=data['binaries']
    if not isinstance(rows,list) or len(rows)!=2:raise ValueError('resource_binary_count_invalid')
    by_name={}
    for row in rows:
        if not isinstance(row,dict) or set(row)!={'name','path','sha256','package','argv'}:raise ValueError('resource_binary_fields_invalid')
        name=row['name'];path=Path(row['path'])
        if (name not in BINARIES or name in by_name or path!=root/name or not path.is_absolute() or
            row['package']!=BINARIES[name] or row['argv']!=['go','test','-c','-o',str(path),BINARIES[name]] or
            not re.fullmatch('[a-f0-9]{64}',str(row['sha256']))):raise ValueError('resource_binary_build_invalid')
        if verify_binaries:
            if not path.is_file() or any(p.is_symlink() for p in (path,*path.parents)) or path.stat().st_uid!=os.getuid() or digest(path)!=row['sha256']:
                raise ValueError('resource_binary_hash_mismatch')
        by_name[name]=dict(row,source_commit=source,goos=data['goos'],goarch=data['goarch'],cgo_enabled='0',go_flags=data['go_flags'],go_work='off')
    return by_name


def validate_proof(data,source,owner,image_id,binary,disk):
    if (not isinstance(data,dict) or set(data)!=PROOF_FIELDS or data['source_commit']!=source or data['owner']!=owner or
        data['exit_code']!=0 or type(data['exit_code']) is not int or data['cleanup'] is not True or
        data['memory']!=536870912 or data['nano_cpus']!=1000000000 or
        not re.fullmatch('[a-f0-9]{64}',str(data['container_id'])) or
        not re.fullmatch('sha256:[a-f0-9]{64}',str(data['image_id'])) or image_id and data['image_id']!=image_id or
        data['binary_name']!=binary['name'] or data['binary_sha256']!=binary['sha256']):
        raise ValueError('resource_limits_or_identity_unproven')
    expected={'/limited':'size=1048576,mode=0700'} if disk else {}
    if data['tmpfs']!=expected:raise ValueError('resource_tmpfs_unproven')
    a,b,seconds=(data[k] for k in ('started_at','ended_at','seconds'))
    if (any(type(v) not in (float,int) or not math.isfinite(v) for v in (a,b,seconds)) or a<=0 or b<a or
        seconds<0 or abs(seconds-round(b-a,3))>.002):raise ValueError('resource_time_unproven')


def validate_delivery_resources(data,report_root):
    event=next((e for e in data['events'] if e['name']=='file_resources'),{})
    evidence=event.get('resource_evidence',{})
    if not isinstance(evidence,dict) or set(evidence)!={'artifacts','binaries','owner','image_id'}:
        raise ValueError('delivery_resource_evidence_missing')
    artifacts=evidence['artifacts']
    if not isinstance(artifacts,list) or len(artifacts)!=len(RESOURCE_FILES):raise ValueError('delivery_resource_artifacts_missing')
    names={x.get('name') for x in artifacts if isinstance(x,dict)}
    if names!=set(RESOURCE_FILES):raise ValueError('delivery_resource_artifacts_invalid')
    by_original={r['original_path']:r for r in data['log_dispositions']}
    original_root=None;contents={}
    for item in artifacts:
        if set(item)!={'name','original_path','sha256'}:raise ValueError('delivery_resource_artifact_fields')
        original=Path(item['original_path'])
        if not original.is_absolute() or '..' in original.parts or original.name!=item['name']:raise ValueError('delivery_resource_artifact_path')
        if original_root is None:original_root=original.parent
        if original.parent!=original_root:raise ValueError('delivery_resource_artifacts_not_grouped')
        row=by_original.get(str(original))
        if not row or row['published_sha256']!=item['sha256'] or row['original_sha256']!=item['sha256']:
            raise ValueError('delivery_resource_artifact_hash')
        path=report_root/row['published_path']
        if digest(path)!=item['sha256']:raise ValueError('delivery_resource_artifact_changed')
        contents[item['name']]=path.read_text()
    expected_image=data['tools']['images'].get('alpine-resource-image',{}).get('image_id')
    if not expected_image or evidence['image_id']!=expected_image:raise ValueError('delivery_resource_image_mismatch')
    owner=evidence['owner']
    registered={(r['identity'],r['owner'],r['source_commit']) for r in data['resource_records'] if r['kind']=='container' and r['event']=='registered'}
    for name in RESOURCE_FILES:
        if name.endswith('.proof.json'):
            proof=json.loads(contents[name])
            if (proof.get('container_id'),owner,data['source_commit']) not in registered:raise ValueError('delivery_resource_container_not_registered')
    from .gates import parse_resource_logs
    with tempfile.TemporaryDirectory(prefix='p431-resource-delivery-') as value:
        root=Path(value).resolve();root.chmod(0o700)
        for name,text in contents.items():
            p=root/name;p.write_text(text);p.chmod(0o600)
        parsed=parse_resource_logs(root,data['source_commit'],0,owner,expected_image,verify_binaries=False,binary_root=original_root)
    if parsed.failures or parsed.skips:raise ValueError('delivery_resource_revalidation_failed')
    for key in ('counts','package_status'):
        if getattr(parsed,key)!=event[key]:raise ValueError('delivery_resource_counts_changed')
    for key in ('passed','executed','started'):
        if getattr(parsed,key)!=set(event[key]):raise ValueError('delivery_resource_names_changed')
    if parsed.resource_evidence['binaries']!=evidence['binaries']:raise ValueError('delivery_resource_binary_receipt_changed')
    registered_hashes={(r['path'],r['sha256'],r['source_commit']) for r in data['binary_hashes']}
    if any((b['path'],b['sha256'],b['source_commit']) not in registered_hashes for b in evidence['binaries']):
        raise ValueError('delivery_resource_binary_hash_missing')
