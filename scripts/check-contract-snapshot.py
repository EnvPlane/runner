#!/usr/bin/env python3
"""Verify reviewed immutable contracts bytes, never a moving remote latest."""
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys

MODULE = "github.com/envplane/contracts"
REPOSITORY = "https://github.com/envplane/contracts.git"
ROOT = Path(__file__).resolve().parent.parent
FIELDS = {"formatVersion", "version", "sourceCommit", "moduleSum", "goModSum", "openapiSha256"}


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def decode(raw):
    return json.loads(raw, object_pairs_hook=unique_object)


def command(args):
    environment = dict(os.environ, GOWORK="off")
    try:
        result = subprocess.run(args, cwd=ROOT, env=environment, capture_output=True,
                                text=True, timeout=120, check=False)
    except (OSError, subprocess.TimeoutExpired) as error:
        raise ValueError("contracts evidence command unavailable") from error
    if result.returncode != 0 or len(result.stdout) > 2 * 1024 * 1024:
        raise ValueError("contracts evidence command failed")
    return result.stdout


def verify():
    if len(sys.argv) != 1:
        raise ValueError("snapshot overrides are not supported")
    snapshot = decode((ROOT / "contracts-release.json").read_text())
    if not isinstance(snapshot, dict) or set(snapshot) != FIELDS or type(snapshot["formatVersion"]) is not int or snapshot["formatVersion"] != 1:
        raise ValueError("reviewed contracts snapshot missing or malformed")
    if not isinstance(snapshot["version"], str) or not re.fullmatch(r"v\d+\.\d+\.\d+", snapshot["version"]):
        raise ValueError("contracts requires an immutable stable release tag")
    for field, length in (("sourceCommit", 40), ("openapiSha256", 64)):
        if not isinstance(snapshot[field], str) or not re.fullmatch(r"[0-9a-f]{" + str(length) + "}", snapshot[field]):
            raise ValueError("invalid reviewed contracts identity")
    for field in ("moduleSum", "goModSum"):
        if not isinstance(snapshot[field], str) or not re.fullmatch(r"h1:[A-Za-z0-9+/]{43}=", snapshot[field]):
            raise ValueError("invalid reviewed contracts checksum")

    module = decode(command(["go", "mod", "edit", "-json"]))
    if not isinstance(module, dict) or not isinstance(module.get("Require"), list) or any(not isinstance(item, dict) for item in module["Require"]):
        raise ValueError("invalid module requirements evidence")
    required = [item for item in module.get("Require", []) if item.get("Path") == MODULE]
    if len(required) != 1 or required[0].get("Version") != snapshot["version"]:
        raise ValueError("go.mod does not match the reviewed contracts snapshot")
    if any(item.get("Old", {}).get("Path") == MODULE for item in (module.get("Replace") or [])):
        raise ValueError("local contracts replacements are not release evidence")
    version = snapshot["version"]
    sums = (ROOT / "go.sum").read_text().splitlines()
    for suffix, field in (("", "moduleSum"), ("/go.mod", "goModSum")):
        entries = [line.split() for line in sums if line.split()[:2] == [MODULE, version + suffix]]
        if len(entries) != 1 or entries[0] != [MODULE, version + suffix, snapshot[field]]:
            raise ValueError("go.sum does not match the reviewed contracts snapshot")

    tag = "refs/tags/" + version
    rows = command(["git", "ls-remote", "--tags", REPOSITORY, tag, tag + "^{}"]).splitlines()
    refs = {}
    for row in rows:
        parts = row.split()
        if len(parts) != 2 or not re.fullmatch(r"[0-9a-f]{40}", parts[0]) or parts[1] not in (tag, tag + "^{}") or parts[1] in refs:
            raise ValueError("invalid remote contracts tag evidence")
        refs[parts[1]] = parts[0]
    if tag not in refs or refs.get(tag + "^{}", refs[tag]) != snapshot["sourceCommit"]:
        raise ValueError("remote contracts tag missing or changed")

    downloaded = decode(command(["go", "mod", "download", "-json", MODULE + "@" + version]))
    if not isinstance(downloaded, dict):
        raise ValueError("invalid published contracts evidence")
    if downloaded.get("Error") or downloaded.get("Path") != MODULE or downloaded.get("Version") != version or downloaded.get("Sum") != snapshot["moduleSum"] or downloaded.get("GoModSum") != snapshot["goModSum"]:
        raise ValueError("published contracts module identity/checksum mismatch")
    origin = downloaded.get("Origin")
    if origin is not None:
        if not isinstance(origin, dict) or origin.get("VCS") != "git" or origin.get("Hash") != snapshot["sourceCommit"] or origin.get("Ref") != tag or str(origin.get("URL", "")).removesuffix(".git").lower() != REPOSITORY.removesuffix(".git").lower():
            raise ValueError("published contracts origin mismatch")
    directory = downloaded.get("Dir")
    if not isinstance(directory, str) or not directory:
        raise ValueError("published contracts module directory missing")
    schema = Path(directory) / "openapi" / "openapi.json"
    if not schema.is_file() or schema.stat().st_size > 32 * 1024 * 1024:
        raise ValueError("published contracts schema unavailable")
    if hashlib.sha256(schema.read_bytes()).hexdigest() != snapshot["openapiSha256"]:
        raise ValueError("published contracts schema checksum mismatch")
    print("verified reviewed contracts " + version + " at " + snapshot["sourceCommit"])


if __name__ == "__main__":
    try:
        verify()
    except (ValueError, OSError, KeyError, TypeError) as failure:
        print("contracts snapshot verification failed: " + str(failure), file=sys.stderr)
        sys.exit(1)
