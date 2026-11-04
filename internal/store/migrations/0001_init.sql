-- Datas e horários: TEXT. Instantes em RFC 3339 UTC; dias locais em YYYY-MM-DD.
CREATE TABLE sources (
    id             INTEGER PRIMARY KEY,
    kind           TEXT NOT NULL,
    name           TEXT NOT NULL UNIQUE,
    config_json    TEXT NOT NULL DEFAULT '{}',
    credential_env TEXT NOT NULL DEFAULT '',
    enabled        INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT NOT NULL
);

CREATE TABLE weather_locations (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL UNIQUE,
    latitude     REAL,
    longitude    REAL,
    timezone     TEXT NOT NULL,
    station_code TEXT NOT NULL DEFAULT ''
);

CREATE TABLE plants (
    id                  INTEGER PRIMARY KEY,
    source_id           INTEGER NOT NULL REFERENCES sources(id),
    external_id         TEXT NOT NULL,
    name                TEXT NOT NULL,
    timezone            TEXT NOT NULL,
    latitude            REAL,
    longitude           REAL,
    peak_power_kw       REAL,
    weather_location_id INTEGER REFERENCES weather_locations(id),
    created_at          TEXT NOT NULL,
    UNIQUE (source_id, external_id)
);

CREATE TABLE devices (
    id            INTEGER PRIMARY KEY,
    plant_id      INTEGER NOT NULL REFERENCES plants(id),
    external_id   TEXT NOT NULL,
    name          TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'inverter',
    capacity_kw   REAL,
    panel_area_m2 REAL,
    efficiency    REAL,
    online        INTEGER,
    last_seen_at  TEXT,
    UNIQUE (plant_id, external_id)
);

CREATE TABLE raw_payloads (
    id          INTEGER PRIMARY KEY,
    source_id   INTEGER NOT NULL REFERENCES sources(id),
    endpoint    TEXT NOT NULL,
    params_json TEXT NOT NULL DEFAULT '{}',
    fetched_at  TEXT NOT NULL,
    body        BLOB NOT NULL
);

-- value é imutável depois da ingestão. Só quality e quality_reason mudam.
CREATE TABLE readings (
    id             INTEGER PRIMARY KEY,
    device_id      INTEGER NOT NULL REFERENCES devices(id),
    metric         TEXT NOT NULL,
    period_start   TEXT NOT NULL,
    period_end     TEXT NOT NULL,
    value          REAL,
    raw_value      TEXT NOT NULL DEFAULT '',
    quality        TEXT NOT NULL DEFAULT 'unchecked'
                   CHECK (quality IN ('unchecked', 'ok', 'suspect', 'invalid')),
    quality_reason TEXT NOT NULL DEFAULT '',
    raw_payload_id INTEGER REFERENCES raw_payloads(id),
    ingested_at    TEXT NOT NULL,
    UNIQUE (device_id, metric, period_start, period_end)
);
CREATE INDEX idx_readings_period ON readings (period_start, device_id);

CREATE TRIGGER readings_value_immutable
BEFORE UPDATE OF value, raw_value ON readings
WHEN OLD.value IS NOT NEW.value OR OLD.raw_value IS NOT NEW.raw_value
BEGIN
    SELECT RAISE(ABORT, 'readings.value is immutable');
END;

-- Valor estimado para um período, sempre separado da medição.
CREATE TABLE imputations (
    id           INTEGER PRIMARY KEY,
    device_id    INTEGER NOT NULL REFERENCES devices(id),
    metric       TEXT NOT NULL,
    period_start TEXT NOT NULL,
    period_end   TEXT NOT NULL,
    value        REAL NOT NULL,
    method       TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    UNIQUE (device_id, metric, period_start, period_end, method)
);

-- kind: observed ou forecast.
CREATE TABLE weather_days (
    location_id       INTEGER NOT NULL REFERENCES weather_locations(id),
    date              TEXT NOT NULL,
    kind              TEXT NOT NULL CHECK (kind IN ('observed', 'forecast')),
    provider          TEXT NOT NULL,
    irradiation_kwh_m2 REAL,
    temperature_mean_c REAL,
    temperature_max_c  REAL,
    precipitation_mm   REAL,
    humidity_mean_pct  REAL,
    fetched_at        TEXT NOT NULL,
    PRIMARY KEY (location_id, date, kind, provider)
);

