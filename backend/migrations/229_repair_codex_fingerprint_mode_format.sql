-- Canonicalize legacy Codex fingerprint modes that were persisted with
-- different casing or surrounding whitespace. Older API clients accepted
-- those values, while the seed backfill migrations used exact comparisons.
-- Preserve session and full: they are distinct opt-in lifecycle modes.
-- Keep this forward-only: migration 225 may already be applied elsewhere.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_mode}',
    to_jsonb(
        LOWER(BTRIM(extra->>'codex_fingerprint_mode'))
    ),
    true
)
WHERE deleted_at IS NULL
  AND platform = 'openai'
  AND type = 'oauth'
  AND LOWER(BTRIM(extra->>'codex_fingerprint_mode')) IN ('off', 'device', 'session', 'full')
  AND extra->>'codex_fingerprint_mode' IS DISTINCT FROM LOWER(BTRIM(extra->>'codex_fingerprint_mode'));

-- Canonicalize valid imported seeds before repairing malformed ones. UUID
-- version is intentionally not restricted: the seed is opaque account state,
-- and changing a valid v1/v5 value would rotate the converged identity.
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
  AND extra->>'codex_fingerprint_mode' IN ('device', 'session', 'full')
  AND NULLIF(BTRIM(extra->>'codex_fingerprint_seed'), '') IS NOT NULL
  AND LOWER(BTRIM(extra->>'codex_fingerprint_seed')) ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
  AND LOWER(BTRIM(extra->>'codex_fingerprint_seed')) <> '00000000-0000-0000-0000-000000000000'
  AND BTRIM(extra->>'codex_fingerprint_seed') IS DISTINCT FROM LOWER(BTRIM(extra->>'codex_fingerprint_seed'));

-- Repair missing, null, or malformed seeds after canonicalization. A volatile
-- UUID function is evaluated per account row, so bulk repair keeps identities
-- distinct.
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
  AND extra->>'codex_fingerprint_mode' IN ('device', 'session', 'full')
  AND (
    NULLIF(BTRIM(extra->>'codex_fingerprint_seed'), '') IS NULL
    OR LOWER(BTRIM(extra->>'codex_fingerprint_seed')) !~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    OR LOWER(BTRIM(extra->>'codex_fingerprint_seed')) = '00000000-0000-0000-0000-000000000000'
  );
