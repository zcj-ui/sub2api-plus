-- Invalidate capability snapshots produced by the pre-v2 permissive probe.
-- Force modes are account configuration and are intentionally preserved.
UPDATE accounts
SET extra = COALESCE(extra, '{}'::jsonb)
    - 'openai_compact_supported'
    - 'openai_compact_probe_version'
    - 'openai_compact_checked_at'
    - 'openai_compact_last_status'
    - 'openai_compact_last_error'
    - 'openai_compact_probe_observed_at_unix_nano'
WHERE platform = 'openai'
  AND COALESCE(extra, '{}'::jsonb) ?| ARRAY[
      'openai_compact_supported',
      'openai_compact_probe_version',
      'openai_compact_checked_at',
      'openai_compact_last_status',
      'openai_compact_last_error',
      'openai_compact_probe_observed_at_unix_nano'
  ];
