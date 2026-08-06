-- +goose Up
-- Filter-list fetch/parse outcome, so a list that isn't working says so
-- instead of being indistinguishable from one that simply hasn't refreshed
-- yet. Before this, a 404 and a brand-new subscription were byte-for-byte
-- identical on the wire: both `entry_count 0, last_refreshed 0`.
--
-- last_status is the authoritative state — 'pending' | 'ok' | 'stale' |
-- 'failed' | 'empty' (see store.ListStatus* and the List doc comment).
-- It is stored rather than derived from the numbers because the four
-- outcomes are genuinely not recoverable from them: 'failed' and 'empty'
-- both leave entry_count 0, and inferring one from a last_refreshed /
-- last_attempt comparison is exactly the kind of implicit coupling that
-- hid this bug in the first place.
--
-- last_error is '' on success (TouchList clears it, so a recovered list
-- stops reporting one) and a short human-readable reason otherwise —
-- "404 Not Found", "dial tcp: no such host", "fetched 4.5 MB, no usable
-- entries — 250,431 lines skipped".
-- last_attempt is unix ms like last_refreshed (BIGINT here, INTEGER on
-- sqlite), 0 = never attempted.
ALTER TABLE lists ADD COLUMN last_error TEXT NOT NULL DEFAULT '';
ALTER TABLE lists ADD COLUMN last_attempt BIGINT NOT NULL DEFAULT 0;
ALTER TABLE lists ADD COLUMN last_status TEXT NOT NULL DEFAULT 'pending';

-- Rows that already refreshed successfully under 0001/0002 never recorded an
-- attempt or a status; seed both from the refresh they did complete so they
-- don't read as "never attempted" after upgrading. Rows still at
-- last_refreshed 0 genuinely never ran, and keep the 'pending' default.
UPDATE lists SET last_attempt = last_refreshed, last_status = 'ok' WHERE last_refreshed > 0;

-- A readable label, so the UI can stop using the raw URL as the identifier in
-- assignment menus, toasts and confirmation dialogs. Optional on input:
-- store.AddList fills a blank one from store.DeriveListName, and reads derive
-- the same way for rows written before this column existed, so there is one
-- implementation of the naming rule and it lives in Go where the URL can
-- actually be parsed. Hence no SQL backfill here.
ALTER TABLE lists ADD COLUMN name TEXT NOT NULL DEFAULT '';
