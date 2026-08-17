-- The gateway cannot faithfully synthesize Codex's stateful session/thread/
-- compact-window lineage. Keep the persisted opt-in, but migrate the legacy
-- stateless modes to the only projection that is protocol-consistent: device.
UPDATE accounts
SET extra = jsonb_set(
    COALESCE(extra, '{}'::jsonb),
    '{codex_fingerprint_mode}',
    to_jsonb('device'::text),
    true
)
WHERE platform = 'openai'
  AND type = 'oauth'
  AND extra->>'codex_fingerprint_mode' IN ('session', 'full');
