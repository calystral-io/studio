package config

import (
	"reflect"
	"testing"
)

func mapLookup(m map[string]string) Lookup {
	return func(key string) (string, bool) {
		v, ok := m[key]
		return v, ok
	}
}

func strptr(s string) *string { return &s }

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(mapLookup(nil), Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := Defaults()
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("cfg = %#v, want %#v", cfg, want)
	}
}

func TestLoadEnvOverridesDefault(t *testing.T) {
	env := map[string]string{
		EnvHTTPAddr:    ":9090",
		EnvAuthMode:    "mock",
		EnvCoreSource:  "grpc",
		EnvCORSOrigins: "http://a.test, http://b.test ,",
		EnvLogLevel:    "debug",
	}
	cfg, err := Load(mapLookup(env), Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("http addr = %q", cfg.HTTPAddr)
	}
	if cfg.CoreSource != CoreSourceGRPC {
		t.Errorf("core source = %q", cfg.CoreSource)
	}
	if !reflect.DeepEqual(cfg.CORSOrigins, []string{"http://a.test", "http://b.test"}) {
		t.Errorf("cors = %#v", cfg.CORSOrigins)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("log level = %q", cfg.LogLevel)
	}
}

func TestLoadFlagBeatsEnv(t *testing.T) {
	env := map[string]string{
		EnvHTTPAddr:   ":9090",
		EnvCoreSource: "grpc",
	}
	flags := Flags{
		HTTPAddr:   strptr(":7070"),
		CoreSource: strptr("fixture"),
	}
	cfg, err := Load(mapLookup(env), flags)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTPAddr != ":7070" {
		t.Errorf("flag should win: http addr = %q", cfg.HTTPAddr)
	}
	if cfg.CoreSource != CoreSourceFixture {
		t.Errorf("flag should win: core source = %q", cfg.CoreSource)
	}
}

func TestLoadUnsetFlagDoesNotOverride(t *testing.T) {
	env := map[string]string{EnvHTTPAddr: ":9090"}
	// nil flag pointer means "not set on the command line".
	cfg, err := Load(mapLookup(env), Flags{HTTPAddr: nil})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.HTTPAddr != ":9090" {
		t.Errorf("env should stand: http addr = %q", cfg.HTTPAddr)
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{name: "bad auth mode", env: map[string]string{EnvAuthMode: "bogus"}, wantErr: true},
		{name: "bad core source", env: map[string]string{EnvCoreSource: "bogus"}, wantErr: true},
		{name: "nexus mode allowed", env: map[string]string{EnvAuthMode: "nexus"}, wantErr: false},
		{name: "grpc source allowed", env: map[string]string{EnvCoreSource: "grpc"}, wantErr: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(mapLookup(tc.env), Flags{})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestCoreReplicaAddrsFallback(t *testing.T) {
	// With no replica list, the effective set is the single CoreGRPCAddr and the
	// BFF is not in cluster mode.
	cfg, err := Load(mapLookup(map[string]string{EnvCoreGRPCAddr: "core-a:50051"}), Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.CoreReplicaAddrs(); !reflect.DeepEqual(got, []string{"core-a:50051"}) {
		t.Errorf("replica addrs = %#v, want single", got)
	}
	if cfg.ClusterMode() {
		t.Error("ClusterMode should be false with one replica")
	}
}

func TestCoreGRPCAddrsEnablesClusterMode(t *testing.T) {
	env := map[string]string{
		EnvCoreSource:    "grpc",
		EnvCoreGRPCAddrs: "core-a:50051, core-b:50051 ,core-c:50051,",
	}
	cfg, err := Load(mapLookup(env), Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"core-a:50051", "core-b:50051", "core-c:50051"}
	if !reflect.DeepEqual(cfg.CoreGRPCAddrs, want) {
		t.Errorf("addrs = %#v, want %#v (trimmed, empties dropped)", cfg.CoreGRPCAddrs, want)
	}
	if !reflect.DeepEqual(cfg.CoreReplicaAddrs(), want) {
		t.Errorf("replica addrs = %#v", cfg.CoreReplicaAddrs())
	}
	if !cfg.ClusterMode() {
		t.Error("ClusterMode should be true with three replicas")
	}
}

func TestCoreGRPCAddrsFlagBeatsEnv(t *testing.T) {
	env := map[string]string{EnvCoreGRPCAddrs: "env-a:1,env-b:2"}
	cfg, err := Load(mapLookup(env), Flags{CoreGRPCAddrs: strptr("flag-a:1,flag-b:2,flag-c:3")})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := []string{"flag-a:1", "flag-b:2", "flag-c:3"}
	if !reflect.DeepEqual(cfg.CoreGRPCAddrs, want) {
		t.Errorf("addrs = %#v, want %#v", cfg.CoreGRPCAddrs, want)
	}
}

func TestSingleReplicaListIsNotClusterMode(t *testing.T) {
	// A one-entry replica list is single-node, not a fan-out cluster.
	cfg, err := Load(mapLookup(map[string]string{EnvCoreGRPCAddrs: "only:50051"}), Flags{})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.ClusterMode() {
		t.Error("one replica must not be cluster mode")
	}
	if got := cfg.CoreReplicaAddrs(); !reflect.DeepEqual(got, []string{"only:50051"}) {
		t.Errorf("replica addrs = %#v", got)
	}
}
