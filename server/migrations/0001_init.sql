-- Drive's complete schema.
-- Every table the product needs lives here, including the ones nothing reads
-- yet (shares, personal access tokens), so a feature landing later never needs
-- a migration for a table that was already specified.
--
-- FK ON DELETE is explicit everywhere: the share children CASCADE, the audit
-- log and the upload session's node/parent pointers SET NULL, and every other
-- foreign key is NO ACTION on purpose.

-- +goose Up

CREATE EXTENSION IF NOT EXISTS citext;
CREATE EXTENSION IF NOT EXISTS pg_trgm;

-- ---------------------------------------------------------------- identity --

CREATE TABLE users (
    id                uuid PRIMARY KEY,
    email             citext NOT NULL UNIQUE,
    password_hash     text NOT NULL,
    display_name      text NOT NULL,
    email_verified_at timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now()
);

-- Server-side and therefore revocable: the cookie carries a raw token whose
-- sha256 is stored here.
CREATE TABLE auth_sessions (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    token_hash   bytea NOT NULL UNIQUE,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_seen_at timestamptz,
    ip           inet,
    user_agent   text
);

CREATE INDEX auth_sessions_user_id_idx ON auth_sessions (user_id);

CREATE TABLE email_tokens (
    id         uuid PRIMARY KEY,
    user_id    uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    purpose    text NOT NULL CHECK (purpose IN ('verify', 'reset')),
    token_hash bytea NOT NULL,
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX email_tokens_token_hash_idx ON email_tokens (token_hash);

-- ------------------------------------------------------------------ blobs ---

-- A blob is one object in Garage. Copies share it via refcount; purge only
-- decrements. The GC sweep deletes rows at refcount 0 past a 2 h grace and
-- then the object -- nothing else ever calls DeleteObject.
CREATE TABLE blobs (
    id         uuid PRIMARY KEY,
    object_key text NOT NULL UNIQUE,
    size       bigint NOT NULL,
    sha256     bytea,
    etag       text,
    refcount   int NOT NULL DEFAULT 1,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- ------------------------------------------------------------------ nodes ---

-- One root folder per user, created in the signup transaction
-- (kind='folder', parent_id NULL, name 'My Drive'). parent_id is NEVER NULL
-- for real content. Folder rows carry size NULL.
CREATE TABLE nodes (
    id           uuid PRIMARY KEY,
    owner_id     uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    parent_id    uuid REFERENCES nodes(id) ON DELETE NO ACTION,
    kind         text NOT NULL CHECK (kind IN ('file', 'folder')),
    name         text NOT NULL,
    blob_id      uuid REFERENCES blobs(id) ON DELETE NO ACTION,
    size         bigint,
    mime         text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    deleted_at   timestamptz,
    trashed_root boolean NOT NULL DEFAULT false
);

-- Exactly one root folder per user.
CREATE UNIQUE INDEX nodes_one_root_per_owner_idx
    ON nodes (owner_id) WHERE parent_id IS NULL;

-- Names are unique per folder among live rows only; trashed siblings do not
-- block a new file of the same name.
CREATE UNIQUE INDEX nodes_parent_name_idx
    ON nodes (parent_id, lower(name)) WHERE deleted_at IS NULL;

-- Unpartial on purpose: the trash restore/purge CTEs traverse deleted rows.
CREATE INDEX nodes_parent_id_idx ON nodes (parent_id);
CREATE INDEX nodes_owner_id_idx ON nodes (owner_id);

CREATE INDEX nodes_name_trgm_idx ON nodes USING gin (name gin_trgm_ops);

-- --------------------------------------------------------- upload ledger ----

CREATE TABLE upload_sessions (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    parent_id       uuid REFERENCES nodes(id) ON DELETE SET NULL,
    file_name       text NOT NULL,
    file_size       bigint NOT NULL,
    mime            text,
    fingerprint     text NOT NULL,
    conflict_policy text CHECK (conflict_policy IN ('replace', 'rename', 'reuse')),
    -- NULL for the 0-byte special case, which skips CreateMultipartUpload.
    s3_upload_id    text,
    object_key      text NOT NULL,
    part_size       bigint NOT NULL,
    parts_total     int NOT NULL,
    status          text NOT NULL CHECK (status IN ('active', 'completing', 'done', 'aborted')),
    mode            text NOT NULL DEFAULT 'direct' CHECK (mode IN ('direct', 'relay')),
    -- Non-NULL = chimera verification armed: the pinned part numbers resume
    -- must see client MD5s for.
    verify_parts    int[],
    node_id         uuid REFERENCES nodes(id) ON DELETE SET NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    -- now() + 7 days, refreshed on every authenticated touch of the session.
    expires_at      timestamptz NOT NULL
);

-- One live session per (user, file, destination): a second create for the same
-- file resumes instead of duplicating.
CREATE UNIQUE INDEX upload_sessions_active_fingerprint_idx
    ON upload_sessions (user_id, fingerprint, parent_id) WHERE status = 'active';

CREATE INDEX upload_sessions_user_id_idx ON upload_sessions (user_id);
CREATE INDEX upload_sessions_status_expires_at_idx ON upload_sessions (status, expires_at);

CREATE TABLE upload_parts (
    session_id   uuid NOT NULL REFERENCES upload_sessions(id) ON DELETE NO ACTION,
    part_number  int NOT NULL,
    size         bigint NOT NULL,
    etag         text NOT NULL,
    -- NULL for rows upserted from ListParts reconciliation (the kill-9 window).
    md5          text,
    confirmed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, part_number)
);

-- ----------------------------------------------------------------- shares ---

-- password_hash is only valid when mode='public'. MVP shares target files only
-- and validate permission='view'; the 'edit' value stays for V2.
CREATE TABLE shares (
    id             uuid PRIMARY KEY,
    node_id        uuid NOT NULL REFERENCES nodes(id) ON DELETE NO ACTION,
    created_by     uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    mode           text NOT NULL CHECK (mode IN ('public', 'restricted')),
    token_hash     bytea NOT NULL UNIQUE,
    permission     text NOT NULL DEFAULT 'view' CHECK (permission IN ('view', 'edit')),
    password_hash  text,
    expires_at     timestamptz,
    max_downloads  int,
    download_count int NOT NULL DEFAULT 0,
    revoked_at     timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX shares_node_id_idx ON shares (node_id);

CREATE TABLE share_allowlist (
    share_id uuid NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    email    citext NOT NULL,
    PRIMARY KEY (share_id, email)
);

-- Exactly one live code per (share_id, email): request-otp deletes prior
-- unconsumed rows before inserting. Budgets live in throttle, never here.
CREATE TABLE share_otps (
    id          uuid PRIMARY KEY,
    share_id    uuid NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    email       citext NOT NULL,
    code_hash   bytea NOT NULL,
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- downloaded_at implements "download cap counted once per guest session".
CREATE TABLE share_guest_sessions (
    id            uuid PRIMARY KEY,
    share_id      uuid NOT NULL REFERENCES shares(id) ON DELETE CASCADE,
    email         citext,
    token_hash    bytea NOT NULL UNIQUE,
    downloaded_at timestamptz,
    expires_at    timestamptz NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Audit rows outlive the share they describe, hence nullable + SET NULL.
CREATE TABLE share_access_log (
    id         bigserial PRIMARY KEY,
    share_id   uuid REFERENCES shares(id) ON DELETE SET NULL,
    email      citext,
    ip         inet,
    user_agent text,
    action     text NOT NULL CHECK (action IN ('view', 'download', 'denied')),
    at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX share_access_log_share_id_id_idx ON share_access_log (share_id, id DESC);

-- ------------------------------------------------------- personal tokens ----

-- No endpoint reads these yet. Token format: 'drv_' + >=30 base62 random +
-- 6-char base62 CRC32
-- checksum; SHA-256 at rest, plaintext shown exactly once. Scopes are
-- 'files:read' | 'files:write' only, read-only by default.
CREATE TABLE api_tokens (
    id           uuid PRIMARY KEY,
    user_id      uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    name         text NOT NULL,
    token_hash   bytea NOT NULL UNIQUE,
    scopes       text[] NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    expires_at   timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at   timestamptz
);

CREATE INDEX api_tokens_user_id_idx ON api_tokens (user_id);

-- --------------------------------------------------------------- throttle ---

-- Every durable budget: otp_request, otp_verify_fail, otp_send_share,
-- share_password, login, email_send. GC prunes expired windows.
CREATE TABLE throttle (
    scope        text NOT NULL,
    key          text NOT NULL,
    window_start timestamptz NOT NULL,
    count        int NOT NULL DEFAULT 0,
    PRIMARY KEY (scope, key, window_start)
);

-- +goose Down

DROP TABLE IF EXISTS throttle;
DROP TABLE IF EXISTS api_tokens;
DROP TABLE IF EXISTS share_access_log;
DROP TABLE IF EXISTS share_guest_sessions;
DROP TABLE IF EXISTS share_otps;
DROP TABLE IF EXISTS share_allowlist;
DROP TABLE IF EXISTS shares;
DROP TABLE IF EXISTS upload_parts;
DROP TABLE IF EXISTS upload_sessions;
DROP TABLE IF EXISTS nodes;
DROP TABLE IF EXISTS blobs;
DROP TABLE IF EXISTS email_tokens;
DROP TABLE IF EXISTS auth_sessions;
DROP TABLE IF EXISTS users;
