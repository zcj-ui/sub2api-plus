package service

// CodexFingerprintRecoveryRequiredExtraKey marks an OpenAI OAuth account whose
// historical fingerprint mode may have been collapsed to device by an older
// migration. The original mode is intentionally unknown; administrators must
// choose the desired mode explicitly before the marker is cleared.
const CodexFingerprintRecoveryRequiredExtraKey = "codex_fingerprint_recovery_required"

// NormalizeCodexFingerprintExtraForAccount prepares account extra before a
// create or repository-level full write. Fingerprint convergence remains an
// explicit OpenAI OAuth opt-in; an enabled mode receives a fresh, server-owned
// seed. External create and import payloads never choose an account identity.
func NormalizeCodexFingerprintExtraForAccount(platform, accountType string, extra map[string]any) map[string]any {
	prepared := stripCodexFingerprintSeed(RetireCodexFingerprintExtra(extra))
	if platform != PlatformOpenAI || accountType != AccountTypeOAuth {
		return stripCodexFingerprintAccountState(prepared)
	}
	return ensureCodexFingerprintSeed(platform, accountType, prepared)
}

// NormalizeCodexFingerprintExtraForExistingAccount keeps an account's stable
// seed when a full edit omits it. Existing accounts never rotate a valid seed;
// otherwise an enabled mode receives one exactly once.
func NormalizeCodexFingerprintExtraForExistingAccount(account *Account, extra map[string]any) map[string]any {
	if account == nil {
		return stripCodexFingerprintSeed(RetireCodexFingerprintExtra(extra))
	}

	// A full edit may include a stale or user-supplied seed. It is never an
	// instruction to rotate or initialize an existing account identity.
	prepared := stripCodexFingerprintSeed(RetireCodexFingerprintExtra(extra))
	if !account.IsOpenAIOAuth() || account.IsCredentialShadow() {
		return stripCodexFingerprintAccountState(prepared)
	}
	if IsCodexFingerprintRecoveryRequired(account.Extra) && !IsCodexFingerprintRecoveryRequired(prepared) {
		prepared = cloneCodexFingerprintExtra(prepared)
		prepared[CodexFingerprintRecoveryRequiredExtraKey] = true
	}
	if existingSeed := account.getCodexFingerprintSeed(); existingSeed != "" {
		prepared = cloneCodexFingerprintExtra(prepared)
		prepared[codexFingerprintSeedExtraKey] = existingSeed
	}
	return ensureCodexFingerprintSeed(account.Platform, account.Type, prepared)
}

func codexFingerprintExtraUpdateRequested(extra map[string]any) bool {
	if extra == nil {
		return false
	}
	for _, key := range []string{
		codexFingerprintModeExtraKey,
		CodexFingerprintRecoveryRequiredExtraKey,
		"openai_device_id",
		"openai_session_id",
	} {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

func codexFingerprintAccountStatePresent(extra map[string]any) bool {
	if extra == nil {
		return false
	}
	for _, key := range []string{
		codexFingerprintModeExtraKey,
		codexFingerprintSeedExtraKey,
		CodexFingerprintRecoveryRequiredExtraKey,
		"openai_device_id",
		"openai_session_id",
	} {
		if _, ok := extra[key]; ok {
			return true
		}
	}
	return false
}

// codexFingerprintModeUpdateRequested distinguishes an explicit mode
// selection (including an explicit null used to clear the account override)
// from a full account payload that merely carries the current stored extra.
// The distinction matters for recovery markers: saving a name, proxy, or
// quota field must not silently acknowledge an ambiguous historical mode.
func codexFingerprintModeUpdateRequested(extra map[string]any) bool {
	if extra == nil {
		return false
	}
	_, requested := extra[codexFingerprintModeExtraKey]
	return requested
}

func cloneCodexFingerprintExtra(extra map[string]any) map[string]any {
	if extra == nil {
		return make(map[string]any)
	}
	cloned := make(map[string]any, len(extra))
	for key, value := range extra {
		cloned[key] = value
	}
	return cloned
}

func stripCodexFingerprintAccountState(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	if !codexFingerprintAccountStatePresent(extra) {
		return extra
	}
	stripped := cloneCodexFingerprintExtra(extra)
	delete(stripped, codexFingerprintModeExtraKey)
	delete(stripped, codexFingerprintSeedExtraKey)
	delete(stripped, CodexFingerprintRecoveryRequiredExtraKey)
	delete(stripped, "openai_device_id")
	delete(stripped, "openai_session_id")
	return stripped
}

// AcknowledgeCodexFingerprintModeEdit clears the historical ambiguity marker
// only for an explicit administrator mode edit. Callers that pass a complete
// stored account snapshot must not use this helper: a routine name/proxy edit
// must leave the warning visible.
func AcknowledgeCodexFingerprintModeEdit(extra map[string]any) map[string]any {
	if extra == nil {
		return nil
	}
	if _, explicit := extra[codexFingerprintModeExtraKey]; !explicit {
		return extra
	}
	if _, marked := extra[CodexFingerprintRecoveryRequiredExtraKey]; !marked {
		return extra
	}
	cloned := cloneCodexFingerprintExtra(extra)
	delete(cloned, CodexFingerprintRecoveryRequiredExtraKey)
	if value, exists := cloned[codexFingerprintModeExtraKey]; exists && value == nil {
		delete(cloned, codexFingerprintModeExtraKey)
	}
	return cloned
}

// IsCodexFingerprintRecoveryRequired reports the durable migration marker in
// an account extra map without making the runtime fingerprint mode depend on
// it. It is used by administrative views and diagnostics only.
func IsCodexFingerprintRecoveryRequired(extra map[string]any) bool {
	if extra == nil {
		return false
	}
	switch value := extra[CodexFingerprintRecoveryRequiredExtraKey].(type) {
	case bool:
		return value
	case string:
		return value == "true"
	default:
		return false
	}
}
