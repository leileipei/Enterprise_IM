"""Validate archive contents and discover compiled Go package/test inventories."""
import importlib.util
import json
from pathlib import Path
import re
import subprocess
from typing import Dict, Optional
from .model import Inventory, SourceSnapshot, Toolchain


def verify_snapshot(data: Dict[str,str]) -> SourceSnapshot:
    if not re.fullmatch('[a-f0-9]{40}',data['commit']):raise ValueError('invalid_source_commit')
    entry=Path(data['root'])/'scripts/test-project-integration.py'
    import hashlib
    if hashlib.sha256(entry.read_bytes()).hexdigest()!=data['entry_sha256']:
        raise ValueError('entry_hash_mismatch')
    spec=importlib.util.spec_from_file_location('_integration_stdlib_bootstrap',entry)
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    module.validate_export(data)
    if module.digest(entry)!=data['entry_sha256']:raise ValueError('entry_hash_mismatch')
    return SourceSnapshot(data['commit'],Path(data['repository_root']),Path(data['root']),
                          Path(data['archive']),data['archive_sha256'])


def _json_objects(text):
    decoder=json.JSONDecoder();offset=0
    while offset<len(text):
        while offset<len(text) and text[offset].isspace():offset+=1
        if offset==len(text):break
        value,end=decoder.raw_decode(text,offset)
        if not isinstance(value,dict):raise ValueError('invalid_inventory_object')
        yield value
        offset=end


def collect_inventory(snapshot: SourceSnapshot,tools: Toolchain,env: Dict[str,str],
                      selection: Optional[Dict[str,str]]) -> Inventory:
    go=str(tools.paths['go'])
    def command(args):
        try:
            result=subprocess.run([go,*args],cwd=snapshot.root,env=env,
                capture_output=True,text=True,timeout=1800)
        except (OSError,subprocess.TimeoutExpired) as exc:raise ValueError('inventory_command_failed') from exc
        if result.returncode:raise ValueError('inventory_command_nonzero')
        return result.stdout
    try:
        listed=list(_json_objects(command(['list','-json','./...'])))
        packages={p['ImportPath'] for p in listed}
        if not packages:raise ValueError('empty_package_inventory')
        if selection is not None and not set(selection)<=packages:raise ValueError('unknown_selected_package')
        chosen=packages if selection is None else set(selection)
        tests={p:set() for p in chosen}
        groups=[('./...', '.') ] if selection is None else sorted(selection.items())
        seen=set()
        for package,pattern in groups:
            for row in _json_objects(command(['test','-json','-list',pattern,'-p','1',package])):
                pkg=row.get('Package');action=row.get('Action')
                if pkg not in chosen:raise ValueError('unexpected_inventory_package')
                if action=='output':
                    for line in row.get('Output','').splitlines():
                        if re.fullmatch(r'Test[A-Za-z0-9_]+',line):tests[pkg].add(line)
                if action in {'pass','skip','fail'}:
                    if action=='fail':raise ValueError('failed_test_inventory')
                    if pkg in seen:raise ValueError('duplicate_inventory_package')
                    seen.add(pkg)
        if seen!=chosen:raise ValueError('missing_inventory_package')
        return Inventory(chosen,tests,set())
    except (KeyError,TypeError,json.JSONDecodeError) as exc:raise ValueError('invalid_inventory_json') from exc
