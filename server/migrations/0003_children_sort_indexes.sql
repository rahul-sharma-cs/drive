-- Sorting a folder's children by date or by size.
--
-- The listing's keyset is (rank, key, lower(name), id) with rank the
-- folders-first CASE, so an index that serves it has to carry the same
-- expressions in the same order -- and the expressions have to be written the
-- way node/store.go writes them, or the planner matches neither.
--
-- Name needs nothing new: nodes_parent_name_idx already covers (parent_id,
-- lower(name)) over live rows.
--
-- These serve ascending pages. "rank ASC, key DESC" is neither a btree's
-- forward scan nor its backward one, so a descending page uses the
-- (parent_id, rank) prefix and sorts the folder's own rows -- fine at the sizes
-- a folder actually reaches, and two more indexes on every insert are not free.
-- The DESC twins are a measurement away if a folder ever gets big enough.

-- +goose Up

CREATE INDEX nodes_children_updated_at_idx
    ON nodes (parent_id, (CASE kind WHEN 'folder' THEN 0 ELSE 1 END), updated_at, lower(name), id)
 WHERE deleted_at IS NULL;

CREATE INDEX nodes_children_size_idx
    ON nodes (parent_id, (CASE kind WHEN 'folder' THEN 0 ELSE 1 END), coalesce(size, 0), lower(name), id)
 WHERE deleted_at IS NULL;

-- +goose Down

DROP INDEX IF EXISTS nodes_children_size_idx;
DROP INDEX IF EXISTS nodes_children_updated_at_idx;
