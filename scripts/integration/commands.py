"""Supervise commands with a deadline and preserve their actual exit status."""
import os
from pathlib import Path
import signal
import sys
import subprocess
import time
from typing import Dict, List
from .model import GateEvent, ResourceRef
from .registry import Registry, process_fingerprint
from .tools import python_cli


def run_command(name: str,kind: str,source_commit: str,argv: List[str],cwd: Path,
                env: Dict[str,str],log: Path,deadline: float,registry: Registry) -> GateEvent:
    if source_commit!=registry.commit:raise ValueError('command_source_mismatch')
    if time.monotonic()>=deadline:
        return GateEvent(name,kind,source_commit,124,log,failures=['command_deadline'])
    if argv and Path(argv[0]).resolve()==Path(sys.executable).resolve():
        argv=[str(python_cli()),*argv[1:]]
    log=Path(log);log.parent.mkdir(mode=0o700,parents=True,exist_ok=True)
    proc=None;fingerprint=None;failure=[]
    fd=os.open(log,os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
    with os.fdopen(fd,'wb') as stream:
        try:
            proc=subprocess.Popen(argv,cwd=cwd,env=env,stdout=stream,
                                  stderr=subprocess.STDOUT,start_new_session=True)
            fingerprint=process_fingerprint(proc.pid)
            if fingerprint:
                registry.add(ResourceRef('process',registry.owner,str(proc.pid),fingerprint))
            while proc.poll() is None:
                remaining=deadline-time.monotonic()
                if remaining<=0:
                    failure.append('command_deadline');break
                try:proc.wait(timeout=min(0.05,remaining))
                except subprocess.TimeoutExpired:pass
        finally:
            if proc and proc.poll() is None:
                actual=process_fingerprint(proc.pid)
                if actual and actual==fingerprint and actual['pgid']==proc.pid:
                    os.killpg(proc.pid,signal.SIGTERM)
                    try:proc.wait(timeout=1)
                    except subprocess.TimeoutExpired:
                        if process_fingerprint(proc.pid)==fingerprint:os.killpg(proc.pid,signal.SIGKILL)
                        proc.wait(timeout=1)
                else:
                    failure.append('command_identity_unproven')
            if proc and fingerprint:
                registry.lifecycle(str(proc.pid),'exited',{'exit_code':proc.returncode,'gate':name})
    rc=proc.returncode if proc and proc.returncode is not None else 125
    return GateEvent(name,kind,source_commit,rc,log,failures=failure)
