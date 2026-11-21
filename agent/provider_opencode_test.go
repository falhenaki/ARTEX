package agent

import "testing"

func TestBridgeAdjustedRunSeconds(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 0},     // unlimited stays unlimited
		{-1, -1},   // negative passthrough (defensive)
		{1200, 4800},
		{1, 4},
	}
	for _, c := range cases {
		if got := BridgeAdjustedRunSeconds(c.in); got != c.want {
			t.Fatalf("BridgeAdjustedRunSeconds(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// The opencode profile must resolve keyless through ConfigFrom and build the
// bridge provider (not the HTTP provider factory).
func TestOpencodeConfigAndProvider(t *testing.T) {
	c := ConfigFrom("opencode", "", "https://ignored.example", "sk-ignored", "socks5://ignored")
	if c.Format != FormatOpencode {
		t.Fatalf("format = %q", c.Format)
	}
	if c.BaseURL != "" || c.APIKey != "" || c.Proxy != "" {
		t.Fatalf("opencode config must clear endpoint fields, got %+v", c)
	}
	if c.Model != DefaultOpencodeModel {
		t.Fatalf("model = %q", c.Model)
	}
	if c.NeedsAPIKey() {
		t.Fatal("opencode must be keyless")
	}
	if c.Provider() != "opencode" {
		t.Fatalf("Provider() = %q", c.Provider())
	}
	prov, err := c.NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if !prov.(interface{ BridgeBacked() bool }).BridgeBacked() {
		t.Fatal("opencode provider must report BridgeBacked")
	}

	// A set model name is preserved (display only).
	if m := ConfigFrom("opencode", "glm-5.3", "", "", "").Model; m != "glm-5.3" {
		t.Fatalf("custom model = %q", m)
	}
}
