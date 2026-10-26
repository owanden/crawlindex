CREATE TABLE IF NOT EXISTS domains (
    origin text PRIMARY KEY,
    next_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS jobs (
    id bigserial PRIMARY KEY,
    url text NOT NULL UNIQUE,
    origin text NOT NULL REFERENCES domains(origin),
    depth integer NOT NULL CHECK (depth >= 0),
    state text NOT NULL DEFAULT 'queued' CHECK (state IN ('queued','leased','done','failed')),
    attempts integer NOT NULL DEFAULT 0,
    available_at timestamptz NOT NULL DEFAULT now(),
    lease_until timestamptz,
    last_error text NOT NULL DEFAULT '',
    updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS jobs_ready ON jobs(origin, available_at, id) WHERE state IN ('queued','leased');
CREATE TABLE IF NOT EXISTS documents (
    job_id bigint PRIMARY KEY REFERENCES jobs(id),
    url text NOT NULL UNIQUE,
    title text NOT NULL,
    body text NOT NULL,
    indexed_at timestamptz NOT NULL DEFAULT now(),
    search tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', title), 'A') ||
        setweight(to_tsvector('english', body), 'B')
    ) STORED
);
CREATE INDEX IF NOT EXISTS documents_search ON documents USING gin(search);
ALTER TABLE jobs ADD COLUMN IF NOT EXISTS discovery_rejected integer NOT NULL DEFAULT 0 CHECK (discovery_rejected >= 0);
CREATE TABLE IF NOT EXISTS admission_budget (
    id integer PRIMARY KEY CHECK (id = 1),
    max_urls bigint NOT NULL CHECK (max_urls > 0)
);
INSERT INTO admission_budget(id,max_urls)
SELECT 1,GREATEST(1000,count(*)) FROM jobs
ON CONFLICT(id) DO NOTHING;
