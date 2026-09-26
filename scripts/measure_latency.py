#!/usr/bin/env python3
"""Read a bounded AX mailbox sample without exporting message bodies or IDs."""
import argparse
from contextlib import closing
import json
import math
import os
from pathlib import Path
import sqlite3
import time


def summarize(rows):
    groups = {}
    for context, queued, handoff, accepted, channel, fetched, acknowledged in rows:
        context = json.loads(context) if context else {}
        key = tuple(context.get(field, "unavailable") for field in (
            "recipient_host", "recipient_state", "sender_state", "broker_version"))
        group = groups.setdefault(key, {"messages": 0, "intervals": {}})
        group["messages"] += 1
        receipts = [value for value in (accepted, channel) if value is not None]
        receipt = min(receipts) if receipts else None
        for name, start, end in (
            ("queue_to_handoff", queued, handoff),
            ("handoff_to_native_receipt", handoff, receipt),
            ("native_receipt_to_content_fetch", receipt, fetched),
            ("queue_to_acknowledgment", queued, acknowledged),
        ):
            interval = group["intervals"].setdefault(name, {"samples": [], "missing": 0, "out_of_order": 0})
            if start is None or end is None:
                interval["missing"] += 1
            elif end < start:
                interval["out_of_order"] += 1
            else:
                interval["samples"].append(end - start)
    result = []
    for key, group in sorted(groups.items()):
        for interval in group["intervals"].values():
            samples = sorted(interval.pop("samples"))
            interval.update(n=len(samples), p50_ms=None, p95_ms=None)
            if samples:
                interval.update(p50_ms=samples[math.ceil(len(samples)*.5)-1],
                                p95_ms=samples[math.ceil(len(samples)*.95)-1])
        result.append(dict(zip(("recipient_host", "recipient_state_at_queue", "sender_state_at_queue", "broker_version"), key), **group))
    return result


def sample(database, hours, limit):
    deadline = time.monotonic() + 2
    with closing(sqlite3.connect(database.resolve().as_uri() + "?mode=ro", uri=True, timeout=.2)) as db:
        db.execute("PRAGMA query_only=ON")
        db.set_progress_handler(lambda: int(time.monotonic() >= deadline), 1000)
        return db.execute("""
            WITH recent AS (
              SELECT id, json_extract(data,'$.queued_context') AS context FROM messages
              WHERE created>=? ORDER BY created DESC, id LIMIT ?
            )
            SELECT r.context,
              min(CASE WHEN e.state='queued' THEN e.at END),
              min(CASE WHEN e.state='handoff_started' THEN e.at END),
              min(CASE WHEN e.state='wake_accepted' THEN e.at END),
              min(CASE WHEN e.state='channel_written' THEN e.at END),
              min(CASE WHEN e.state='content_served' THEN e.at END),
              min(CASE WHEN e.state IN ('acknowledged','replied') THEN e.at END)
            FROM recent r LEFT JOIN events e ON e.message=r.id GROUP BY r.id
        """, (int((time.time()-hours*3600)*1000), limit)).fetchall()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("-d", type=Path, default=Path(os.environ.get("AX_HOME", str(Path.home()/".ax")))/"mailbox.db", help="mailbox database (read only)")
    parser.add_argument("-n", type=int, default=1000, help="maximum recent messages, 1 to 1000")
    parser.add_argument("-w", type=float, default=24, help="sample window in hours, up to 168")
    parser.add_argument("-v", default="unrecorded", help="operator-supplied harness versions for this measurement")
    args = parser.parse_args()
    if not 1 <= args.n <= 1000 or not 0 < args.w <= 168:
        parser.error("use 1 to 1000 messages and a window greater than 0 and at most 168 hours")
    try:
        rows = sample(args.d, args.w, args.n)
    except sqlite3.Error as error:
        parser.exit(1, f"AX latency sample unavailable: {error}\n")
    print(json.dumps({"sampled_at_ms": int(time.time()*1000), "window_hours": args.w,
        "limit": args.n, "messages": len(rows), "harness_versions": args.v,
        "guidance": "Workload observations, not an idle benchmark. Versions are operator supplied, not per-message native versions. Historical context missing from older messages stays unavailable. Ready is not proof of idle; offline does not prove reconnecting. No recipient turn-start observation is available. CPU and memory require a separate process measurement.",
        "groups": summarize(rows)}, indent=2))


if __name__ == "__main__":
    main()
