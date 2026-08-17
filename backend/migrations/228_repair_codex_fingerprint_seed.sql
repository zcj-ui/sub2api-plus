-- Repair non-empty but malformed seeds left by early opt-in implementations.
-- Keep this as a new forward migration so deployments that already recorded
-- migration 225 do not encounter a checksum mismatch.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_seed}',
    to_jsonb(gen_random_uuid()::text),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND extra->>'codex_fingerprint_mode' IN ('device', 'session', 'full')
  AND (
    NULLIF(BTRIM(extra->>'codex_fingerprint_seed'), '') IS NULL
    OR BTRIM(extra->>'codex_fingerprint_seed') !~* '^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$'
  );
