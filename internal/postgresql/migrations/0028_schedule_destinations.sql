-- 0028: where an administrator chose, in the UI, to send the reports of a
-- schedule that is managed outside the UI (docs/schedules.md, "Changing the
-- channel of a schedule managed in Git"). It replaces the destination of
-- spec.result.notify; Git keeps the rest of the schedule.
CREATE TABLE IF NOT EXISTS schedule_destinations (
    tenant_id    TEXT NOT NULL,
    namespace    TEXT NOT NULL,
    name         TEXT NOT NULL,
    app          TEXT NOT NULL,
    channel_id   TEXT NOT NULL,
    channel_name TEXT NOT NULL DEFAULT '',
    set_by       TEXT NOT NULL,
    set_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, namespace, name)
);

ALTER TABLE schedule_destinations ENABLE ROW LEVEL SECURITY;
ALTER TABLE schedule_destinations FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON schedule_destinations USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
