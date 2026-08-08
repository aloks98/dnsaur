-- +goose Up
-- RFC 2308 §5: a negative answer's TTL is min(SOA.MINIMUM, the SOA record's
-- own TTL). MINIMUM is an rdata field — 0004 already stores it — while this
-- is the header TTL the SOA RR carries like any other RR. With only one
-- column the two can never differ, the min() is degenerate, and a zone file
-- cannot round-trip its SOA (Milestone C's export needs the header TTL).
--
-- 900 matches soa_minimum's default, so existing rows keep exactly the
-- negative TTL they hand out today.
ALTER TABLE zones ADD COLUMN soa_ttl BIGINT NOT NULL DEFAULT 900;
