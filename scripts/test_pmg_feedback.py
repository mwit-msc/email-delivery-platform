import importlib.util
import json
import pathlib
import sqlite3
import unittest

spec = importlib.util.spec_from_file_location('feedback', pathlib.Path(__file__).with_name('pmg-feedback.py'))
feedback = importlib.util.module_from_spec(spec)
spec.loader.exec_module(feedback)

class FeedbackTests(unittest.TestCase):
    def test_external_queued_as_is_reported_and_internal_is_correlated(self):
        db = sqlite3.connect(':memory:')
        db.execute('CREATE TABLE queues(queue TEXT PRIMARY KEY,message TEXT)')
        db.execute('CREATE TABLE pending(event_key TEXT PRIMARY KEY,payload TEXT)')
        feedback.correlate('postfix/cleanup[42]: ABC: message-id=<msg_abcdef@example.org>',db,set())
        feedback.correlate('postfix/smtp[42]: ABC: relay=remote[1.2.3.4]:25, status=sent (250 queued as XYZ)',db,set())
        self.assertEqual(json.loads(db.execute('SELECT payload FROM pending').fetchone()[0])['status'],'remote_accepted')
        db.execute('DELETE FROM pending')
        feedback.correlate('postfix/smtp[42]: ABC: relay=internal[127.0.0.1]:10025, status=sent (250 queued as NEXT)',db,{'internal[127.0.0.1]:10025'})
        self.assertEqual(db.execute('SELECT count(*) FROM pending').fetchone()[0],0)
        feedback.correlate('postfix/smtp[42]: NEXT: relay=remote[1.2.3.4]:25, status=bounced (550 rejected)',db,set())
        self.assertEqual(json.loads(db.execute('SELECT payload FROM pending').fetchone()[0])['status'],'bounced')
