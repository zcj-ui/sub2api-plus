package service

import "testing"

func TestAPIKeyService_RejectsV10AuthSnapshotWithoutModelsListConfig(t *testing.T) {
	groupID := int64(9)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-models-list", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  10,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:          2,
				Status:      StatusActive,
				Role:        RoleUser,
				Balance:     10,
				Concurrency: 3,
			},
			Group: &APIKeyAuthGroupSnapshot{
				ID:               groupID,
				Name:             "openai",
				Platform:         PlatformOpenAI,
				Status:           StatusActive,
				SubscriptionType: SubscriptionTypeStandard,
				RateMultiplier:   1,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatalf("expected v10 auth snapshot to be rejected after models_list_config was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV15AuthSnapshotWithoutReasoningEffortPolicy(t *testing.T) {
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-reasoning-mappings", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{Version: 15},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v15 auth snapshot to be rejected after reasoning effort policy was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_RejectsV20AuthSnapshotWithoutPublicGroupRestriction(t *testing.T) {
	groupID := int64(12)
	svc := &APIKeyService{}

	// Version 20 predates restrict_public_groups. Even if a stale payload is
	// hand-built with the field set, it must not be accepted as a complete
	// snapshot: a real serialized v20 payload would omit the field and decode
	// it as false, which could bypass the new public-group gate.
	apiKey, ok, err := svc.applyAuthCacheEntry("k-legacy-public-groups", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  20,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:                   2,
				Status:               StatusActive,
				Role:                 RoleUser,
				RestrictPublicGroups: true,
			},
		},
	})

	if err != nil {
		t.Fatalf("expected stale snapshot to be ignored without error, got %v", err)
	}
	if ok {
		t.Fatal("expected v20 auth snapshot to be rejected after restrict_public_groups was added")
	}
	if apiKey != nil {
		t.Fatalf("expected no API key from stale snapshot, got %#v", apiKey)
	}
}

func TestAPIKeyService_CurrentAuthSnapshotPreservesPublicGroupRestriction(t *testing.T) {
	groupID := int64(13)
	svc := &APIKeyService{}

	apiKey, ok, err := svc.applyAuthCacheEntry("k-current-public-groups", &APIKeyAuthCacheEntry{
		Snapshot: &APIKeyAuthSnapshot{
			Version:  apiKeyAuthSnapshotVersion,
			APIKeyID: 1,
			UserID:   2,
			GroupID:  &groupID,
			Status:   StatusActive,
			User: APIKeyAuthUserSnapshot{
				ID:                   2,
				Status:               StatusActive,
				Role:                 RoleUser,
				RestrictPublicGroups: true,
			},
		},
	})

	if err != nil {
		t.Fatalf("current snapshot should be accepted, got %v", err)
	}
	if !ok || apiKey == nil || apiKey.User == nil {
		t.Fatalf("expected current snapshot to produce an API key, got ok=%v key=%#v", ok, apiKey)
	}
	if !apiKey.User.RestrictPublicGroups {
		t.Fatal("current snapshot lost restrict_public_groups")
	}
}
