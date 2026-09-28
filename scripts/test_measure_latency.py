import json
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest

from measure_latency import sample, summarize


class LatencySampleTests(unittest.TestCase):
    def test_sample_is_bounded_read_only_and_excludes_content(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/"mailbox.db"
            with sqlite3.connect(path) as db:
                db.executescript("CREATE TABLE messages(id TEXT,data TEXT,created INTEGER); CREATE TABLE events(message TEXT,state TEXT,at INTEGER);")
                context = dict(recipient_host="codex", recipient_state="busy", sender_state="ready", broker_version="fixture")
                now = int(time.time()*1000)
                for i in range(4):
                    db.execute("INSERT INTO messages VALUES(?,?,?)", (f"private-id-{i}", json.dumps(dict(text="SECRET_BODY", queued_context=context)), now+i))
                    for state, offset in (("queued", 0), ("handoff_started", 5), ("wake_accepted", 8), ("content_served", 12), ("replied", 20)):
                        db.execute("INSERT INTO events VALUES(?,?,?)", (f"private-id-{i}", state, now+i+offset))
            before = path.read_bytes()
            rows = sample(path, 24, 2)
            self.assertEqual(len(rows), 2)
            result = summarize(rows)
            encoded = json.dumps(result)
            self.assertNotIn("SECRET_BODY", encoded)
            self.assertNotIn("private-id", encoded)
            self.assertEqual(result[0]["recipient_state_at_queue"], "busy")
            self.assertEqual(result[0]["intervals"]["queue_to_acknowledgment"]["p95_ms"], 20)
            self.assertEqual(path.read_bytes(), before)

    def test_unknown_context_missing_receipts_and_late_observations(self):
        rows = [(None, 100, 105, None, None, None, None), (None, 100, 105, 120, None, 115, 130)]
        result = summarize(rows)[0]
        self.assertEqual(result["recipient_state_at_queue"], "unavailable")
        self.assertEqual(result["messages"], 2)
        interval = result["intervals"]["native_receipt_to_content_fetch"]
        self.assertEqual(interval, dict(n=0, missing=1, out_of_order=1, p50_ms=None, p95_ms=None))


if __name__ == "__main__":
    unittest.main()
