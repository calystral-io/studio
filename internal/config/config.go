// Package config loads BFF configuration from defaults, environment, and
// explicit CLI flag overrides. Precedence (lowest to highest): built-in
// default -> environment variable -> explicitly-set flag (12-factor: env wins
// over defaults, an operator-set flag wins over env).
package config

import (
	"fmt"
	"os"
	"strings"
)

// AuthMode enumerates the supported authentication backends.
type AuthMode string

const (
	// AuthModeMock is the PR1 default: a static token map.
	AuthModeMock AuthMode = "mock"
	// AuthModeNexus forwards a real upstream Nexus JWT (later PR).
	AuthModeNexus AuthMode = "nexus"
)

// CoreSource enumerates the anchor data sources.
type CoreSource string

const (
	// CoreSourceFixture serves a seeded in-memory anchor set (PR1 default).
	CoreSourceFixture CoreSource = "fixture"
	// CoreSourceGRPC dials Core's QueryService over gRPC.
	CoreSourceGRPC CoreSource = "grpc"
)

// Config is the fully-resolved BFF configuration.
type Config struct {
	HTTPAddr     string
	AuthMode     AuthMode
	CoreSource   CoreSource
	CoreGRPCAddr string
	// CoreGRPCAddrs is the cluster-mode replica set: the BFF fans out cluster
	// topology reads across all of these Core replicas and aggregates them. Empty
	// means single-node (dial CoreGRPCAddr only); use CoreReplicaAddrs to read the
	// effective list, which is always non-empty.
	CoreGRPCAddrs     []string
	CoreDevSigningKey string
	// CoreTLSCert / CoreTLSKey / CoreTLSCA point at the client certificate, its
	// key, and the CA bundle the BFF uses to dial Core over mTLS. When all three
	// are set (source=grpc), the BFF presents the client cert and verifies Core's
	// server cert against the CA; when all three are empty it dials plaintext
	// (fixture/local dev). Core's edge mandates mTLS, so a live cluster must set
	// them - see CoreTLSEnabled.
	CoreTLSCert string
	CoreTLSKey  string
	CoreTLSCA   string
	CORSOrigins []string
	LogLevel    string
}

// CoreTLSEnabled reports whether mTLS to Core is configured (all three cert
// files set). validate guarantees the all-or-nothing invariant, so testing one
// field is sufficient everywhere else.
func (c Config) CoreTLSEnabled() bool {
	return c.CoreTLSCert != "" && c.CoreTLSKey != "" && c.CoreTLSCA != ""
}

// CoreReplicaAddrs is the effective set of Core replica addresses to dial: the
// explicit cluster-mode list when set, else the single CoreGRPCAddr. Always
// returns at least one address.
func (c Config) CoreReplicaAddrs() []string {
	if len(c.CoreGRPCAddrs) > 0 {
		return c.CoreGRPCAddrs
	}
	return []string{c.CoreGRPCAddr}
}

// ClusterMode reports whether more than one Core replica is configured, i.e. the
// BFF should fan cluster reads out across replicas rather than dialing one node.
func (c Config) ClusterMode() bool {
	return len(c.CoreGRPCAddrs) > 1
}

// Defaults returns the contract section 8 defaults.
func Defaults() Config {
	return Config{
		HTTPAddr:          ":8080",
		AuthMode:          AuthModeMock,
		CoreSource:        CoreSourceFixture,
		CoreGRPCAddr:      "localhost:50051",
		CoreDevSigningKey: "",
		CORSOrigins:       []string{"http://localhost:5173"},
		LogLevel:          "info",
	}
}

// Env is the set of environment variable names the BFF honors.
const (
	EnvHTTPAddr          = "STUDIO_HTTP_ADDR"
	EnvAuthMode          = "STUDIO_AUTH_MODE"
	EnvCoreSource        = "STUDIO_CORE_SOURCE"
	EnvCoreGRPCAddr      = "STUDIO_CORE_GRPC_ADDR"
	EnvCoreGRPCAddrs     = "STUDIO_CORE_GRPC_ADDRS"
	EnvCoreDevSigningKey = "STUDIO_CORE_DEV_SIGNING_KEY"
	EnvCoreTLSCert       = "STUDIO_CORE_TLS_CERT"
	EnvCoreTLSKey        = "STUDIO_CORE_TLS_KEY"
	EnvCoreTLSCA         = "STUDIO_CORE_TLS_CA"
	EnvCORSOrigins       = "STUDIO_CORS_ORIGINS"
	EnvLogLevel          = "STUDIO_LOG_LEVEL"
)

// Lookup mirrors os.LookupEnv; injectable for tests.
type Lookup func(key string) (string, bool)

