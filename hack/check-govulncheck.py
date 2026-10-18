#!/usr/bin/env python3
"""Run govulncheck and gate on reachable vulnerabilities.

Findings in modules fail the check. Findings in the Go standard library only
warn. A standard library finding is fixed by a Go toolchain bump, never by a
change in this repository, so it must not block a pull request. The scan still
runs in full and every finding stays in the job log.
"""

from __future__ import annotations

import json
import subprocess
import sys
from dataclasses import dataclass
from typing import Any


GOVULNCHECK = [
    "go",
    "run",
    "golang.org/x/vuln/cmd/govulncheck@v1.8.0",
    "-format=json",
    "./...",
]


# govulncheck reports standard library findings under this module name.
STDLIB_MODULE = "stdlib"


@dataclass(frozen=True)
class AllowlistedVulnerability:
    module: str
    reason: str
    versions: frozenset[str] | None = None


ALLOWLIST = {
    # CVE-2026-30405 (NEXT_HOP denial of service). The fix, osrg/gobgp commit
    # 583080a7 "server: fix malformed nexthop attribute crash", is in v4.4.0
    # and later, but the Go vulnerability database reports no fixed version for
    # any GoBGP module. Keep the exception to the exact version in go.mod, so a
    # GoBGP change has to check this again.
    "GO-2026-4736": AllowlistedVulnerability(
        module="github.com/osrg/gobgp/v4",
        reason="fixed by 583080a7 since v4.4.0; the Go vuln DB lists no fixed version",
        versions=frozenset({"4.10.0"}),
    ),
    # Stable gRPC v1.84.0 contains the backported fix for GHSA-2v4p-qf9q-27wj.
    # Keep this exact-version exception narrow while the Go vulnerability
    # database catches up with the advisory's Sep 25, 2026 update.
    "GO-2026-6443": AllowlistedVulnerability(
        module="google.golang.org/grpc",
        reason="fixed in stable v1.84.0; exact-version exception pending Go vuln DB refresh",
        versions=frozenset({"1.84.0"}),
    ),
}


def iter_json_objects(data: str) -> list[dict[str, Any]]:
    decoder = json.JSONDecoder()
    objects: list[dict[str, Any]] = []
    idx = 0
    while idx < len(data):
        while idx < len(data) and data[idx].isspace():
            idx += 1
        if idx >= len(data):
            break
        obj, end = decoder.raw_decode(data, idx)
        if not isinstance(obj, dict):
            raise ValueError("govulncheck emitted a non-object JSON value")
        objects.append(obj)
        idx = end
    return objects


def is_symbol_finding(finding: dict[str, Any]) -> bool:
    return any(
        isinstance(frame.get("function"), str) and frame["function"] != ""
        for frame in finding.get("trace", [])
        if isinstance(frame, dict)
    )


def vulnerable_module(finding: dict[str, Any]) -> str:
    """Return the module that holds the vulnerable symbol.

    govulncheck orders the trace from the vulnerable symbol outwards to the
    entry point, so the first frame names the affected module.
    """
    for frame in finding.get("trace", []):
        if isinstance(frame, dict) and isinstance(frame.get("module"), str):
            return frame["module"]
    return ""


def finding_modules(finding: dict[str, Any]) -> set[str]:
    modules: set[str] = set()
    for frame in finding.get("trace", []):
        if not isinstance(frame, dict):
            continue
        module = frame.get("module")
        if isinstance(module, str):
            modules.add(module)
    return modules


def finding_module_versions(finding: dict[str, Any]) -> dict[str, set[str]]:
    versions: dict[str, set[str]] = {}
    for frame in finding.get("trace", []):
        if not isinstance(frame, dict):
            continue
        module = frame.get("module")
        version = frame.get("version")
        if isinstance(module, str) and isinstance(version, str) and version:
            versions.setdefault(module, set()).add(version.removeprefix("v"))
    return versions


