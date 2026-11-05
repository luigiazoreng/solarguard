-- Contas do painel. Cada fonte pertence a uma conta, e as plantas herdam o dono da fonte.
-- Uma fonte sem dono não aparece no painel.
CREATE TABLE users (
    id            INTEGER PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

ALTER TABLE sources ADD COLUMN user_id INTEGER REFERENCES users(id);
-- Token criptografado com SOLARGUARD_SECRET_KEY. Nunca guardar o token em claro.
ALTER TABLE sources ADD COLUMN credential_enc TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_sources_user ON sources (user_id);