CREATE TABLE model_runs (
    id           INTEGER PRIMARY KEY,
    name         TEXT NOT NULL,
    version      TEXT NOT NULL,
    plant_id     INTEGER REFERENCES plants(id),
    trained_at   TEXT NOT NULL,
    train_start  TEXT,
    train_end    TEXT,
    test_start   TEXT,
    test_end     TEXT,
    metrics_json TEXT NOT NULL DEFAULT '{}',
    params_json  TEXT NOT NULL DEFAULT '{}',
    active       INTEGER NOT NULL DEFAULT 0,
    UNIQUE (name, version, plant_id)
);

-- date é o dia local da planta. kwh_expected é geração esperada, não medida.
CREATE TABLE predictions (
    id           INTEGER PRIMARY KEY,
    device_id    INTEGER NOT NULL REFERENCES devices(id),
    date         TEXT NOT NULL,
    model_run_id INTEGER NOT NULL REFERENCES model_runs(id),
    kwh_expected REAL NOT NULL,
    kwh_lower    REAL,
    kwh_upper    REAL,
    created_at   TEXT NOT NULL,
    UNIQUE (device_id, date, model_run_id)
);

CREATE TABLE alerts (
    id            INTEGER PRIMARY KEY,
    plant_id      INTEGER NOT NULL REFERENCES plants(id),
    device_id     INTEGER REFERENCES devices(id),
    rule          TEXT NOT NULL,
    severity      TEXT NOT NULL CHECK (severity IN ('info', 'warning', 'critical')),
    status        TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'ack', 'resolved')),
    dedupe_key    TEXT NOT NULL,
    title         TEXT NOT NULL,
    evidence_json TEXT NOT NULL DEFAULT '{}',
    first_date    TEXT NOT NULL,
    last_date     TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    note          TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX idx_alerts_active ON alerts (dedupe_key) WHERE status IN ('open', 'ack');
CREATE INDEX idx_alerts_plant ON alerts (plant_id, status);

CREATE TABLE alert_rule_config (
    rule        TEXT NOT NULL,
    plant_id    INTEGER REFERENCES plants(id),
    enabled     INTEGER NOT NULL DEFAULT 1,
    params_json TEXT NOT NULL DEFAULT '{}',
    UNIQUE (rule, plant_id)
);

-- author: system, ai ou user. A IA só pode criar draft.
CREATE TABLE reports (
    id           INTEGER PRIMARY KEY,
    plant_id     INTEGER REFERENCES plants(id),
    kind         TEXT NOT NULL,
    period_start TEXT NOT NULL,
    period_end   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'final')),
    author       TEXT NOT NULL CHECK (author IN ('system', 'ai', 'user')),
    title        TEXT NOT NULL,
    body_md      TEXT NOT NULL,
    data_json    TEXT NOT NULL DEFAULT '{}',
    created_at   TEXT NOT NULL
);

CREATE TABLE fetch_tasks (
    id          INTEGER PRIMARY KEY,
    source_id   INTEGER NOT NULL REFERENCES sources(id),
    task_key    TEXT NOT NULL,
    task_json   TEXT NOT NULL,
    priority    INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'done', 'failed')),
    not_before  TEXT NOT NULL,
    attempts    INTEGER NOT NULL DEFAULT 0,
    last_error  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE (source_id, task_key)
);
CREATE INDEX idx_fetch_tasks_due ON fetch_tasks (status, not_before, priority);

-- Sobrevive a reinícios: evita gastar o limite do provedor depois de um restart.
CREATE TABLE rate_limits (
    source_id       INTEGER NOT NULL REFERENCES sources(id),
    limit_key       TEXT NOT NULL,
    next_allowed_at TEXT NOT NULL,
    PRIMARY KEY (source_id, limit_key)
);

CREATE TABLE daily_budgets (
    source_id  INTEGER NOT NULL REFERENCES sources(id),
    budget_key TEXT NOT NULL,
    day        TEXT NOT NULL,
    used       INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (source_id, budget_key, day)
);
