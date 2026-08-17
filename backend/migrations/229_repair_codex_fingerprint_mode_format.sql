-- Canonicalize legacy Codex fingerprint modes that were persisted with
-- different casing or surrounding whitespace. Older API clients accepted
-- those values, while the seed backfill migrations used exact comparisons.
-- Keep this forward-only: migration 225 may already be applied elsewhere.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_mode}',
    to_jsonb(
        CASE
            WHEN LOWER(BTRIM(extra->>'codex_fingerprint_mode')) IN ('session', 'full') THEN 'device'
            ELSE LOWER(BTRIM(extra->>'codex_fingerprint_mode'))
        END
    ),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND LOWER(BTRIM(extra->>'codex_fingerprint_mode')) IN ('off', 'device', 'session', 'full')
  AND extra->>'codex_fingerprint_mode' IS DISTINCT FROM CASE
      WHEN LOWER(BTRIM(extra->>'codex_fingerprint_mode')) IN ('session', 'full') THEN 'device'
      ELSE LOWER(BTRIM(extra->>'codex_fingerprint_mode'))
  END;

-- Repair missing, null, or malformed seeds after mode canonicalization. A
-- volatile UUID function is evaluated per account row, so bulk repair keeps
-- identities distinct.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND extra->>'codex_fingerprint_mode' = 'device'
  AND (
    NULLIF(BTRIM(extra->>'codex_fingerprint_seed'), '') IS NULL
    OR BTRIM(extra->>'codex_fingerprint_seed') !~* '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
  );
