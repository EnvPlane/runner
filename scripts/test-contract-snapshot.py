#!/usr/bin/env python3
import copy
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("snapshot", Path(__file__).with_name("check-contract-snapshot.py"))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class SnapshotTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.schema = self.root / "module" / "openapi" / "openapi.json"
        self.schema.parent.mkdir(parents=True)
        self.schema.write_bytes(b'{"openapi":"3.0.3"}\n')
        self.snapshot = {"formatVersion": 1, "version": "v0.1.111", "sourceCommit": "a" * 40,
                         "moduleSum": "h1:" + "A" * 43 + "=", "goModSum": "h1:" + "B" * 43 + "=",
                         "openapiSha256": hashlib.sha256(self.schema.read_bytes()).hexdigest()}
        self.module = {"Require": [{"Path": guard.MODULE, "Version": "v0.1.111"}], "Replace": None}
        self.download = {"Path": guard.MODULE, "Version": "v0.1.111", "Sum": self.snapshot["moduleSum"],
                         "GoModSum": self.snapshot["goModSum"], "Dir": str(self.schema.parent.parent),
                         "Origin": {"VCS": "git", "Hash": "a" * 40, "Ref": "refs/tags/v0.1.111",
                                    "URL": "https://github.com/envplane/contracts"}}
        self.tags = "b" * 40 + "\trefs/tags/v0.1.111\n" + "a" * 40 + "\trefs/tags/v0.1.111^{}\n"
        self.calls = []
        self.write_snapshot()
        (self.root / "go.sum").write_text(guard.MODULE + " v0.1.111 " + self.snapshot["moduleSum"] + "\n" + guard.MODULE + " v0.1.111/go.mod " + self.snapshot["goModSum"] + "\n")

    def write_snapshot(self):
        (self.root / "contracts-release.json").write_text(json.dumps(self.snapshot))

    def command(self, args):
        self.calls.append(args)
        if args == ["go", "mod", "edit", "-json"]:
            return json.dumps(self.module)
        if args == ["git", "ls-remote", "--tags", guard.REPOSITORY, "refs/tags/v0.1.111", "refs/tags/v0.1.111^{}"]:
            return self.tags
        if args == ["go", "mod", "download", "-json", guard.MODULE + "@v0.1.111"]:
            return json.dumps(self.download)
        self.fail("unreviewed evidence request")

    def verify(self):
        with patch.object(guard, "ROOT", self.root), patch.object(guard, "command", self.command), patch.object(guard.sys, "argv", ["guard"]), patch("sys.stdout", new=io.StringIO()):
            guard.verify()

    def test_reviewed_release_survives_new_remote_latest(self):
        with patch.dict(os.environ, {"ENVPLANE_CONTRACTS_LATEST": "v99.0.0", "ENVPLANE_CONTRACTS_REPOSITORY": "https://untrusted.invalid"}):
            self.verify()
        self.assertEqual(len(self.calls), 3)
        self.assertNotIn("refs/tags/v*", self.calls[1])

    def test_old_unreviewed_pin_rejected(self):
        self.module["Require"][0]["Version"] = "v0.1.110"
        with self.assertRaisesRegex(ValueError, "go.mod"):
            self.verify()

    def test_local_replace_rejected(self):
        self.module["Replace"] = [{"Old": {"Path": guard.MODULE}, "New": {"Path": "../contracts"}}]
        with self.assertRaisesRegex(ValueError, "replacements"):
            self.verify()

    def test_tag_missing_or_changed_rejected(self):
        for tags in ("", self.tags.replace("a" * 40, "c" * 40), self.tags + self.tags):
            with self.subTest(tags=tags):
                self.tags = tags
                with self.assertRaises(ValueError):
                    self.verify()

    def test_download_checksum_and_origin_rejected(self):
        original = copy.deepcopy(self.download)
        for field in ("Sum", "GoModSum", "Version", "Path"):
            self.download = copy.deepcopy(original)
            self.download[field] = "unreviewed"
            with self.subTest(field=field), self.assertRaises(ValueError):
                self.verify()
        self.download = copy.deepcopy(original)
        self.download["Origin"]["Hash"] = "c" * 40
        with self.assertRaisesRegex(ValueError, "origin"):
            self.verify()

    def test_proxy_without_origin_still_requires_tag_and_bytes(self):
        self.download.pop("Origin")
        self.verify()
        self.schema.write_bytes(b"tampered schema")
        with self.assertRaisesRegex(ValueError, "schema"):
            self.verify()

    def test_snapshot_missing_unknown_duplicate_and_boolean_version(self):
        snapshot_path = self.root / "contracts-release.json"
        snapshot_path.unlink()
        with self.assertRaises(OSError):
            self.verify()
        self.snapshot["unknown"] = True
        self.write_snapshot()
        with self.assertRaises(ValueError):
            self.verify()
        self.snapshot.pop("unknown")
        self.snapshot["formatVersion"] = True
        self.write_snapshot()
        with self.assertRaises(ValueError):
            self.verify()
        snapshot_path.write_text('{"formatVersion":1,"formatVersion":1}')
        with self.assertRaisesRegex(ValueError, "duplicate"):
            self.verify()

    def test_missing_or_changed_go_sum_rejected(self):
        (self.root / "go.sum").write_text("")
        with self.assertRaisesRegex(ValueError, "go.sum"):
            self.verify()

    def test_evidence_error_never_echoes_remote_secret(self):
        response = subprocess.CompletedProcess([], 1, "credential-secret", "credential-secret")
        with patch.object(guard.subprocess, "run", return_value=response):
            with self.assertRaises(ValueError) as failure:
                guard.command(["git", "ls-remote"])
        self.assertNotIn("credential-secret", str(failure.exception))


if __name__ == "__main__":
    unittest.main()
