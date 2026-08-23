-- The two indexes share links read, both partial on revoked_at IS NULL --
-- because a revoked share is kept, not deleted: share_access_log points at it
-- and the owner's history has to stay attributable after the link is gone.
--
-- 1. One active link per file.
--
-- shares_node_active_idx is the rule itself, not an accelerator. The create
-- path is a single INSERT ... ON CONFLICT (node_id) WHERE revoked_at IS NULL
-- DO NOTHING, and zero rows back means "a link already exists" whether it was
-- made last week or by a concurrent request a millisecond earlier; there is
-- no pre-check and no window between checking and inserting. The inference
-- clause has to repeat this predicate word for word, which is what a partial
-- unique index requires. A plain unique index on node_id would refuse the
-- second link forever, revoked or not, and shares_node_id_idx from 0001 stays
-- for the per-file lookups that want every row.
--
-- 2. The owner's list.
--
-- The shared-links page is a keyset listing of one owner's active links,
-- newest first, on (created_at DESC, id DESC). Without this every page would
-- read every share the owner has ever made and top-N-sort it. The predicate is
-- the listing's own WHERE, written as the store writes it: the planner uses a
-- partial index only when it can prove the query implies the predicate, so a
-- rewrite of either text has to move both.
--
-- Neither is CONCURRENTLY -- goose wraps a migration in a transaction, which
-- rules it out -- so both build under SHARE on shares: reads continue, writes
-- wait, milliseconds at the size the table is. The unique one can fail only on
-- data that already violates it, and nothing writes a share before this lands.

-- +goose Up

CREATE UNIQUE INDEX shares_node_active_idx
    ON shares (node_id)
 WHERE revoked_at IS NULL;

CREATE INDEX shares_owner_active_idx
    ON shares (created_by, created_at DESC, id DESC)
 WHERE revoked_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS shares_owner_active_idx;
DROP INDEX IF EXISTS shares_node_active_idx;
