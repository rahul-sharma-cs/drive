-- Two things the code already relies on, written down where they hold.
--
-- 1. upload_sessions.object_key is unique.
--
-- The GC's orphan-multipart claim is on the object key alone -- "does any
-- active or completing session own this key?" -- and deliberately not on the
-- upload id, because R2 re-mints the UploadId on every response, so a claim
-- that compared ids could never match there and every long-running resumable
-- upload past the orphan grace would be aborted. That narrower predicate is
-- sound only while one key belongs to one session: two sessions sharing a key
-- would let a dead one vouch for a live one's multipart, or the reverse.
--
-- Nothing can produce a duplicate today -- keys come from upload.NewObjectKey,
-- a fresh uuid, at its single call site, and no path (resume, the insert race,
-- expiry retire, finalize) ever rewrites one -- so this costs nothing to hold
-- and turns a convention the collector depends on into a guarantee. It also
-- means an accidental reuse fails at the INSERT instead of silently at the next
-- sweep. A violation here is NOT the create race: that one names
-- upload_sessions_active_fingerprint_idx and is handled as ErrRace.
--
-- 2. The trash's own index.
--
-- ListTrash and TrashRootIDs share one predicate -- owner_id AND trashed_root
-- AND deleted_at IS NOT NULL -- and order by (deleted_at, id): descending for
-- the listing, ascending for emptying, both of which a single btree serves
-- (backward scan). With only nodes_owner_id_idx to help, each 200-root page
-- re-read every node the owner has ever trashed and top-N-sorted it.
--
-- The partial predicate is written exactly as node/trash.go writes it. The
-- planner only uses a partial index when it can prove the query implies the
-- index predicate, so a rewrite of either text has to move both.

-- +goose Up

ALTER TABLE upload_sessions
    ADD CONSTRAINT upload_sessions_object_key_key UNIQUE (object_key);

CREATE INDEX nodes_trash_roots_idx
    ON nodes (owner_id, deleted_at, id)
 WHERE trashed_root AND deleted_at IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS nodes_trash_roots_idx;

ALTER TABLE upload_sessions
    DROP CONSTRAINT IF EXISTS upload_sessions_object_key_key;
