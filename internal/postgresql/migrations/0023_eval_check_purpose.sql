-- 0023: the builder's "Check with my cases". An experiment of purpose 'check'
-- runs a draft agent (an inline spec carried by one arm) against a dataset,
-- optionally against the first max_cases cases only. It is hidden from the
-- experiments list and is never a publish gate's evidence.
ALTER TABLE eval_experiments DROP CONSTRAINT IF EXISTS eval_experiments_purpose_check;
ALTER TABLE eval_experiments ADD CONSTRAINT eval_experiments_purpose_check CHECK (purpose IN ('compare','gate','check'));
ALTER TABLE eval_experiments ADD COLUMN IF NOT EXISTS max_cases INTEGER CHECK (max_cases IS NULL OR max_cases > 0);
