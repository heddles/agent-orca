-- 0016: the LLM judge and human review of evaluation results
-- (docs/evaluations.md, docs/proposals/evaluations.md sections 4 and 7).
--
-- eval_grades (0014) gains what a grade that is not a plain pass or fail needs:
--   status          ok (graded), error (the grader could not run), invalid (a judge
--                   reply that was not usable), pending_review (a human has not
--                   labelled it yet). Only ok grades count towards a case.
--   judge_cost_usd  what the judge call cost, counted in the experiment's spent_usd.
--   graded_by       who labelled it, for a human grade; '' otherwise.
--   inconsistent    a pairwise judge that disagreed with itself when the answers
--                   swapped places (the grade is then a tie).
--   created_at      when the grade was written, so the tenant's daily budget can
--                   count the judge spend of today.
--
-- eval_review_assignments is the human review queue: one row per (result, human
-- grader) that needs a label. The reviewer sees the row's case and output, never
-- the arm. order_key is a random number the queue is ordered by, so the order
-- carries no information about the arm either. The label itself is stored in
-- eval_grades (score, pass, rationale = the comment, graded_by).
--
-- Both carry tenant_id with the row-level security of 0010, and the composite
-- foreign keys make the database refuse an assignment under another tenant than
-- its experiment, result or grader.

ALTER TABLE eval_grades ADD COLUMN IF NOT EXISTS status         TEXT NOT NULL DEFAULT 'ok';
ALTER TABLE eval_grades ADD COLUMN IF NOT EXISTS judge_cost_usd NUMERIC(14,8) NOT NULL DEFAULT 0;
ALTER TABLE eval_grades ADD COLUMN IF NOT EXISTS graded_by      TEXT NOT NULL DEFAULT '';
ALTER TABLE eval_grades ADD COLUMN IF NOT EXISTS inconsistent   BOOLEAN NOT NULL DEFAULT FALSE;
ALTER TABLE eval_grades ADD COLUMN IF NOT EXISTS created_at     TIMESTAMPTZ NOT NULL DEFAULT now();

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'eval_grades_status_check') THEN
        ALTER TABLE eval_grades ADD CONSTRAINT eval_grades_status_check
            CHECK (status IN ('ok','error','invalid','pending_review'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'eval_grades_judge_cost_check') THEN
        ALTER TABLE eval_grades ADD CONSTRAINT eval_grades_judge_cost_check CHECK (judge_cost_usd >= 0);
    END IF;
END $$;

-- What the judge's canary check cost, which belongs to no grade. It is part of
-- spent_usd too; it is kept apart so the tenant's daily budget can count it.
ALTER TABLE eval_experiments ADD COLUMN IF NOT EXISTS canary_usd NUMERIC(14,8) NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_eval_grades_judge_cost ON eval_grades (tenant_id, created_at) WHERE judge_cost_usd > 0;

CREATE TABLE IF NOT EXISTS eval_review_assignments (
    id            BIGSERIAL PRIMARY KEY,
    tenant_id     TEXT NOT NULL,
    experiment_id BIGINT NOT NULL,
    result_id     BIGINT NOT NULL,
    grader_id     BIGINT NOT NULL,
    order_key     BIGINT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','done','closed')),
    reviewer      TEXT NOT NULL DEFAULT '',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    labelled_at   TIMESTAMPTZ,
    UNIQUE (result_id, grader_id),
    UNIQUE (id, tenant_id),
    FOREIGN KEY (experiment_id, tenant_id) REFERENCES eval_experiments (id, tenant_id) ON DELETE CASCADE,
    FOREIGN KEY (result_id, tenant_id)     REFERENCES eval_results (id, tenant_id)     ON DELETE CASCADE,
    FOREIGN KEY (grader_id, tenant_id)     REFERENCES eval_graders (id, tenant_id)     ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_eval_review_queue ON eval_review_assignments (tenant_id, experiment_id, status, order_key);

ALTER TABLE eval_review_assignments ENABLE ROW LEVEL SECURITY;
ALTER TABLE eval_review_assignments FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON eval_review_assignments USING (agentorca_tenant_visible(tenant_id)) WITH CHECK (agentorca_tenant_visible(tenant_id));
