"""AWS's published S3 GET vector is independent of the Envoy signer under test."""
import unittest
import json
from pathlib import Path
from http.client import HTTPConnection
from threading import Thread
from http.server import ThreadingHTTPServer
from email.message import Message
from sigv4 import verify_sigv4


class SignatureTest(unittest.TestCase):
    def setUp(self):
        # Public AWS documentation example; not a live account or credential.
        self.access = "AKIAIOSFODNN7EXAMPLE"
        self.secret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
        self.headers = Message()
        for name, value in {
            "Host": "examplebucket.s3.amazonaws.com", "Range": "bytes=0-9",
            "x-amz-date": "20130524T000000Z",
            "x-amz-content-sha256": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
            "Authorization": "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request,"
            "SignedHeaders=host;range;x-amz-content-sha256;x-amz-date,"
            "Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41",
        }.items():
            self.headers[name] = value

    def check(self, method="GET", path="/test.txt"):
        return verify_sigv4(method, path, self.headers, self.access, self.secret)

    def test_aws_published_vector(self):
        self.assertTrue(self.check())

    def test_wire_mutations_rejected(self):
        for method, path in [("HEAD", "/test.txt"), ("GET", "/other.txt"), ("GET", "/test.txt?q=1")]:
            with self.subTest(method=method, path=path):
                self.assertFalse(self.check(method, path))
        self.headers.replace_header("Range", "bytes=1-9")
        self.assertFalse(self.check())

    def test_forged_and_missing_authorization_rejected(self):
        del self.headers["Authorization"]
        self.assertFalse(self.check())
        self.headers["Authorization"] = "AWS4-HMAC-SHA256 Credential=test/fake"
        self.assertFalse(self.check())


class SDKVectorTest(unittest.TestCase):
    def vectors(self):
        return json.loads(Path(__file__).with_name("sdk_vectors.json").read_text())["vectors"]

    def test_independent_sdk_canonical_paths(self):
        for vector in self.vectors():
            headers = Message()
            for name, value in vector["headers"].items():
                headers[name] = value
            with self.subTest(target=vector["target"]):
                self.assertTrue(verify_sigv4("GET", vector["target"], headers))
                self.assertFalse(verify_sigv4("GET", vector["target"] + "x", headers))

    def test_real_stub_rejects_signature_tampering(self):
        from stub import Handler
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        worker = Thread(target=server.serve_forever, daemon=True)
        worker.start()
        vector = self.vectors()[1]
        try:
            for forged in (False, True):
                headers = dict(vector["headers"])
                if forged:
                    signature = headers["Authorization"]
                    headers["Authorization"] = signature[:-1] + ("0" if signature[-1] != "0" else "1")
                connection = HTTPConnection("127.0.0.1", server.server_port, timeout=5)
                try:
                    connection.request("GET", vector["target"], headers=headers)
                    response = connection.getresponse()
                    # Valid signature reaches key lookup (unknown fixture); forged one never does.
                    self.assertEqual(response.status, 403 if forged else 404)
                    body = response.read()
                    self.assertIn(b"SignatureDoesNotMatch" if forged else b"NoSuchKey", body)
                finally:
                    connection.close()
        finally:
            server.shutdown()
            server.server_close()
            worker.join()


if __name__ == "__main__":
    unittest.main()
