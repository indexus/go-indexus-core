#!/usr/bin/env python3
"""Mesh consistency scan: ownership partition, delegation chains, aggregates.

Detects the failure classes seen live:
  1. delegated child with no live owner            -> writes loop / data holes
  2. zone owned by several nodes at once           -> double serving
  3. owned zone invisible from the @ chain         -> aggregates < leaf items
  4. @ aggregate vs sum of per-node item counts    -> drift / lost writes
  5. stuck ingress queues (pending + requeues + parked + samples)

Usage: scan_consistency.py [--host 127.0.0.1] [--ports 19000,19010-19025]
Exit code 1 when an inconsistency is found.
"""

import argparse
import json
import sys
import urllib.parse
import urllib.request

ROOT = "@"


def parent(zone: str) -> str:
    if zone == ROOT:
        return ""
    if len(zone) == 1:
        return ROOT
    return zone[:-1]


def fetch(url, timeout=3):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return json.load(r)
    except Exception:
        return None


def parse_ports(spec: str):
    out = []
    for part in spec.split(","):
        part = part.strip()
        if "-" in part:
            a, b = part.split("-", 1)
            out.extend(range(int(a), int(b) + 1))
        elif part:
            out.append(int(part))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--ports", default="19000,19010-19040")
    ap.add_argument("--issuer", default="http://127.0.0.1:22000")
    ap.add_argument("--posted", type=int, default=None,
                    help="expected item count (e.g. loader 'added') to compare against")
    args = ap.parse_args()

    nodes = {}  # name -> {mon, p2p, status, ownership, queue}
    for port in parse_ports(args.ports):
        status = fetch(f"http://{args.host}:{port}/status", timeout=2)
        if not status:
            continue
        name = status["name"]
        nodes[name] = {
            "mon": port,
            "p2p": port + 2000,  # local lab convention (19000->21000)
            "status": status,
            "ownership": fetch(f"http://{args.host}:{port}/ownership") or {},
            "queue": fetch(f"http://{args.host}:{port}/queue") or {},
        }

    if not nodes:
        print("no nodes answered — is the mesh up?")
        sys.exit(2)

    token = ""
    try:
        req = urllib.request.Request(
            f"{args.issuer}/v1/issue/token",
            data=json.dumps({"client_id": "scan", "scopes": ["read"]}).encode(),
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(req, timeout=3) as r:
            token = json.load(r).get("token", "")
    except Exception:
        pass

    def aggregates(node, collection, location):
        url = (f"http://{args.host}:{nodes[node]['p2p']}/aggregates"
               f"?collection={urllib.parse.quote(collection)}"
               f"&location={urllib.parse.quote(location)}&refresh=true")
        req = urllib.request.Request(url, headers={"Authorization": f"Bearer {token}"})
        try:
            with urllib.request.urlopen(req, timeout=4) as r:
                return (json.load(r).get("aggregates") or {}).get(location)
        except Exception:
            return None

    # Global view: owner[collection][zone] = [names], marks[collection][zone] = {children}
    owners, marks = {}, {}
    for name, n in nodes.items():
        for col, zones in n["ownership"].items():
            owners.setdefault(col, {})
            marks.setdefault(col, {})
            for zone, children in zones.items():
                owners[col].setdefault(zone, []).append(name)
                if isinstance(children, dict) and children:
                    marks[col].setdefault(zone, {}).update(
                        {c: name for c in children})

    problems = []

    for col in owners:
        own = owners[col]
        mk = marks.get(col, {})

        # 2. double ownership
        for zone, holders in own.items():
            if len(holders) > 1:
                problems.append(f"[{col}] zone {zone!r} owned by {holders}")

        # 1. delegated child never owned anywhere (and not just re-marked deeper)
        for parent_zone, children in mk.items():
            for child, marker in children.items():
                if child in own:
                    continue
                agg = None
                for name in nodes:
                    agg = aggregates(name, col, child)
                    if agg:
                        break
                if not agg:
                    problems.append(
                        f"[{col}] {parent_zone!r}->{child!r} delegated by "
                        f"{marker} but nobody owns or answers for it")

        # 3. owned zone unreachable through the delegation chain from @
        for zone in own:
            if zone == ROOT:
                continue
            hop, anc = zone, parent(zone)
            while anc:
                if anc in own:
                    if hop not in mk.get(anc, {}):
                        problems.append(
                            f"[{col}] zone {zone!r} owned by {own[zone]} is "
                            f"invisible: ancestor {anc!r} (owned by "
                            f"{own[anc]}) does not mark hop {hop!r}")
                    break
                hop, anc = anc, parent(anc)

        # 4. root aggregate vs summed node counts
        root_owner = (own.get(ROOT) or [None])[0]
        agg = aggregates(root_owner, col, ROOT) if root_owner else None
        total_counts = sum(n["status"].get("items") or 0 for n in nodes.values())
        total_prep = sum(n["status"].get("items_prep") or 0 for n in nodes.values())
        total_queue = sum(n["queue"].get("pending") or 0 for n in nodes.values())
        print(f"[{col}] @-aggregate={agg['count'] if agg else 'n/a'} "
              f"sum(items)={total_counts} prep={total_prep} queued={total_queue}")
        if agg and abs(agg["count"] - total_counts) > total_queue + total_prep + 100:
            problems.append(
                f"[{col}] @ says {agg['count']} but nodes hold {total_counts} "
                f"(queued={total_queue}, prep={total_prep})")
        if args.posted is not None:
            gap = args.posted - total_counts - total_queue - total_prep
            if gap != 0:
                print(f"[{col}] posted={args.posted} -> unaccounted gap {gap} "
                      f"(client-side refusals or duplicates)")

    # 5. queue health
    for name, n in nodes.items():
        q = n["queue"]
        if (q.get("pending") or 0) > 0 or (q.get("parked") or 0) > 0:
            print(f"queue {name} (:{n['mon']}): pending={q.get('pending')} "
                  f"parked={q.get('parked')} requeues={q.get('requeues')} "
                  f"parked_by_collection={q.get('parked_by_collection')} "
                  f"stalls={q.get('stalls')}")
            for s in (q.get("samples") or [])[:5]:
                print(f"    stuck: {s.get('op')} loc={s.get('location')} "
                      f"via={s.get('via')} parked={s.get('parked')} "
                      f"child={s.get('child')} id={s.get('id')}")

    if problems:
        print("\nINCONSISTENCIES:")
        for p in problems:
            print(f"  ✗ {p}")
        sys.exit(1)
    print("\nno structural inconsistency found "
          f"({len(nodes)} nodes, {sum(len(z) for z in owners.values())} zones)")


if __name__ == "__main__":
    main()