def main() -> int:
    result = subprocess.run(
        GOVULNCHECK,
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    if result.stderr:
        print(result.stderr, file=sys.stderr, end="")
    if result.returncode != 0:
        print("::error::govulncheck execution failed", file=sys.stderr)
        print(result.stdout, file=sys.stderr)
        return result.returncode

    try:
        objects = iter_json_objects(result.stdout)
    except ValueError as exc:
        print(f"::error::failed to parse govulncheck JSON: {exc}", file=sys.stderr)
        print(result.stdout, file=sys.stderr)
        return 1

    osv_by_id = {
        obj["osv"]["id"]: obj["osv"]
        for obj in objects
        if isinstance(obj.get("osv"), dict) and isinstance(obj["osv"].get("id"), str)
    }

    findings: dict[str, set[str]] = {}
    module_versions: dict[str, dict[str, set[str]]] = {}
    stdlib: dict[str, set[str]] = {}
    fixed_versions: dict[str, set[str]] = {}
    for obj in objects:
        finding = obj.get("finding")
        if not isinstance(finding, dict):
            continue
        osv_id = finding.get("osv")
        if not isinstance(osv_id, str):
            continue
        if not is_symbol_finding(finding):
            continue
        fixed = finding.get("fixed_version")
        if isinstance(fixed, str) and fixed:
            fixed_versions.setdefault(osv_id, set()).add(fixed)
        if vulnerable_module(finding) == STDLIB_MODULE:
            stdlib.setdefault(osv_id, set()).update(finding_modules(finding))
            continue
        findings.setdefault(osv_id, set()).update(finding_modules(finding))
        for module, versions in finding_module_versions(finding).items():
            module_versions.setdefault(osv_id, {}).setdefault(module, set()).update(versions)

    unexpected: dict[str, set[str]] = {}
    allowed: dict[str, set[str]] = {}
    for osv_id, modules in findings.items():
        allow = ALLOWLIST.get(osv_id)
        observed_versions = module_versions.get(osv_id, {}).get(allow.module, set()) if allow else set()
        version_match = allow is not None and (
            allow.versions is None
            or bool(observed_versions)
            and observed_versions <= allow.versions
        )
        if allow and allow.module in modules and version_match:
            allowed[osv_id] = modules
            continue
        unexpected[osv_id] = modules

    for osv_id in sorted(stdlib):
        summary = osv_by_id.get(osv_id, {}).get("summary", "stdlib vulnerability")
        fixed = ", ".join(sorted(fixed_versions.get(osv_id, set()))) or "unknown"
        print(
            f"::warning::Standard library govulncheck finding {osv_id} "
            f"({summary}). Fixed in Go {fixed}. "
            f"This does not fail the check, because only a Go toolchain bump "
            f"clears it."
        )

    for osv_id, modules in sorted(allowed.items()):
        allow = ALLOWLIST[osv_id]
        summary = osv_by_id.get(osv_id, {}).get("summary", "allowed vulnerability")
        print(
            f"::warning::Allowed govulncheck finding {osv_id} "
            f"({summary}) for {allow.module}: {allow.reason}. "
            f"Modules in traces: {', '.join(sorted(modules))}"
        )

    if unexpected:
        for osv_id, modules in sorted(unexpected.items()):
            summary = osv_by_id.get(osv_id, {}).get("summary", "reachable vulnerability")
            print(
                f"::error::Unexpected govulncheck finding {osv_id} "
                f"({summary}). Modules in traces: {', '.join(sorted(modules))}",
                file=sys.stderr,
            )
        return 1

    if stdlib:
        print(
            f"govulncheck found {len(stdlib)} reachable standard library "
            f"vulnerabilities, reported as warnings"
        )
    if not findings:
        print("govulncheck found no reachable module vulnerabilities")
    else:
        print("govulncheck found only documented allowlisted module vulnerabilities")
    return 0


if __name__ == "__main__":
    sys.exit(main())
