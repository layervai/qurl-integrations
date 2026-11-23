#!/usr/bin/env python3
"""Exercise pin drift and supported-platform rejection without registry access."""
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("check", Path(__file__).with_name("check-s3-origin-default.py"))
check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(check)


class OriginDefaultTest(unittest.TestCase):
    def setUp(self):
        self.index = {"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json",
                      "manifests": [{"digest": "sha256:" + char * 64,
                                     "platform": {"os": "linux", "architecture": arch}}
                                    for arch, char in (("amd64", "a"), ("arm64", "b"))]}
        self.index["manifests"].append({"digest": "sha256:" + "c" * 64,
                                        "platform": {"os": "unknown", "architecture": "unknown"}})
        self.body = json.dumps(self.index).encode()
        self.pin = check.IMAGE + "@sha256:" + hashlib.sha256(self.body).hexdigest()

    def test_canonical_default_and_public_index(self):
        source = '\tdefaultS3StaticConnectorImage = "' + self.pin + '"\n'
        self.assertEqual(check.default_image(source), self.pin)
        self.assertEqual(check.check_manifest(self.pin, self.body), self.pin)
        for invalid in (source + source, source.replace("@sha256:", ":"), source.replace(check.IMAGE, "private.example/origin")):
            with self.subTest(source=invalid), self.assertRaises(ValueError):
                check.default_image(invalid)

    def test_changed_promotion_requires_pin_rotation(self):
        with self.assertRaisesRegex(ValueError, "rotate the tested pin"):
            check.check_manifest(check.IMAGE + "@sha256:" + "0" * 64, self.body)

    def test_incomplete_or_ambiguous_platforms_fail(self):
        cases = []
        missing = copy.deepcopy(self.index); missing["manifests"].pop(0); cases.append(missing)
        duplicate = copy.deepcopy(self.index); duplicate["manifests"].append(duplicate["manifests"][0]); cases.append(duplicate)
        invalid = copy.deepcopy(self.index); invalid["manifests"][0]["digest"] = "main"; cases.append(invalid)
        single = copy.deepcopy(self.index); single["mediaType"] = "application/vnd.oci.image.manifest.v1+json"; cases.append(single)
        for document in cases:
            with self.subTest(document=document), self.assertRaises(ValueError):
                body = json.dumps(document).encode()
                pin = check.IMAGE + "@sha256:" + hashlib.sha256(body).hexdigest()
                check.check_manifest(pin, body)


if __name__ == "__main__":
    unittest.main()
