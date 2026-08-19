-- Compatibility no-op.
--
-- This migration was originally published with a destructive UPDATE that
-- collapsed the explicit `session` and `full` Codex fingerprint modes into
-- `device`.  The gateway now supports the complete opt-in lifecycle, so a
-- fresh install must preserve all four modes.  Keep the historical filename
-- for migration ordering; the checksum compatibility rule in the runner
-- allows databases that already recorded the original file to upgrade.
SELECT 1;
