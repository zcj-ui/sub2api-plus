-- Repair non-empty but malformed seeds left by early opt-in implementations.
-- The seed is opaque account state, so every canonical non-nil UUID version is
-- valid.  Restricting this to UUIDv4 would silently rotate valid imported v1/v5
-- identities before migration 229 can canonicalize their casing.
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
    OR BTRIM(extra->>'codex_fingerprint_seed') !~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    OR LOWER(BTRIM(extra->>'codex_fingerprint_seed')) = '00000000-0000-0000-0000-000000000000'
  );
