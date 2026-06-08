package config

// ServerSettings configures the gRPC server endpoint for gojira.
// It is one of the standalone entity structs composed under [App];
// it can also be used directly by any component that only needs the
// server-address setting.
//
// The env key carries the full GOJIRA_SERVER_ADDRESS name so the
// parent App can use `env:",nested"` (empty prefix) without
// introducing nested-prefix stutter such as
// GOJIRA_SERVER_SERVER_ADDRESS.
type ServerSettings struct {
	// Address is the gRPC server bind address.
	// Sourced from GOJIRA_SERVER_ADDRESS or the "server.address"
	// YAML key.
	Address string `yaml:"address" json:"address" env:"GOJIRA_SERVER_ADDRESS"`
}

// DefaultServerSettings returns a [ServerSettings] seeded with the
// loopback bind address 127.0.0.1:50051. The loopback default
// reflects the Phase-1 trusted-network scope: the gRPC server is
// not exposed to external interfaces unless explicitly configured.
func DefaultServerSettings() ServerSettings {
	return ServerSettings{
		Address: "127.0.0.1:50051",
	}
}

// EffectiveAddress returns the gRPC bind address with precedence
// cli > configured > "127.0.0.1:50051". A nil receiver is tolerated
// and treated as an empty configured value, mirroring the
// [OutputSettings] pattern.
func (s *ServerSettings) EffectiveAddress(cli string) string {
	if cli != "" {
		return cli
	}
	if s != nil && s.Address != "" {
		return s.Address
	}
	return "127.0.0.1:50051"
}
