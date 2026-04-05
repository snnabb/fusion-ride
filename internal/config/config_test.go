package config

import (
	"path/filepath"
	"testing"
)

func TestDefaultProvidesSensibleDefaults(t *testing.T) {
	cfg := Default()

	if cfg.Server.Port != 8096 {
		t.Fatalf("expected default port 8096, got %d", cfg.Server.Port)
	}
	if cfg.Server.Name != "FusionRide" {
		t.Fatalf("expected default server name FusionRide, got %q", cfg.Server.Name)
	}
	if cfg.Playback.Mode != "proxy" {
		t.Fatalf("expected default playback mode proxy, got %q", cfg.Playback.Mode)
	}
	if len(cfg.Bitrate.CodecPriority) == 0 {
		t.Fatal("expected default codec priority to be populated")
	}
}

func TestValidateNormalizesInvalidValues(t *testing.T) {
	cfg := &Config{
		Server: ServerConfig{
			Port:  -1,
			Name:  "",
			ID:    "",
		},
		Playback: PlaybackConfig{Mode: ""},
		Timeouts: TimeoutConfig{
			API:            0,
			Aggregate:      -1,
			Login:          -1,
			HealthCheck:    -1,
			HealthInterval: 0,
		},
		Bitrate: BitrateConfig{},
	}

	cfg.Validate()

	if cfg.Server.Port != 8096 {
		t.Fatalf("expected invalid port to be normalized to 8096, got %d", cfg.Server.Port)
	}
	if cfg.Server.Name != "FusionRide" {
		t.Fatalf("expected empty server name to be normalized, got %q", cfg.Server.Name)
	}
	if cfg.Playback.Mode != "proxy" {
		t.Fatalf("expected empty playback mode to be normalized, got %q", cfg.Playback.Mode)
	}
	if cfg.Timeouts.API != 30000 {
		t.Fatalf("expected API timeout default 30000, got %d", cfg.Timeouts.API)
	}
	if cfg.Timeouts.Aggregate != 15000 {
		t.Fatalf("expected aggregate timeout default 15000, got %d", cfg.Timeouts.Aggregate)
	}
	if cfg.Timeouts.Login != 10000 {
		t.Fatalf("expected login timeout default 10000, got %d", cfg.Timeouts.Login)
	}
	if cfg.Timeouts.HealthCheck != 10000 {
		t.Fatalf("expected health check timeout default 10000, got %d", cfg.Timeouts.HealthCheck)
	}
	if cfg.Timeouts.HealthInterval != 60000 {
		t.Fatalf("expected health interval default 60000, got %d", cfg.Timeouts.HealthInterval)
	}
	if len(cfg.Bitrate.CodecPriority) == 0 {
		t.Fatal("expected codec priority defaults to be populated")
	}
}

func TestSaveAndLoadRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Server.Name = "Test Server"
	cfg.Server.Port = 18096
	cfg.Admin.Username = "tester"
	cfg.Playback.Mode = "redirect"
	cfg.Upstream = []UpstreamDef{{
		Name:         "emby-a",
		URL:          "http://127.0.0.1:8096",
		PlaybackMode: "proxy",
	}}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := cfg.Save(path); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if loaded.Server.Name != cfg.Server.Name {
		t.Fatalf("expected server name %q, got %q", cfg.Server.Name, loaded.Server.Name)
	}
	if loaded.Server.Port != cfg.Server.Port {
		t.Fatalf("expected port %d, got %d", cfg.Server.Port, loaded.Server.Port)
	}
	if len(loaded.Upstream) != 1 || loaded.Upstream[0].Name != "emby-a" {
		t.Fatalf("expected upstream round-trip to preserve entries, got %+v", loaded.Upstream)
	}
}

func TestSnapshotReturnsIndependentCopy(t *testing.T) {
	cfg := Default()
	cfg.Upstream = []UpstreamDef{{Name: "emby-a", URL: "http://127.0.0.1:8096"}}

	snap := cfg.Snapshot()
	snap.Server.Name = "Changed"
	snap.Upstream[0].Name = "changed-upstream"

	if cfg.Server.Name == "Changed" {
		t.Fatal("expected snapshot mutation not to affect original server config")
	}
	if cfg.Upstream[0].Name == "changed-upstream" {
		t.Fatal("expected snapshot mutation not to affect original upstream slice")
	}
}
