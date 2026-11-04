-- Garante isolamento entre plantas para alertas no nível de usina (device_id NULL).
DROP INDEX IF EXISTS idx_alerts_active;
CREATE UNIQUE INDEX idx_alerts_active ON alerts (plant_id, dedupe_key) WHERE status IN ('open', 'ack');
