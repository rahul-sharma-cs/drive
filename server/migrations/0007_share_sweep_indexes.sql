-- The three indexes the share sweeps ride. 0001 gave share_guest_sessions
-- its token_hash lookup and share_access_log its per-share listing; the
-- deletes had nothing.
--
-- 1. share_guest_sessions (share_id): the store's deleteGuests, which
--    regenerate, revoke and a raised password gate all run, and the ON DELETE
--    CASCADE from shares behind a purge.
-- 2. share_guest_sessions (expires_at): the GC's hourly
--    DELETE ... WHERE expires_at < now().
-- 3. share_access_log (at): the GC's retention sweep,
--    DELETE ... WHERE at < now() - <age>. The first two scan a table bounded
--    by a 30-minute TTL; this one scanned every row an anonymous caller could
--    add for 90 days, once an hour.
--
-- Plain btrees, no predicate: each query is a range or an equality on the one
-- column, with no WHERE the planner would have to prove. Not CONCURRENTLY --
-- goose wraps a migration in a transaction -- so they build under SHARE,
-- milliseconds at the size these tables are.

-- +goose Up

CREATE INDEX share_guest_sessions_share_id_idx
    ON share_guest_sessions (share_id);

CREATE INDEX share_guest_sessions_expires_at_idx
    ON share_guest_sessions (expires_at);

CREATE INDEX share_access_log_at_idx
    ON share_access_log (at);

-- +goose Down

DROP INDEX IF EXISTS share_access_log_at_idx;
DROP INDEX IF EXISTS share_guest_sessions_expires_at_idx;
DROP INDEX IF EXISTS share_guest_sessions_share_id_idx;
