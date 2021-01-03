CREATE TABLE jobs(
        id uuid NOT NULL PRIMARY KEY,
        type varchar NOT NULL,
        args jsonb,
        result jsonb,
        queued_at timestamp NOT NULL,
        started_at timestamp,
        finished_at timestamp,
        canceled boolean NOT NULL DEFAULT FALSE,

        -- this is ok when canceled, because started_at must be set
        CONSTRAINT started_before_finished
          CHECK (finished_at IS NULL OR started_at IS NOT NULL),

        CONSTRAINT finished_when_canceled
          CHECK (finished_at IS NOT NULL or canceled = FALSE)

        -- TODO additional constraints: result must be set when finished, but not when canceled
        --                              queued_at <= started_at (if !null) <= finished_at
);

CREATE TABLE job_dependencies(


        -- TODO maybe we don't want CASCADE but some kind of error?

        job_id uuid REFERENCES jobs(id) ON DELETE CASCADE,
        dependency_id uuid REFERENCES jobs(id) ON DELETE CASCADE
);

CREATE VIEW ready_jobs AS
  SELECT *
  FROM jobs
  -- NOTE: no need to check for cancel, because finished_at is true when canceled is set
  WHERE started_at IS NULL
    AND id NOT IN (
      SELECT job_id
      FROM job_dependencies JOIN jobs ON dependency_id = id
      WHERE finished_at IS NULL
    )
  ORDER BY queued_at ASC
