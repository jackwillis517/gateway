CREATE TABLE IF NOT EXISTS users (
    id bigserial PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    name text NOT NULL,
    email citext UNIQUE NOT NULL,
    -- LGF password type; seeded by 000003 (bcrypt cost 12)
    password_hash bytea NOT NULL,
    activated boolean NOT NULL DEFAULT true,
    version integer NOT NULL DEFAULT 1   -- LGF optimistic concurrency
);
