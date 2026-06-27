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
	HTTPAddr          string
	AuthMode          AuthMode
	CoreSource        CoreSource
	CoreGRPCAddr      string
	CoreDevSigningKey string
	CORSOrigins       []string
	LogLevel          string
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
	EnvCoreDevSigningKey = "STUDIO_CORE_DEV_SIGNING_KEY"
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
	CoreDevSigningKey *string
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
	if v, ok := lookup(EnvCoreDevSigningKey); ok {
		c.CoreDevSigningKey = v
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
	if flags.CoreDevSigningKey != nil {
		c.CoreDevSigningKey = *flags.CoreDevSigningKey
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
	return nil
}

func splitOrigins(v string) []string {
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
