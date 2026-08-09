#!/usr/bin/env python3
"""Ingestion conservation probe against a live local mesh.

Seeds enough items to push the mesh through autoscaling and a few delegations,
then checks the only property that matters at the end of a load: every write the
mesh accepted is a write the mesh holds. A zone left without an owner used to
break exactly this — writes answered 201 and then circulated between nodes for
ever, so the totals stayed frozen while the queues stayed hot.

Throwaway diagnostic, not part of the build.
"""

import json
import os
import random
import string
import sys
import time
import urllib.error
import urllib.request
from concurrent.futures import ThreadPoolExecutor

ISSUER = "http://127.0.0.1:22000"
NODE = "http://127.0.0.1:21000"
COLLECTION = "probeV2020idx0001"
MON_PORTS = [19000] + list(range(19010, 19026))
ALPHABET = string.ascii_letters + string.digits
SEED = int(os.environ.get("PROBE_SEED", "60000"))
WORKERS = 48


def post(url, payload, token=None, timeout=10):
    body = json.dumps(payload).encode()
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    with urllib.request.urlopen(req, timeout=timeout) as res:
        return res.status, res.read()


def get(url, timeout=3):
    with urllib.request.urlopen(url, timeout=timeout) as res:
        return json.load(res)


def token():
    _, raw = post(f"{ISSUER}/v1/issue/token", {"client_id": "probe", "scopes": ["read", "write"]})
    return json.loads(raw)["token"]


def mesh():
    """Returns (items held, items queued, nodes answering)."""
    held = queued = alive = 0
    for port in MON_PORTS:
        try:
            st = get(f"http://127.0.0.1:{port}/status")
        except Exception:
            continue
        held += st.get("items", 0)
        queued += st.get("queue", 0)
        alive += 1
    return held, queued, alive


def insert(tok, i):
    loc = "".join(random.choice(ALPHABET) for _ in range(16))
    payload = {
        "item": {
            "collection": COLLECTION,
            "location": loc,
            "id": f"probe-{i}",
            "metrics": [1.0, 2.0, 3.0, 4.0, 5.0],
        },
        "root": "@",
        "current": loc,
    }
    for _ in range(3):
        try:
            status, _ = post(f"{NODE}/item", payload, tok)
            return status == 201
        except urllib.error.HTTPError as err:
            if err.code in (429, 503):
                time.sleep(0.2)
                continue
            return False
        except Exception:
            time.sleep(0.2)
    return False


def main():
    tok = token()
    print(f"=== seeding {SEED} items ===")
    started = time.time()
    with ThreadPoolExecutor(max_workers=WORKERS) as pool:
        accepted = sum(pool.map(lambda i: insert(tok, i), range(SEED)))
    print(f"  accepted {accepted}/{SEED} in {time.time() - started:.0f}s")

    print("=== waiting for the queues to drain ===")
    stable = 0
    previous = None
    for _ in range(120):
        time.sleep(5)
        held, queued, alive = mesh()
        print(f"  held={held:<8} queued={queued:<8} nodes={alive}")
        if queued == 0 and held == previous:
            stable += 1
            if stable >= 2:
                break
        else:
            stable = 0
        previous = held

    held, queued, alive = mesh()
    print()
    print(f"  accepted={accepted}  held={held}  queued={queued}  lost={accepted - held - queued}")
    if queued > 0:
        print(f"  \u2717 {queued} writes still circulating after the load stopped")
        return 1
    if held != accepted:
        print(f"  \u2717 the mesh does not hold what it accepted (delta {held - accepted})")
        return 1
    print("  \u2713 every accepted write is held by the mesh, nothing is spinning")
    return 0


if __name__ == "__main__":
    sys.exit(main())
