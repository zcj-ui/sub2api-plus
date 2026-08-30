package tlsfingerprint

import "testing"

func sameUint16Multiset(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[uint16]int, len(a))
	for _, value := range a {
		counts[value]++
	}
	for _, value := range b {
		counts[value]--
	}
	for _, count := range counts {
		if count != 0 {
			return false
		}
	}
	return true
}

func TestShuffleExtensionOrderPreservesSetAndInput(t *testing.T) {
	original := []uint16{0, 5, 10, 11, 13, 23, 35, 43, 45, 51}
	input := append([]uint16(nil), original...)
	seen := make(map[string]struct{})
	for i := 0; i < 20; i++ {
		got := shuffleExtensionOrder(input)
		if !sameUint16Multiset(original, got) {
			t.Fatalf("iteration %d changed extension set: want %v got %v", i, original, got)
		}
		key := make([]byte, len(got)*2)
		for j, value := range got {
			key[j*2] = byte(value >> 8)
			key[j*2+1] = byte(value)
		}
		seen[string(key)] = struct{}{}
	}
	if len(seen) < 2 {
		t.Fatalf("expected at least two extension orders, got %d", len(seen))
	}
	for i := range input {
		if input[i] != original[i] {
			t.Fatalf("input slice was mutated: want %v got %v", original, input)
		}
	}
}

func TestBuildClientHelloSpecRandomizeOptIn(t *testing.T) {
	original := []uint16{0, 5, 10, 11, 13, 23, 35, 43, 45, 51}
	profile := &Profile{Extensions: append([]uint16(nil), original...), RandomizeExtensionOrder: true}
	for i := 0; i < 10; i++ {
		spec := buildClientHelloSpecFromProfile(profile)
		if len(spec.Extensions) != len(original) {
			t.Fatalf("got %d extensions, want %d", len(spec.Extensions), len(original))
		}
	}
	for i := range profile.Extensions {
		if profile.Extensions[i] != original[i] {
			t.Fatalf("profile Extensions was mutated: want %v got %v", original, profile.Extensions)
		}
	}
}

func TestBuildClientHelloSpecRandomizeDisabledKeepsOrder(t *testing.T) {
	original := []uint16{0, 5, 10, 11, 13, 23, 35, 43, 45, 51}
	profile := &Profile{Extensions: append([]uint16(nil), original...)}
	spec := buildClientHelloSpecFromProfile(profile)
	if len(spec.Extensions) != len(original) {
		t.Fatalf("got %d extensions, want %d", len(spec.Extensions), len(original))
	}
}
