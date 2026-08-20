-- Forward repair for the released 225 seed backfill. Keep 225 immutable: it
-- may already be recorded by deployed databases. This only touches active
-- OpenAI OAuth accounts that explicitly opted into a convergence mode.
--
-- Imports and restores can reintroduce a malformed seed after earlier Codex
-- migrations have run. Canonical non-nil UUIDs remain opaque account state;
-- normalize their representation without rotating the converged identity.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_seed}',
    to_jsonb(LOWER(BTRIM(extra->>'codex_fingerprint_seed'))),
    true
)
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND type = 'oauth'
  AND LOWER(BTRIM(COALESCE(extra->>'codex_fingerprint_mode', ''))) IN ('device', 'session', 'full')
  AND NULLIF(BTRIM(extra->>'codex_fingerprint_seed'), '') IS NOT NULL
  AND LOWER(BTRIM(extra->>'codex_fingerprint_seed')) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  AND LOWER(BTRIM(extra->>'codex_fingerprint_seed')) <> '00000000-0000-0000-0000-000000000000'
  AND BTRIM(extra->>'codex_fingerprint_seed') IS DISTINCT FROM LOWER(BTRIM(extra->>'codex_fingerprint_seed'));

UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND type = 'oauth'
  AND LOWER(BTRIM(COALESCE(extra->>'codex_fingerprint_mode', ''))) IN ('device', 'session', 'full')
  AND (
    NULLIF(BTRIM(extra->>'codex_fingerprint_seed'), '') IS NULL
    OR LOWER(BTRIM(extra->>'codex_fingerprint_seed')) !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    OR LOWER(BTRIM(extra->>'codex_fingerprint_seed')) = '00000000-0000-0000-0000-000000000000'
  );
