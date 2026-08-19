-- Historical Codex migrations collapsed explicit session/full convergence to
-- device.  The original mode is not recoverable from the resulting row, so
-- mark only accounts that already existed when a destructive migration ran.
-- The application/UI can then require an explicit administrator choice.
WITH destructive_migration AS (
    -- Use the latest destructive run: an account created between two such
    -- migrations could have been collapsed by the later one.
    SELECT MAX(applied_at) AS applied_at
    FROM schema_migrations
    WHERE (filename = '226_converge_codex_fingerprint_modes_to_device.sql'
             AND checksum IN (
                 'f867dc727096af3586c908ac3323c76525be89d9ac2916691cd1c0191c1e51a'
             ))
       OR (filename = '227_codex_fingerprint_device_only.sql'
             AND checksum IN (
                 'e15ec8765b3d16b488695b40c6303f8da31e3db89d7a0b094b99e7dd35ae545e',
                 '7d49ebfa33c409b82ef2f3e3dee654c447c68795f472314af461731c83e4b46a',
                 '0ad6ed038ae2f336dacd37df1dcceaaae919b008bfedca9bfcb6e8f9f0102246'
             ))
       OR (filename = '229_repair_codex_fingerprint_mode_format.sql'
             AND checksum IN (
                 'ce149f7d9cc46c3266f4c0235688b418563a2076be40cb346a49cf8d8c308c38',
                 'b42b4f511c70854ce29e3c7b65b2dbd9a3d964bbd3b9f4729a686543e923f938',
                 'b90e9b19933d6fddaf4868101b33303f96ac45a03e5aa75f7ce77db8663b0da3'
             ))
)
UPDATE accounts AS a
SET extra = jsonb_set(
    COALESCE(a.extra, '{}'::jsonb),
    '{codex_fingerprint_recovery_required}',
    'true'::jsonb,
    true
)
FROM destructive_migration AS d
WHERE d.applied_at IS NOT NULL
  AND a.deleted_at IS NULL
  AND a.platform = 'openai'
  AND a.type = 'oauth'
  AND a.created_at <= d.applied_at
  AND LOWER(BTRIM(a.extra->>'codex_fingerprint_mode')) = 'device'
  AND COALESCE(a.extra->>'codex_fingerprint_recovery_required', '') <> 'true';
