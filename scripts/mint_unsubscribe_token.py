"""Mint a signed unsubscribe token for one (user, topic) pair.

The token format, payload and subkey derivation mirror the server
(`server/internal/content/subscriptions.go`): HKDF-SHA256 over APP_SECRET with
the info string `unsubscribe-token`, then HMAC-SHA256 over
`v1|u<user>|t<topic>|e<expiry>`. It exists so the browser verification run can
open the unsubscribe page with a token the server will accept, instead of only
ever testing the rejected path.

    python mint-unsubscribe-token.py <app-secret-base64> <user-id> <topic-id>

This is a verification fixture, not product code, and it prints a credential:
use it only against a scratch environment.
"""
import base64
import hashlib
import hmac
import struct
import sys
import time

TTL_SECONDS = 180 * 24 * 60 * 60


def hkdf_sha256(secret: bytes, info: bytes, length: int = 32) -> bytes:
    """RFC 5869 extract-and-expand with an empty salt, as crypto/hkdf does."""
    prk = hmac.new(b'\x00' * hashlib.sha256().digest_size, secret, hashlib.sha256).digest()
    output, block, counter = b'', b'', 1
    while len(output) < length:
        block = hmac.new(prk, block + info + struct.pack('B', counter), hashlib.sha256).digest()
        output += block
        counter += 1
    return output[:length]


def mint(secret_b64: str, user_id: str, topic_id: str) -> str:
    master = base64.b64decode(secret_b64)
    if len(master) < 32:
        raise SystemExit('APP_SECRET must decode to at least 32 bytes')
    key = hkdf_sha256(master, b'unsubscribe-token')
    expiry = int(time.time()) + TTL_SECONDS
    payload = f'v1|u{user_id}|t{topic_id}|e{expiry}'.encode()
    signature = hmac.new(key, payload, hashlib.sha256).digest()
    return (base64.urlsafe_b64encode(payload).rstrip(b'=').decode() + '.'
            + base64.urlsafe_b64encode(signature).rstrip(b'=').decode())


if __name__ == '__main__':
    if len(sys.argv) != 4:
        raise SystemExit(__doc__)
    print(mint(sys.argv[1], sys.argv[2], sys.argv[3]))
