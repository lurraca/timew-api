"""Cloudflare Access: verify the JWT Access puts on every request it lets in.

Access already decides who gets through (email one-time code for browsers, a
service token for scripts). Checking its signed token here as well means a
request that reaches the port some other way, e.g. from another process on
kodama, still gets nothing. Same approach as splice's internal/web/auth.go.
"""

import base64
import json
import threading
import time
import urllib.request

from cryptography.exceptions import InvalidSignature
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.asymmetric import padding, rsa

HEADER = "Cf-Access-Jwt-Assertion"
LEEWAY = 60


class Unauthorized(Exception):
    pass


def _b64(s):
    return base64.urlsafe_b64decode(s + "=" * (-len(s) % 4))


def _b64int(s):
    return int.from_bytes(_b64(s), "big")


class AccessVerifier:
    def __init__(self, team_domain, audience, fetch=None, ttl=3600):
        self.issuer = f"https://{team_domain}"
        self.certs_url = f"{self.issuer}/cdn-cgi/access/certs"
        self.audience = audience
        self.fetch = fetch or self._fetch
        self.ttl = ttl
        self._keys = {}
        self._loaded = 0.0
        self._lock = threading.Lock()

    def _fetch(self, url):
        with urllib.request.urlopen(url, timeout=10) as r:
            return json.load(r)

    def _key(self, kid):
        with self._lock:
            stale = time.time() - self._loaded > self.ttl
            # Refresh on an unknown kid too: Access rotates keys, and the new
            # one shows up in tokens before our cache would expire.
            if stale or kid not in self._keys:
                jwks = self.fetch(self.certs_url)
                self._keys = {
                    k["kid"]: rsa.RSAPublicNumbers(_b64int(k["e"]), _b64int(k["n"])).public_key()
                    for k in jwks.get("keys", []) if k.get("kty") == "RSA"
                }
                self._loaded = time.time()
            if kid not in self._keys:
                raise Unauthorized("unknown signing key")
            return self._keys[kid]

    def verify(self, token, now=None):
        """Return the claims if the token is valid for this application."""
        if not token:
            raise Unauthorized("missing Access token")
        try:
            h64, p64, s64 = token.split(".")
            header, claims = json.loads(_b64(h64)), json.loads(_b64(p64))
            sig = _b64(s64)
        except ValueError:
            raise Unauthorized("malformed token") from None
        if header.get("alg") != "RS256":
            raise Unauthorized("unexpected algorithm")
        try:
            self._key(header.get("kid")).verify(sig, f"{h64}.{p64}".encode(), padding.PKCS1v15(), hashes.SHA256())
        except InvalidSignature:
            raise Unauthorized("bad signature") from None

        now = time.time() if now is None else now
        aud = claims.get("aud")
        if self.audience not in (aud if isinstance(aud, list) else [aud]):
            raise Unauthorized("wrong audience")
        if claims.get("iss") != self.issuer:
            raise Unauthorized("wrong issuer")
        if claims.get("exp", 0) + LEEWAY < now:
            raise Unauthorized("expired")
        if claims.get("nbf", 0) - LEEWAY > now:
            raise Unauthorized("not yet valid")
        return claims


def who(claims):
    """A browser login carries an email; a service token its client id."""
    return claims.get("email") or claims.get("common_name") or "unknown"
