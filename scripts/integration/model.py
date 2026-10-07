"""Evidence records shared by the integration runner and its adapters."""
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional, Set


@dataclass
class SourceSnapshot:
    commit: str
    repository_root: Path
    root: Path
    archive: Path
    archive_sha256: str


@dataclass
class Toolchain:
    paths: Dict[str, Path]
    versions: Dict[str, str]
    hashes: Dict[str, str]
    images: Dict[str, Dict[str, str]]
    goos: str
    goarch: str


@dataclass
class Inventory:
    packages: Set[str]
    tests: Dict[str, Set[str]]
    required_subtests: Set[str]


@dataclass
class GateEvent:
    name: str
    kind: str
    source_commit: str
    exit_code: int
    log: Path
    counts: Dict[str, int] = field(default_factory=dict)
    executed: Set[str] = field(default_factory=set)
    failures: List[str] = field(default_factory=list)
    skips: List[str] = field(default_factory=list)
    checks: List[str] = field(default_factory=list)
    helper_proofs: List[dict] = field(default_factory=list)
    passed: Set[str] = field(default_factory=set)
    package_status: Dict[str, str] = field(default_factory=dict)
    started: Set[str] = field(default_factory=set)
    inventory: Optional[Inventory] = None


@dataclass
class ResourceRef:
    kind: str
    owner: str
    identity: str
    fingerprint: dict = field(repr=False)
    state: str = 'registered'


@dataclass
class CleanupResult:
    removed: bool
    failures: List[str]


@dataclass(repr=False)
class FixtureBundle:
    environment: Dict[str, str]
    secrets: Set[str]
    registry_path: Path
    owners: Set[str]
    metadata: dict


@dataclass
class Verdict:
    required_gates_passed: bool
    full_suite_passed: bool
    race_suite_passed: bool
    failures: List[str]
