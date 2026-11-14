#!/usr/bin/env python3
"""Check startup and proxy authentication without submitting inference requests."""
import argparse
import json
from pathlib import Path
import time
import urllib.error
import urllib.request


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--base-url", default="http://127.0.0.1:8789")
    parser.add_argument("--key-file", type=Path, default=Path("data/api-key"))
    args = parser.parse_args()
    base = args.base_url.rstrip("/")
    key = args.key_file.read_text().strip()

    def request(path, authenticated=False):
        headers = {"Authorization": "Bearer " + key} if authenticated else {}
        req = urllib.request.Request(base + path, headers=headers)
        try:
            with urllib.request.urlopen(req, timeout=2) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as error:
            return error.code, json.load(error)

    deadline = time.monotonic() + 30
    while True:
        try:
            status, body = request("/healthz")
            assert status == 200 and body["status"] == "ok", "health check failed"
            break
        except urllib.error.URLError:
            if time.monotonic() >= deadline:
                raise SystemExit("Service did not become ready within 30 seconds") from None
            time.sleep(0.2)
    status, body = request("/v1/models")
    assert status == 401 and body["error"]["code"] == "invalid_api_key", "authentication check failed"
    status, body = request("/v1/status", authenticated=True)
    assert status == 200 and body["code"] == 200 and "token_ready" in body["data"], "status check failed"
    if not body["data"]["token_ready"] and not body["data"]["automatic_sso_refresh"]:
        status, _ = request("/v1/workflows", authenticated=True)
        assert status == 503, "missing credentials must reject upstream access"
    print("Startup, authentication and local status checks passed; no inference submitted.")


if __name__ == "__main__":
    main()
