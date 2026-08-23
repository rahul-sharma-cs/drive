-- A second way into an account: an external identity provider's subject, and a
-- password column that is allowed to be empty because of it.
--
-- The two halves are one migration because they are one decision. An account
-- created by signing in with an identity provider has never chosen a password,
-- and users.password_hash NOT NULL is what would otherwise force one to be
-- invented -- and an invented hash is a credential nobody can use and nobody
-- can revoke.
--
-- Why the indexes are the shape they are:
--
--   * (provider, subject) unique is the sign-in lookup itself. The subject is
--     the provider's stable, never-reused id for a person, so it -- and not the
--     email -- is what identifies the account on every sign-in after the first.
--     Unique makes a duplicate impossible rather than merely unlikely, which is
--     what lets the callback insert with ON CONFLICT DO NOTHING and treat zero
--     rows as "somebody else won the race".
--   * (user_id, provider) unique is the shape the account screen promises: one
--     Google account per Drive account. A second link is refused as a conflict
--     rather than becoming a second row the UI would have to explain.
--   * (user_id) covers the account screen's own listing.
--
-- email_at_link is an audit fact and is never updated. It records what the
-- provider said the address was when the link was made; the provider's copy can
-- change afterwards, and users.email is never rewritten from a claim.
--
-- ON DELETE NO ACTION matches every other foreign key to users: nothing deletes
-- an account today, and a cascade written now would be a policy decided by
-- whoever writes account deletion later, in a file they would not think to read.
--
-- The Down is honest rather than convenient. Restoring NOT NULL fails if a
-- password-less account exists by then, and that failure is the correct
-- outcome: the alternative is a down-migration that deletes somebody's account
-- to make itself succeed.

-- +goose Up

CREATE TABLE user_identities (
    id            uuid PRIMARY KEY,
    user_id       uuid NOT NULL REFERENCES users(id) ON DELETE NO ACTION,
    provider      text NOT NULL CHECK (provider IN ('google')),
    subject       text NOT NULL,
    email_at_link citext NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_login_at timestamptz
);

CREATE UNIQUE INDEX user_identities_provider_subject_idx ON user_identities (provider, subject);
CREATE UNIQUE INDEX user_identities_user_provider_idx    ON user_identities (user_id, provider);
CREATE INDEX        user_identities_user_id_idx          ON user_identities (user_id);

ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;

-- +goose Down

ALTER TABLE users ALTER COLUMN password_hash SET NOT NULL;

DROP TABLE IF EXISTS user_identities;
