#!/usr/bin/env python3
import copy
import datetime
import importlib.util
import pathlib
import sys
import unittest

sys.dont_write_bytecode = True

spec = importlib.util.spec_from_file_location("guard", pathlib.Path(__file__).with_name("check-cli360-image-recovery.py"))
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


class RecoveryTest(unittest.TestCase):
    def test_exact_owner_candidate_and_expiry(self):
        args = [guard.SOURCE, "v3.6.0", guard.RUN, "1", guard.DIGEST]
        env = {"GITHUB_REPOSITORY": guard.REPOSITORY, "GITHUB_REF": "refs/heads/main", "GITHUB_ACTOR": "justin-layerv", "GITHUB_TRIGGERING_ACTOR": "justin-layerv", "GITHUB_EVENT_NAME": "workflow_dispatch"}
        now = guard.EXPIRES - datetime.timedelta(seconds=1)
        guard.validate_inputs(*args, env, now)
        for index, wrong in enumerate(["a" * 40, "v3.6.1", "123", "2", "sha256:" + "a" * 64]):
            changed = args.copy()
            changed[index] = wrong
            with self.assertRaises(ValueError):
                guard.validate_inputs(*changed, env, now)
        for key in env:
            with self.assertRaises(ValueError):
                guard.validate_inputs(*args, {**env, key: "other"}, now)
        with self.assertRaises(ValueError):
            guard.validate_inputs(*args, env, guard.EXPIRES)

    def evidence(self):
        run = {"id": int(guard.RUN), "run_attempt": 1, "head_sha": "4646b9e6f4ca7dc97d5f9517564b591149e59e46", "head_branch": "main", "path": ".github/workflows/release-please.yml", "event": "workflow_dispatch", "status": "completed", "conclusion": "failure", "repository": {"full_name": guard.REPOSITORY}, "head_repository": {"full_name": guard.REPOSITORY}}
        jobs = [{"id": key, "name": value[0], "conclusion": value[1], "status": "completed", "steps": [{"name": name, "conclusion": "failure"} for name in guard.FAILED_STEPS.get(key, [])]} for key, value in guard.JOBS.items()]
        assets = [{"name": "checksums.txt", "digest": "sha256:" + guard.CHECKSUMS}, {"name": "checksums.txt.sigstore.json", "digest": "sha256:" + guard.BUNDLE}]
        assets += [{"name": name, "digest": "sha256:" + digest} for name, digest in guard.NATIVE_ARCHIVES.items()]
        return run, jobs, {"tag_name": "v3.6.0", "target_commitish": guard.SOURCE, "draft": False, "assets": assets}

    def test_only_exact_terminal_security_and_cleanup_evidence(self):
        evidence = self.evidence()
        guard.validate_evidence(*evidence)
        for key in evidence[0]:
            changed = copy.deepcopy(evidence)
            changed[0][key] = "other"
            with self.assertRaises((ValueError, AttributeError)):
                guard.validate_evidence(*changed)
        for index in range(len(evidence[1])):
            changed = copy.deepcopy(evidence)
            changed[1][index]["conclusion"] = "cancelled"
            with self.assertRaises(ValueError):
                guard.validate_evidence(*changed)
        changed = copy.deepcopy(evidence)
        changed[1][0]["steps"] = [{"name": "security failure", "conclusion": "failure"}]
        with self.assertRaises(ValueError):
            guard.validate_evidence(*changed)
        for index in range(len(evidence[2]["assets"])):
            changed = copy.deepcopy(evidence)
            changed[2]["assets"][index]["digest"] = "sha256:" + "a" * 64
            with self.assertRaises(ValueError):
                guard.validate_evidence(*changed)
        for key in evidence[2]:
            changed = copy.deepcopy(evidence)
            changed[2][key] = [] if key == "assets" else "other"
            with self.assertRaises(ValueError):
                guard.validate_evidence(*changed)


if __name__ == "__main__":
    unittest.main()
