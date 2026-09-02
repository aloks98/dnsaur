-- +goose Up
-- Telling secondaries a zone changed: who to tell, and how each of them went.
--
-- notify_to is the target list, default empty, which means notify nobody --
-- what every zone that exists before this migration gets. Turning on an
-- outbound path and pointing every zone down it in the same release would
-- start sending traffic nobody asked for.
--
-- Its format is a comma-separated list of host[:port] [key:<tsig name>],
-- parsed by zones.ParseNotifyTo and stored in that package's canonical
-- spelling rather than as typed. The canonical form is load-bearing for the
-- same reason allow_transfer's is: tsigKeyStore.Delete matches a key name
-- inside this column in SQL, and it can only do that because the separator
-- and the name spelling are known.
--
-- The key is per target rather than per zone. zones.tsig_key_id means "the
-- key a secondary signs its transfer requests with" and is refused on a
-- primary, so a primary has no key of its own to sign a NOTIFY with -- and
-- a dnsaur secondary whose zone names a key refuses an unsigned one.
ALTER TABLE zones ADD COLUMN notify_to TEXT NOT NULL DEFAULT '';

-- One row per (zone, target): the delivery state of one target, and the
-- queue entry for it, which are the same thing.
--
-- **There is no `pending` column, and that is the design.** The serial
-- already is one: a row records what was *achieved*, and whether there is
-- work is derived by comparing notified_serial against the zone's
-- soa_serial. That is what makes the trigger self-healing -- any write that
-- advances a serial is picked up by the next pass, through the API, an
-- import, auto-PTR, a transfer install, or a path nobody has thought of yet,
-- with no call site for a future mutation to forget.
--
-- target is host:port as *written* -- 'ns2.example.com:5353', not a resolved
-- address -- and carries no key. Both are deliberate. A hostname is resolved
-- at send time so a target that moves is followed, and if the identity were
-- the resolved address a target that moved would orphan its history and
-- start a new row; the key is read from the zone's current notify_to at send
-- time, so re-keying a target keeps its history rather than orphaning it.
--
-- pending_serial is the round currently being attempted, notified_serial and
-- notified_at the last round that landed (notified_at 0 = never), attempts
-- the count within the current round, and last_error the most recent
-- failure, cleared on success so a target that recovered stops reporting a
-- problem it no longer has.
--
-- created_at exists for one line on screen: a target that has never been
-- notified has no notify date, so it is dated by when it was added. Without
-- it the only honest rendering is a blank cell, and a blank cell in a row
-- whose whole point is "nothing has happened yet" reads as missing data
-- rather than as the answer.
--
-- Who writes them: only zones.Notifier, through NoteDelivered and
-- NoteAttempt, two UPDATEs with disjoint column sets -- the same rule, for
-- the same reason, as 0010 and 0011. Reconcile is the only other writer and
-- touches neither.
--
-- Unlike zones.tsig_key_id this takes a real foreign key. 0009 declined one
-- there because that column is NOT NULL DEFAULT 0 where 0 means "no key" and
-- a foreign key skips NULL rather than zero; zone_id has no zero-means-none
-- case, and zone_records already sets the ON DELETE CASCADE precedent, so a
-- deleted zone takes its queue rows with it and no application code has to.
CREATE TABLE zone_notifies (
  id              INTEGER PRIMARY KEY,
  zone_id         INTEGER NOT NULL REFERENCES zones(id) ON DELETE CASCADE,
  target          TEXT    NOT NULL,
  pending_serial  INTEGER NOT NULL DEFAULT 0,
  notified_serial INTEGER NOT NULL DEFAULT 0,
  notified_at     INTEGER NOT NULL DEFAULT 0,
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL DEFAULT 0,
  last_error      TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL DEFAULT 0,
  UNIQUE(zone_id, target)
);
