-- The blob grace period has to run from the moment a blob's last reference
-- went, not from when the blob was created.
--
-- Measured from created_at, any blob older than the grace was collectible the
-- instant its refcount hit zero -- so a download URL issued a minute earlier
-- (TTL 1 h) could 404 mid-transfer, and a file uploaded yesterday and trashed
-- today lost its bytes with no grace at all. unreferenced_at is stamped by the
-- purge decrement that takes a refcount to zero and cleared by the copy
-- increment that brings it back, and the sweep reads it instead.

-- +goose Up

ALTER TABLE blobs ADD COLUMN unreferenced_at timestamptz;

-- Rows already at zero have no record of when they got there; starting their
-- grace now is the safe direction (it delays a delete, never advances one).
UPDATE blobs SET unreferenced_at = now() WHERE refcount = 0;

CREATE INDEX blobs_unreferenced_at_idx ON blobs (unreferenced_at)
    WHERE refcount = 0 AND unreferenced_at IS NOT NULL;

-- +goose Down

DROP INDEX IF EXISTS blobs_unreferenced_at_idx;
ALTER TABLE blobs DROP COLUMN IF EXISTS unreferenced_at;
