package repository

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexFingerprintMigrationChecksumCompatibility(t *testing.T) {
	tests := []struct {
		name      string
		migration string
		file      string
		db        string
	}{
		{
			name:      "227 historical destructive migration",
			migration: "227_codex_fingerprint_device_only.sql",
			file:      "e15ec8765b3d16b488695b40c6303f8da31e3db89d7a0b094b99e7dd35ae545e",
			db:        "7d49ebfa33c409b82ef2f3e3dee654c447c68795f472314af461731c83e4b46a",
		},
		{
			name:      "227 v0.2.4 production checksum",
			migration: "227_codex_fingerprint_device_only.sql",
			file:      "e15ec8765b3d16b488695b40c6303f8da31e3db89d7a0b094b99e7dd35ae545e",
			db:        "0ad6ed038ae2f336dacd37df1dcceaaae919b008bfedca9bfcb6e8f9f0102246",
		},
		{
			name:      "227 current compatibility no-op checksum",
			migration: "227_codex_fingerprint_device_only.sql",
			file:      "12ee67e1598007b45f76b3354a15f682fb0a16d713bfb7a325e360e9ed97b066",
			db:        "12ee67e1598007b45f76b3354a15f682fb0a16d713bfb7a325e360e9ed97b066",
		},
		{
			name:      "228 historical v4-only seed repair",
			migration: "228_repair_codex_fingerprint_seed.sql",
			file:      "14e050c1161ca3d6ea3d140215ec5cdca49a3867ad76f7f7718122109a65b43c",
			db:        "8e8853589f93a7be6f13d69d2cf65e323bcc672043e9e983b26dee276fe26382",
		},
		{
			name:      "228 current trimmed checksum",
			migration: "228_repair_codex_fingerprint_seed.sql",
			file:      "161fc374b23255cdeca5c47a80ce3910440cf07e326549cc4d117dfd00336b5c",
			db:        "8e8853589f93a7be6f13d69d2cf65e323bcc672043e9e983b26dee276fe26382",
		},
		{
			name:      "228 historical trimmed checksum",
			migration: "228_repair_codex_fingerprint_seed.sql",
			file:      "161fc374b23255cdeca5c47a80ce3910440cf07e326549cc4d117dfd00336b5c",
			db:        "5bba233ee8aecb0227335e4b1e0483261ea488af439132fdd9080657634e1198",
		},
		{
			name:      "229 historical mode collapse migration",
			migration: "229_repair_codex_fingerprint_mode_format.sql",
			file:      "ce149f7d9cc46c3266f4c0235688b418563a2076be40cb346a49cf8d8c308c38",
			db:        "b42b4f511c70854ce29e3c7b65b2dbd9a3d964bbd3b9f4729a686543e923f938",
		},
		{
			name:      "229 v0.2.4 production checksum",
			migration: "229_repair_codex_fingerprint_mode_format.sql",
			file:      "ce149f7d9cc46c3266f4c0235688b418563a2076be40cb346a49cf8d8c308c38",
			db:        "b90e9b19933d6fddaf4868101b33303f96ac45a03e5aa75f7ce77db8663b0da3",
		},
		{
			name:      "229 canonical UUID repair checksum",
			migration: "229_repair_codex_fingerprint_mode_format.sql",
			file:      "dfc3f0c38c8dc2149fb8f95e596f9c62fc2aff682d604da9e95a22e40978bb4c",
			db:        "f91fa9ddfe0ef513730c9ae8252847375d3c550abb4a08ec5c35710ab51cda87",
		},
		{
			name:      "229 current trimmed checksum",
			migration: "229_repair_codex_fingerprint_mode_format.sql",
			file:      "537cc3f05e834739b49f02ff3fe94eabaedac8f5968b92da685b15fca14d42cf",
			db:        "f91fa9ddfe0ef513730c9ae8252847375d3c550abb4a08ec5c35710ab51cda87",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, isMigrationChecksumCompatible(tt.migration, tt.db, tt.file))
			require.False(t, isMigrationChecksumCompatible(tt.migration, "unexpected", tt.file))
		})
	}
}
