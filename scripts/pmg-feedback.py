#!/usr/bin/env python3
"""Read Postfix logs on stdin; persist and retry correlated EDP transport events."""
import hashlib
import json
import os
import re
import sqlite3
import sys
import threading
import urllib.error
import urllib.request

QUEUE = re.compile(r'postfix/[\w-]+\[\d+\]:\s+([A-Za-z0-9]+):')
MESSAGE = re.compile(r'message-id=<((?:msg_)[a-f0-9]+)@')
FORWARD = re.compile(r'queued as ([A-Za-z0-9]+)')
STATUS = re.compile(r'status=(sent|bounced)\b')
RELAY = re.compile(r'relay=([^,\s]+)')

def correlate(line, database, internal_relays):
    queue = QUEUE.search(line)
    if not queue:
        return
    qid = queue[1]
    mid = MESSAGE.search(line)
    if mid:
        database.execute('INSERT OR REPLACE INTO queues VALUES(?,?)', (qid, mid[1]))
    row = database.execute('SELECT message FROM queues WHERE queue=?', (qid,)).fetchone()
    if row:
        forwarded = FORWARD.search(line)
        relay = RELAY.search(line)
        internal = bool(relay and relay[1] in internal_relays)
        if internal and forwarded:
            database.execute('INSERT OR REPLACE INTO queues VALUES(?,?)', (forwarded[1], row[0]))
        outcome = STATUS.search(line)
        if outcome and (outcome[1] == 'bounced' or not internal):
            payload = {'message_id': row[0], 'status': 'remote_accepted' if outcome[1] == 'sent' else 'bounced',
                       'event_key': hashlib.sha256(line.encode()).hexdigest(),
                       'detail': 'Postfix transport reported ' + outcome[1]}
            database.execute('INSERT OR IGNORE INTO pending VALUES(?,?)', (payload['event_key'], json.dumps(payload)))
    database.commit()

def drain(database, endpoint, token):
    for key, payload in database.execute('SELECT event_key,payload FROM pending LIMIT 100').fetchall():
        request = urllib.request.Request(endpoint, payload.encode(), {'Authorization': 'Bearer '+token, 'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(request, timeout=10) as response:
                response.read()
            database.execute('DELETE FROM pending WHERE event_key=?', (key,))
            database.commit()
        except urllib.error.HTTPError as error:
            print('Feedback pending: HTTP', error.code, file=sys.stderr)
            return False
        except OSError:
            print('Feedback pending: EDP unavailable', file=sys.stderr)
            return False
    return True

def main():
    endpoint = os.environ['EDP_URL'].rstrip('/') + '/v1/delivery-events'
    token = os.environ['EDP_REPORT_KEY']
    if not endpoint.startswith('https://') and not endpoint.startswith('http://localhost:'):
        raise SystemExit('EDP_URL requires HTTPS outside localhost')
    os.umask(0o077)
    path = os.environ.get('EDP_FEEDBACK_STATE', 'edp-feedback.db')
    database = sqlite3.connect(path)
    database.execute('PRAGMA journal_mode=WAL')
    database.execute('CREATE TABLE IF NOT EXISTS queues(queue TEXT PRIMARY KEY,message TEXT NOT NULL)')
    database.execute('CREATE TABLE IF NOT EXISTS pending(event_key TEXT PRIMARY KEY,payload TEXT NOT NULL)')
    database.commit()
    internal_relays = set(filter(None, os.environ.get('EDP_INTERNAL_RELAYS', '').split(',')))
    stop = threading.Event()
    def retry_loop():
        worker_db = sqlite3.connect(path)
        backoff = 1
        while not stop.is_set():
            ok = drain(worker_db, endpoint, token)
            backoff = 3 if ok else min(backoff*2, 60)
            stop.wait(backoff)
        worker_db.close()
    thread = threading.Thread(target=retry_loop, daemon=True)
    thread.start()
    try:
        for line in sys.stdin:
            correlate(line, database, internal_relays)
        # A finite log import gets one last attempt; failed events remain durable.
        drain(database, endpoint, token)
    finally:
        stop.set()
        thread.join(timeout=12)
        database.close()

if __name__ == '__main__':
    main()