// Flags carries explicitly-set CLI flag overrides. A nil pointer means the flag
// was not set on the command line and so must not override env/default.
type Flags struct {
	HTTPAddr          *string
	AuthMode          *string
	CoreSource        *string
	CoreGRPCAddr      *string
	CoreGRPCAddrs     *string
	CoreDevSigningKey *string
	CoreTLSCert       *string
	CoreTLSKey        *string
	CoreTLSCA         *string
	CORSOrigins       *string
	LogLevel          *string
}

// Load resolves configuration applying defaults, then env, then explicit flags.
// It validates enum-valued fields and returns an error on an invalid value.
func Load(lookup Lookup, flags Flags) (Config, error) {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	c := Defaults()

	// Environment layer.
	if v, ok := lookup(EnvHTTPAddr); ok {
		c.HTTPAddr = v
	}
	if v, ok := lookup(EnvAuthMode); ok {
		c.AuthMode = AuthMode(v)
	}
	if v, ok := lookup(EnvCoreSource); ok {
		c.CoreSource = CoreSource(v)
	}
	if v, ok := lookup(EnvCoreGRPCAddr); ok {
		c.CoreGRPCAddr = v
	}
	if v, ok := lookup(EnvCoreGRPCAddrs); ok {
		c.CoreGRPCAddrs = splitList(v)
	}
	if v, ok := lookup(EnvCoreDevSigningKey); ok {
		c.CoreDevSigningKey = v
	}
	if v, ok := lookup(EnvCoreTLSCert); ok {
		c.CoreTLSCert = v
	}
	if v, ok := lookup(EnvCoreTLSKey); ok {
		c.CoreTLSKey = v
	}
	if v, ok := lookup(EnvCoreTLSCA); ok {
		c.CoreTLSCA = v
	}
	if v, ok := lookup(EnvCORSOrigins); ok {
		c.CORSOrigins = splitOrigins(v)
	}
	if v, ok := lookup(EnvLogLevel); ok {
		c.LogLevel = v
	}

	// Explicit-flag layer (highest precedence).
	if flags.HTTPAddr != nil {
		c.HTTPAddr = *flags.HTTPAddr
	}
	if flags.AuthMode != nil {
		c.AuthMode = AuthMode(*flags.AuthMode)
	}
	if flags.CoreSource != nil {
		c.CoreSource = CoreSource(*flags.CoreSource)
	}
	if flags.CoreGRPCAddr != nil {
		c.CoreGRPCAddr = *flags.CoreGRPCAddr
	}
	if flags.CoreGRPCAddrs != nil {
		c.CoreGRPCAddrs = splitList(*flags.CoreGRPCAddrs)
	}
	if flags.CoreDevSigningKey != nil {
		c.CoreDevSigningKey = *flags.CoreDevSigningKey
	}
	if flags.CoreTLSCert != nil {
		c.CoreTLSCert = *flags.CoreTLSCert
	}
	if flags.CoreTLSKey != nil {
		c.CoreTLSKey = *flags.CoreTLSKey
	}
	if flags.CoreTLSCA != nil {
		c.CoreTLSCA = *flags.CoreTLSCA
	}
	if flags.CORSOrigins != nil {
		c.CORSOrigins = splitOrigins(*flags.CORSOrigins)
	}
	if flags.LogLevel != nil {
		c.LogLevel = *flags.LogLevel
	}

	if err := c.validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) validate() error {
	switch c.AuthMode {
	case AuthModeMock, AuthModeNexus:
	default:
		return fmt.Errorf("invalid %s %q (want mock|nexus)", EnvAuthMode, c.AuthMode)
	}
	switch c.CoreSource {
	case CoreSourceFixture, CoreSourceGRPC:
	default:
		return fmt.Errorf("invalid %s %q (want fixture|grpc)", EnvCoreSource, c.CoreSource)
	}
	if c.CoreSource == CoreSourceGRPC {
		for _, addr := range c.CoreReplicaAddrs() {
			if strings.TrimSpace(addr) == "" {
				return fmt.Errorf("%s=grpc requires a non-empty Core address (%s / %s)",
					EnvCoreSource, EnvCoreGRPCAddr, EnvCoreGRPCAddrs)
			}
		}
		// The three Core TLS files are all-or-nothing: a partial set (e.g. a cert
		// without its CA) cannot form a working mTLS dialer, so reject it loudly
		// rather than silently falling back to plaintext against an mTLS edge. Only
		// enforced under grpc - the TLS files are irrelevant to the fixture source,
		// so a stray env var must not block a fixture/local startup.
		if n := boolCount(c.CoreTLSCert != "", c.CoreTLSKey != "", c.CoreTLSCA != ""); n != 0 && n != 3 {
			return fmt.Errorf("%s / %s / %s must be set together (got %d of 3)",
				EnvCoreTLSCert, EnvCoreTLSKey, EnvCoreTLSCA, n)
		}
	}
	return nil
}

// boolCount returns how many of the given flags are true.
func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

func splitOrigins(v string) []string {
	return splitList(v)
}

// splitList parses a comma-separated value into trimmed, non-empty entries.
func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
