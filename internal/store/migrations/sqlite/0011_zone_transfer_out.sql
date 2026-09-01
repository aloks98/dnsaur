-- +goose Up
-- Serving a zone to a secondary: who may ask, and how the last one that asked
-- went.
--
-- allow_transfer is the ACL, default deny. Empty means no peer may transfer
-- this zone, which is what every zone that exists before this migration gets:
-- turning on a transfer path and opening every zone through it in the same
-- release would be a silent change of what the server discloses.
--
-- Its format is a comma-separated list of address, CIDR, or key:<tsig name>,
-- parsed by zones.ParseACL and stored in that package's canonical spelling
-- rather than as typed. The canonical form is load-bearing: tsigKeyStore.Delete
-- matches a key name inside this column in SQL, and it can only do that
-- exactly because the separator and the name spelling are known.
--
-- The other three are the outbound twin of last_error/last_attempt from 0010,
-- and they are separate columns rather than a reuse of those two because the
-- questions are different: 0010's pair is "did our pull from our primary
-- work", these are "did a peer's pull from us work". A secondary that both
-- pulls and serves has an answer to each, and one pair could not hold both.
--
-- last_xfr_error is '' when the last attempt was served and the refusal
-- reason when it was not -- cleared on success, so it cannot become a
-- tombstone of a problem fixed weeks ago. last_xfr_peer is the address that
-- asked, which is the thing an operator is actually trying to identify when a
-- secondary is not updating. last_xfr_at is unix ms; 0 = never asked.
--
-- Who writes them: only zones.TransferServer, through
-- ZoneStore.NoteTransferRequest, a three-column UPDATE that touches nothing
-- else -- not updateZoneSQL and not ReplaceRecords, both of which bind every
-- configuration column and would revert a concurrent edit. The same rule, for
-- the same reason, as 0010.
ALTER TABLE zones ADD COLUMN allow_transfer TEXT NOT NULL DEFAULT '';
ALTER TABLE zones ADD COLUMN last_xfr_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE zones ADD COLUMN last_xfr_peer TEXT NOT NULL DEFAULT '';
ALTER TABLE zones ADD COLUMN last_xfr_error TEXT NOT NULL DEFAULT '';
