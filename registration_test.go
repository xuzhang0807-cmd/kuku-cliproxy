package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// TestSingleResourceAndIcon pins the two panel-facing facts an operator sees:
// one sidebar entry (the login and quota pages are one panel now) and a logo the
// host can actually deliver.
func TestSingleResourceAndIcon(t *testing.T) {
	raw, err := handleManagementRegister([]byte(`{"base_path":"/v0/management"}`))
	if err != nil {
		t.Fatal(err)
	}
	var env struct {
		OK     bool                                     `json:"ok"`
		Result pluginapi.ManagementRegistrationResponse `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("registration refused: %s", raw)
	}
	if len(env.Result.Resources) != 1 {
		t.Fatalf("want exactly one sidebar entry, got %d: %+v", len(env.Result.Resources), env.Result.Resources)
	}
	if got := env.Result.Resources[0].Menu; got != "库库AI" {
		t.Errorf("sidebar label is %q", got)
	}
	if got := env.Result.Resources[0].Path; got != "/quota" {
		t.Errorf("resource path is %q, expected the /quota URL that was already in use", got)
	}
	// The key gate is what keeps account data off the no-auth resource route, so
	// the JSON routes behind it must stay registered.
	if len(env.Result.Routes) == 0 {
		t.Error("no management routes registered")
	}

	// The host runs the logo through html.EscapeString, so it has to be base64:
	// a raw inline SVG would arrive with its "<" mangled.
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(kukuLogoDataURL, prefix) {
		t.Fatalf("logo is not a base64 PNG data URL: %.40s", kukuLogoDataURL)
	}
	png, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(kukuLogoDataURL, prefix))
	if err != nil {
		t.Fatalf("logo is not valid base64: %v", err)
	}
	if !bytes.HasPrefix(png, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}) {
		t.Fatal("logo does not decode to a PNG")
	}
	if kukuRegistration().Metadata.Logo != kukuLogoDataURL {
		t.Error("registration metadata does not carry the logo")
	}
}
