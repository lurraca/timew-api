import base64
import json
import time
import unittest

from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import padding, rsa

from timew_api.auth import AccessVerifier, Unauthorized, who

TEAM = "example.cloudflareaccess.com"
AUD = "aud-tag-123"


def b64(data):
    return base64.urlsafe_b64encode(data).rstrip(b"=").decode()


def b64int(n):
    return b64(n.to_bytes((n.bit_length() + 7) // 8, "big"))


class Keys:
    def __init__(self, kid="k1"):
        self.kid = kid
        self.private = rsa.generate_private_key(public_exponent=65537, key_size=2048)

    def jwk(self):
        pub = self.private.public_key().public_numbers()
        return {"kid": self.kid, "kty": "RSA", "alg": "RS256", "n": b64int(pub.n), "e": b64int(pub.e)}

    def token(self, alg="RS256", **claims):
        body = {"aud": [AUD], "iss": f"https://{TEAM}", "exp": time.time() + 600, "email": "me@example.com"}
        body.update(claims)
        head = b64(json.dumps({"alg": alg, "kid": self.kid}).encode())
        payload = b64(json.dumps(body).encode())
        sig = self.private.sign(f"{head}.{payload}".encode(), padding.PKCS1v15(), hashes.SHA256())
        return f"{head}.{payload}.{b64(sig)}"


class Verify(unittest.TestCase):
    def setUp(self):
        self.keys = Keys()
        self.fetches = 0

        def fetch(url):
            self.assertEqual(url, f"https://{TEAM}/cdn-cgi/access/certs")
            self.fetches += 1
            return {"keys": [self.keys.jwk()]}

        self.v = AccessVerifier(TEAM, AUD, fetch=fetch)

    def test_valid_browser_token(self):
        self.assertEqual(who(self.v.verify(self.keys.token())), "me@example.com")

    def test_service_token_has_common_name(self):
        claims = self.v.verify(self.keys.token(email=None, common_name="abc.access"))
        self.assertEqual(who(claims), "abc.access")

    def test_keys_are_cached(self):
        self.v.verify(self.keys.token())
        self.v.verify(self.keys.token())
        self.assertEqual(self.fetches, 1)

    def test_rejections(self):
        other = Keys()
        cases = {
            "missing": None,
            "garbage": "a.b",
            "wrong audience": self.keys.token(aud=["someone-else"]),
            "wrong issuer": self.keys.token(iss="https://evil.cloudflareaccess.com"),
            "expired": self.keys.token(exp=time.time() - 3600),
            "not yet valid": self.keys.token(nbf=time.time() + 3600),
            "signed by another key": other.token(),
            "alg none": self.keys.token(alg="none"),
        }
        for name, token in cases.items():
            with self.subTest(name), self.assertRaises(Unauthorized):
                self.v.verify(token)

    def test_tampered_payload(self):
        h, _, s = self.keys.token().split(".")
        forged = b64(json.dumps({"aud": [AUD], "iss": f"https://{TEAM}", "exp": time.time() + 600}).encode())
        with self.assertRaises(Unauthorized):
            self.v.verify(f"{h}.{forged}.{s}")

    def test_key_rotation_refetches_unknown_kid(self):
        self.v.verify(self.keys.token())
        self.keys = Keys(kid="k2")  # Access rotated: new kid, new key
        self.v.verify(self.keys.token())
        self.assertEqual(self.fetches, 2)


if __name__ == "__main__":
    unittest.main()
