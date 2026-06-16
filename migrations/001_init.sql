CREATE EXTENSION IF NOT EXISTS "uuid-ossp";

-- Users
CREATE TABLE users (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    nickname    TEXT,
    avatar_url  TEXT,
    status      TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'suspended', 'deleted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Social Identities
CREATE TABLE social_identities (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider     TEXT NOT NULL CHECK (provider IN ('github', 'google', 'wechat')),
    provider_uid TEXT NOT NULL,
    email        TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, provider_uid)
);

CREATE INDEX idx_social_identities_user_id ON social_identities(user_id);
CREATE INDEX idx_social_identities_email ON social_identities(email) WHERE email IS NOT NULL;

-- Apps
CREATE TABLE apps (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    secret        TEXT NOT NULL,
    name          TEXT NOT NULL,
    redirect_uris TEXT[] NOT NULL DEFAULT '{}',
    providers     TEXT[] NOT NULL DEFAULT '{github,google,wechat}',
    default_plan  TEXT NOT NULL DEFAULT 'free' CHECK (default_plan IN ('free', 'trial', 'paid')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Subscriptions
CREATE TABLE subscriptions (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id     UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    plan       TEXT NOT NULL DEFAULT 'free' CHECK (plan IN ('free', 'trial', 'paid')),
    status     TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'expired', 'cancelled')),
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, app_id)
);

CREATE INDEX idx_subscriptions_user_id ON subscriptions(user_id);
CREATE INDEX idx_subscriptions_app_id ON subscriptions(app_id);

-- Sessions
CREATE TABLE sessions (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    app_id        UUID NOT NULL REFERENCES apps(id) ON DELETE CASCADE,
    session_type  TEXT NOT NULL DEFAULT 'refresh' CHECK (session_type IN ('auth_code', 'refresh')),
    refresh_token TEXT NOT NULL,
    scope         TEXT[] NOT NULL DEFAULT '{}',
    revoked       BOOLEAN NOT NULL DEFAULT false,
    expires_at    TIMESTAMPTZ NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_sessions_user_id ON sessions(user_id);
CREATE INDEX idx_sessions_refresh_token ON sessions(refresh_token);
