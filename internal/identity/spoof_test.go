package identity

import (
	"net/http"
	"strings"
	"testing"
)

func TestSpooferInfuseModeHeaders(t *testing.T) {
	spoofer := NewSpoofer("infuse", "", "", "", "", "")
	headers := spoofer.Headers()

	if spoofer.Mode() != "infuse" {
		t.Fatalf("expected infuse mode, got %q", spoofer.Mode())
	}
	if headers["User-Agent"] != "Infuse/7.8.1" {
		t.Fatalf("expected Meridian infuse UA, got %q", headers["User-Agent"])
	}
	if headers["X-Emby-Client"] != "Infuse" {
		t.Fatalf("expected Infuse client, got %q", headers["X-Emby-Client"])
	}
	if !strings.Contains(headers["X-Emby-Authorization"], `Client="Infuse"`) {
		t.Fatalf("expected authorization header to contain Infuse client, got %q", headers["X-Emby-Authorization"])
	}
}

func TestSpooferWebModeHeaders(t *testing.T) {
	spoofer := NewSpoofer("web", "", "", "", "", "")
	headers := spoofer.Headers()

	if spoofer.Mode() != "web" {
		t.Fatalf("expected web mode, got %q", spoofer.Mode())
	}
	if headers["X-Emby-Client"] != "Emby Web" {
		t.Fatalf("expected Emby Web client, got %q", headers["X-Emby-Client"])
	}
	if headers["X-Emby-Client-Version"] != "4.9.0.42" {
		t.Fatalf("expected Meridian web version, got %q", headers["X-Emby-Client-Version"])
	}
	if !strings.Contains(headers["User-Agent"], "Emby Theater") {
		t.Fatalf("expected web UA to contain Emby Theater, got %q", headers["User-Agent"])
	}
}

func TestSpooferClientModeHeaders(t *testing.T) {
	spoofer := NewSpoofer("client", "", "", "", "", "")
	headers := spoofer.Headers()

	if spoofer.Mode() != "client" {
		t.Fatalf("expected client mode, got %q", spoofer.Mode())
	}
	if headers["User-Agent"] != "Emby-Theater/4.7.0" {
		t.Fatalf("expected Meridian client UA, got %q", headers["User-Agent"])
	}
	if headers["X-Emby-Client"] != "Emby Theater" {
		t.Fatalf("expected Emby Theater client, got %q", headers["X-Emby-Client"])
	}
}

func TestSpooferLegacyModesAreNormalized(t *testing.T) {
	tests := map[string]string{
		"passthrough": "client",
		"custom":      "infuse",
		"none":        "infuse",
		"":            "infuse",
	}

	for input, want := range tests {
		if got := NewSpoofer(input, "", "", "", "", "").Mode(); got != want {
			t.Fatalf("expected mode %q to normalize to %q, got %q", input, want, got)
		}
	}
}

func TestApplyToHeaderRewritesAuthorizationIdentity(t *testing.T) {
	spoofer := NewSpoofer("web", "", "", "", "", "")
	header := http.Header{}
	header.Set("Authorization", `MediaBrowser Client="Old Client", Device="TV", DeviceId="dev-1", Version="1.0.0"`)
	header.Set("X-Emby-Authorization", `MediaBrowser Client="Old Client", Device="TV", DeviceId="dev-1", Version="1.0.0"`)

	spoofer.ApplyToHeader(header)

	for _, key := range []string{"Authorization", "X-Emby-Authorization"} {
		value := header.Get(key)
		if !strings.Contains(value, `Client="Emby Web"`) {
			t.Fatalf("expected %s to contain web client, got %q", key, value)
		}
		if !strings.Contains(value, `Version="4.9.0.42"`) {
			t.Fatalf("expected %s to contain web version, got %q", key, value)
		}
	}
}

func TestBuildAuthorizationHeaderIncludesIdentity(t *testing.T) {
	spoofer := NewSpoofer("client", "", "", "", "", "")
	header := spoofer.BuildAuthorizationHeader()

	for _, want := range []string{
		`Client="Emby Theater"`,
		`Device="Windows"`,
		`DeviceId="fusionride-client"`,
		`Version="4.7.0"`,
	} {
		if !strings.Contains(header, want) {
			t.Fatalf("expected authorization header to contain %s, got %q", want, header)
		}
	}
}
