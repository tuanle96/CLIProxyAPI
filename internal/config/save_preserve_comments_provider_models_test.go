package config

import (
	"os"
	"path/filepath"
	"testing"

)

// TestSaveConfigPreserveComments_ProviderModelsPrefixUpdate verifies that updating
// the prefix for an existing provider-models entry is persisted correctly.
func TestSaveConfigPreserveComments_ProviderModelsPrefixUpdate(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Step 1: Write initial config with provider-models.copilot prefix=ghc
	initial := `
port: 8080
provider-models:
  copilot:
    prefix: ghc
    use_all: true
    models:
      - id: gpt-5.3-codex
        display_name: GPT 5.3 Codex
        type: copilot
        owned_by: github
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 2: Load config, change prefix to "abc", save
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProviderModels["copilot"].Prefix != "ghc" {
		t.Fatalf("expected prefix=ghc, got %q", cfg.ProviderModels["copilot"].Prefix)
	}

	cfg.ProviderModels["copilot"] = ProviderModelsConfig{
		Prefix: "abc",
		UseAll: true,
		Models: cfg.ProviderModels["copilot"].Models,
	}

	if err := SaveConfigPreserveComments(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	// Step 3: Reload and verify prefix changed
	cfg2, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.ProviderModels["copilot"].Prefix != "abc" {
		t.Errorf("expected prefix=abc after save, got %q", cfg2.ProviderModels["copilot"].Prefix)
	}

	// Step 4: Change prefix again to "xyz", save
	cfg2.ProviderModels["copilot"] = ProviderModelsConfig{
		Prefix: "xyz",
		UseAll: true,
		Models: cfg2.ProviderModels["copilot"].Models,
	}
	if err := SaveConfigPreserveComments(cfgPath, cfg2); err != nil {
		t.Fatal(err)
	}

	// Step 5: Reload and verify
	cfg3, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.ProviderModels["copilot"].Prefix != "xyz" {
		t.Errorf("expected prefix=xyz after second save, got %q", cfg3.ProviderModels["copilot"].Prefix)
	}

	// Dump YAML for debugging
	data, _ := os.ReadFile(cfgPath)
	t.Logf("Final YAML:\n%s", string(data))
}

// TestSaveConfigPreserveComments_ProviderModelsAddSecondProvider verifies that
// adding a new provider to provider-models persists correctly.
func TestSaveConfigPreserveComments_ProviderModelsAddSecondProvider(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Step 1: Write initial config with provider-models.copilot
	initial := `
port: 8080
provider-models:
  copilot:
    prefix: ghc
    use_all: true
    models:
      - id: gpt-5.3-codex
        display_name: GPT 5.3 Codex
        type: copilot
        owned_by: github
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 2: Load config, add gemini provider, save
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ProviderModels["gemini"] = ProviderModelsConfig{
		Prefix: "gg",
		UseAll: false,
		Models: nil,
	}

	if err := SaveConfigPreserveComments(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	// Step 3: Reload and verify both providers exist
	cfg2, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.ProviderModels["copilot"].Prefix != "ghc" {
		t.Errorf("expected copilot prefix=ghc, got %q", cfg2.ProviderModels["copilot"].Prefix)
	}
	if cfg2.ProviderModels["gemini"].Prefix != "gg" {
		t.Errorf("expected gemini prefix=gg, got %q", cfg2.ProviderModels["gemini"].Prefix)
	}

	// Step 4: Add a third provider, save
	cfg2.ProviderModels["openai"] = ProviderModelsConfig{
		Prefix: "oai",
		UseAll: false,
		Models: nil,
	}
	if err := SaveConfigPreserveComments(cfgPath, cfg2); err != nil {
		t.Fatal(err)
	}

	// Step 5: Reload and verify all three
	cfg3, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg3.ProviderModels["copilot"].Prefix != "ghc" {
		t.Errorf("expected copilot prefix=ghc, got %q", cfg3.ProviderModels["copilot"].Prefix)
	}
	if cfg3.ProviderModels["gemini"].Prefix != "gg" {
		t.Errorf("expected gemini prefix=gg, got %q", cfg3.ProviderModels["gemini"].Prefix)
	}
	if cfg3.ProviderModels["openai"].Prefix != "oai" {
		t.Errorf("expected openai prefix=oai, got %q", cfg3.ProviderModels["openai"].Prefix)
	}

	// Dump YAML for debugging
	data, _ := os.ReadFile(cfgPath)
	t.Logf("Final YAML:\n%s", string(data))
}

// TestSaveConfigPreserveComments_ProviderModelsFromEmpty verifies that
// adding provider-models when it doesn't exist in the original config works.
func TestSaveConfigPreserveComments_ProviderModelsFromEmpty(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Step 1: Write config without provider-models
	initial := `
port: 8080
debug: false
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	// Step 2: Load config, add provider-models, save
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	cfg.ProviderModels = map[string]ProviderModelsConfig{
		"copilot": {
			Prefix: "ghc",
			UseAll: true,
			Models: []ProviderModelEntry{
				{ID: "gpt-5.3-codex", DisplayName: "GPT 5.3 Codex", Type: "copilot", OwnedBy: "github"},
			},
		},
	}

	if err := SaveConfigPreserveComments(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	// Step 3: Reload and verify
	cfg2, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.ProviderModels["copilot"].Prefix != "ghc" {
		t.Errorf("expected copilot prefix=ghc, got %q", cfg2.ProviderModels["copilot"].Prefix)
	}

	// Dump YAML for debugging
	data, _ := os.ReadFile(cfgPath)
	t.Logf("Final YAML:\n%s", string(data))
}

// TestSaveConfigPreserveComments_ProviderModelsPrefixRoundtrip verifies the
// full roundtrip: load -> modify prefix -> save -> load -> verify.
func TestSaveConfigPreserveComments_ProviderModelsPrefixRoundtrip(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Write initial config
	initial := `
port: 8080
provider-models:
  copilot:
    use_all: true
    models:
      - id: gpt-5.3-codex
        display_name: GPT 5.3 Codex
        type: copilot
        owned_by: github
      - id: gemini-3.1-pro-preview
        display_name: Gemini 3.1 Pro Preview
        type: copilot
        owned_by: google
    prefix: ghc
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate the PatchProviderModels flow:
	// 1. Load config
	// 2. Update provider entry in memory
	// 3. Save
	// 4. Reload (simulating config watcher)
	// 5. Verify prefix

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate editing prefix from "ghc" to "abc"
	cfg.ProviderModels["copilot"] = ProviderModelsConfig{
		Prefix: "abc",
		UseAll: true,
		Models: cfg.ProviderModels["copilot"].Models,
	}

	if err := SaveConfigPreserveComments(cfgPath, cfg); err != nil {
		t.Fatal(err)
	}

	// Simulate config watcher reload
	reloaded, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if reloaded.ProviderModels["copilot"].Prefix != "abc" {
		t.Errorf("BUG: expected prefix=abc after roundtrip, got %q", reloaded.ProviderModels["copilot"].Prefix)
	}

	// Now simulate editing prefix from "abc" to "xyz"
	reloaded.ProviderModels["copilot"] = ProviderModelsConfig{
		Prefix: "xyz",
		UseAll: true,
		Models: reloaded.ProviderModels["copilot"].Models,
	}

	if err := SaveConfigPreserveComments(cfgPath, reloaded); err != nil {
		t.Fatal(err)
	}

	// Simulate config watcher reload again
	reloaded2, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	if reloaded2.ProviderModels["copilot"].Prefix != "xyz" {
		t.Errorf("BUG: expected prefix=xyz after second roundtrip, got %q", reloaded2.ProviderModels["copilot"].Prefix)
	}

	// Dump YAML for debugging
	data, _ := os.ReadFile(cfgPath)
	t.Logf("Final YAML:\n%s", string(data))
}
