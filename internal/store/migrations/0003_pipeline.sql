-- Histórico de execuções da pipeline noturna e manual por planta.
CREATE TABLE pipeline_runs (
    id          INTEGER PRIMARY KEY,
    plant_id    INTEGER NOT NULL REFERENCES plants(id),
    local_date  TEXT NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('success', 'partial', 'failed')),
    steps_json  TEXT NOT NULL DEFAULT '[]',
    started_at  TEXT NOT NULL,
    finished_at TEXT NOT NULL
);

CREATE INDEX idx_pipeline_runs_plant_date ON pipeline_runs (plant_id, local_date);
CREATE INDEX idx_pipeline_runs_started ON pipeline_runs (started_at DESC);
