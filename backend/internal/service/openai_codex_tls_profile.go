package service

import "github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"

// codexTLSProfile describes the TLS ClientHello emitted by the official
// Codex CLI's reqwest/rustls transport.  It is deliberately separate from
// the existing Node/Claude profile: the two clients advertise different
// extension sets and ALPN behavior.  Keep this profile immutable after init;
// the dialer copies and randomizes the extension slice per connection.
var codexTLSProfile = &tlsfingerprint.Profile{
	Name: "Codex CLI (reqwest+rustls)",
	CipherSuites: []uint16{
		0x1302, // TLS_AES_256_GCM_SHA384
		0x1301, // TLS_AES_128_GCM_SHA256
		0x1303, // TLS_CHACHA20_POLY1305_SHA256
		0xc02c, // TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384
		0xc02b, // TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256
		0xcca9, // TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256
		0xc030, // TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384
		0xc02f, // TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256
		0xcca8, // TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256
		0x00ff, // TLS_EMPTY_RENEGOTIATION_INFO_SCSV
	},
	// X25519MLKEM768 is the hybrid group used by current rustls builds.
	// Keep supported_groups and key_share in lockstep so the advertised
	// handshake is internally consistent.
	Curves:         []uint16{0x11ec, 0x001d, 0x0017, 0x0018},
	KeyShareGroups: []uint16{0x11ec, 0x001d, 0x0017, 0x0018},
	PointFormats:   []uint16{0},
	// The official Codex reqwest client is built without HTTP/2 support and
	// therefore does not send ALPN.  Leaving both the slice and extension out
	// is intentional.
	ALPNProtocols: nil,
	Extensions: []uint16{
		0,  // server_name
		5,  // status_request
		10, // supported_groups
		11, // ec_point_formats
		13, // signature_algorithms
		23, // extended_master_secret
		35, // session_ticket
		43, // supported_versions
		45, // psk_key_exchange_modes
		51, // key_share
	},
	EnableGREASE:            false,
	RandomizeExtensionOrder: true,
}

// resolveOpenAICodexTLSProfile gives explicitly configured profiles priority,
// then enables the built-in Codex profile for OAuth accounts.  API-key and
// other account types retain the ordinary transport path.  A nil service is
// valid in lightweight fixtures and simply means there is no explicit profile.
func resolveOpenAICodexTLSProfile(explicit *tlsfingerprint.Profile, account *Account) *tlsfingerprint.Profile {
	if explicit != nil {
		return explicit
	}
	if account != nil && account.IsOpenAIOAuth() {
		return codexTLSProfile
	}
	return nil
}
