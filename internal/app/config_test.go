package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDefaultConfigHasExcludeModels(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig error: %v", err)
	}
	want := []string{"gpt-", "claude-", "gemini-"}
	if len(cfg.ExcludeModels) != len(want) {
		t.Fatalf("len(ExcludeModels) = %d, want %d", len(cfg.ExcludeModels), len(want))
	}
	for i, v := range want {
		if cfg.ExcludeModels[i] != v {
			t.Fatalf("ExcludeModels[%d] = %q, want %q", i, cfg.ExcludeModels[i], v)
		}
	}
}

func TestDefaultConfigUsesLocalhost(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig error: %v", err)
	}
	if cfg.Host != "localhost" {
		t.Fatalf("host = %q", cfg.Host)
	}
	if cfg.Port != 11434 {
		t.Fatalf("port = %d", cfg.Port)
	}
}

func TestLoadConfigExcludeModels(t *testing.T) {
	yamlData := "exclude_models:\n  - gpt-\n  - claude-\n  - gemini-\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	want := []string{"gpt-", "claude-", "gemini-"}
	if len(cfg.ExcludeModels) != len(want) {
		t.Fatalf("len(ExcludeModels) = %d, want %d", len(cfg.ExcludeModels), len(want))
	}
	for i := range want {
		if cfg.ExcludeModels[i] != want[i] {
			t.Fatalf("ExcludeModels[%d] = %q, want %q", i, cfg.ExcludeModels[i], want[i])
		}
	}
}

func TestLoadConfigNoExcludeModels(t *testing.T) {
	yamlData := "host: localhost\nport: 11434\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if cfg.ExcludeModels != nil {
		t.Fatalf("ExcludeModels = %v, want nil", cfg.ExcludeModels)
	}
}

func TestLoadConfigEmptyExcludeModels(t *testing.T) {
	yamlData := "exclude_models: []\n"
	var cfg Config
	if err := yaml.Unmarshal([]byte(yamlData), &cfg); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if len(cfg.ExcludeModels) != 0 {
		t.Fatalf("len(ExcludeModels) = %d, want 0", len(cfg.ExcludeModels))
	}
}

func TestWriteConfigTemplateIncludesDefaultExclusionComment(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	tmp := t.TempDir() + "/config.yaml"
	if err := writeConfigTemplate(tmp, cfg); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "# exclude_models is enabled by default") {
		t.Fatalf("missing default exclusion comment in:\n%s", content)
	}
	if strings.Contains(content, "# exclude_models:") {
		t.Fatalf("template should not include a duplicate commented exclude_models key:\n%s", content)
	}
	if !strings.Contains(content, "exclude_models:\n    - gpt-") {
		t.Fatalf("missing active default exclude_models in:\n%s", content)
	}
	if !strings.Contains(content, cfg.APIKeys[0].Key) {
		t.Fatalf("missing client key in:\n%s", content)
	}
	if strings.Contains(content, "api_key:") {
		t.Fatalf("fresh template must not contain the legacy api_key field:\n%s", content)
	}
}

func TestWriteConfigTemplateDefaultExclusionLoadsActive(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	tmp := t.TempDir() + "/config.yaml"
	if err := writeConfigTemplate(tmp, cfg); err != nil {
		t.Fatalf("writeConfigTemplate: %v", err)
	}
	loaded, err := loadConfig(tmp)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := []string{"gpt-", "claude-", "gemini-"}
	if len(loaded.ExcludeModels) != len(want) {
		t.Fatalf("len(ExcludeModels) = %d, want %d", len(loaded.ExcludeModels), len(want))
	}
	for i := range want {
		if loaded.ExcludeModels[i] != want[i] {
			t.Fatalf("ExcludeModels[%d] = %q, want %q", i, loaded.ExcludeModels[i], want[i])
		}
	}
}

func TestDefaultConfigHasGatewaySections(t *testing.T) {
	cfg, err := defaultConfig()
	if err != nil {
		t.Fatalf("defaultConfig: %v", err)
	}
	if got := cfg.GatewayBaseURL(GatewayCmdcode); got != "https://api.commandcode.ai" {
		t.Fatalf("cmdcode base_url = %q, want https://api.commandcode.ai", got)
	}
	if got := cfg.GatewayBaseURL(GatewayZen); got != "https://opencode.ai/zen" {
		t.Fatalf("zen base_url = %q, want https://opencode.ai/zen", got)
	}
}

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigMigratesLegacyCommandCode(t *testing.T) {
	path := writeTempConfig(t, "commandcode:\n  api_key: cc-legacy\n  base_url: https://api.commandcode.ai\n  accounts:\n  - name: a\n    api_key: cc-a\n  - name: b\n    api_key: cc-b\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	gc := cfg.Gateways[GatewayCmdcode]
	if gc == nil {
		t.Fatal("gateways.cmdcode missing after migration")
	}
	if gc.BaseURL != "https://api.commandcode.ai" {
		t.Fatalf("migrated base_url = %q", gc.BaseURL)
	}
	if len(gc.Accounts) != 2 || gc.Accounts[0].APIKey != "cc-a" || gc.Accounts[1].Name != "b" {
		t.Fatalf("migrated accounts = %+v", gc.Accounts)
	}
	if cfg.GatewayBaseURL(GatewayCmdcode) != "https://api.commandcode.ai" {
		t.Fatalf("GatewayBaseURL = %q", cfg.GatewayBaseURL(GatewayCmdcode))
	}
	if cfg.CommandCode.BaseURL != "https://api.commandcode.ai" || len(cfg.CommandCode.Accounts) != 2 {
		t.Fatalf("legacy mirror not kept: %+v", cfg.CommandCode)
	}
	zen := cfg.GatewayBaseURL(GatewayZen)
	if zen != "https://opencode.ai/zen" {
		t.Fatalf("zen default base_url = %q", zen)
	}
}

func TestLoadConfigMigratesLegacySingleAPIKey(t *testing.T) {
	path := writeTempConfig(t, "commandcode:\n  api_key: cc-legacy\n  base_url: https://api.commandcode.ai\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	gc := cfg.Gateways[GatewayCmdcode]
	if gc == nil || len(gc.Accounts) != 1 {
		t.Fatalf("migrated accounts = %+v", cfg.Gateways)
	}
	if gc.Accounts[0].APIKey != "cc-legacy" || gc.Accounts[0].Name != "default" {
		t.Fatalf("migrated account = %+v", gc.Accounts[0])
	}
}

func TestSaveConfigPersistsGatewaysAndDropsLegacy(t *testing.T) {
	path := writeTempConfig(t, "commandcode:\n  base_url: https://api.commandcode.ai\n  accounts:\n  - name: a\n    api_key: cc-a\n  api_keys:\n  - name: default\n    key: ccgw-x\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "gateways:") {
		t.Fatalf("saved config must contain gateways::\n%s", content)
	}
	if strings.Contains(content, "commandcode:") {
		t.Fatalf("saved config must not contain legacy commandcode::\n%s", content)
	}
	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	gc := reloaded.Gateways[GatewayCmdcode]
	if gc == nil || len(gc.Accounts) != 1 || gc.Accounts[0].APIKey != "cc-a" {
		t.Fatalf("round-trip accounts = %+v", reloaded.Gateways)
	}
	if gc.APIKey != "" {
		t.Fatalf("legacy single key must be cleared on save: %+v", gc)
	}
	if gc.BaseURL != "https://api.commandcode.ai" {
		t.Fatalf("round-trip base_url = %q", gc.BaseURL)
	}
}

func TestPersistLegacyMigrationWritesGatewayConfig(t *testing.T) {
	path := writeTempConfig(t, "commandcode:\n  base_url: https://api.commandcode.ai\n  accounts:\n  - name: a\n    api_key: cc-a\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.migratedLegacy {
		t.Fatal("legacy config was not marked for persistence")
	}
	if err := persistLegacyMigration(path, cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.migratedLegacy {
		t.Fatal("migration marker remained set after persistence")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "commandcode:") || !strings.Contains(string(data), "gateways:") {
		t.Fatalf("persisted migration =\n%s", data)
	}
}

func TestSaveConfigClearsMigratedGatewayAPIKey(t *testing.T) {
	path := writeTempConfig(t, "commandcode:\n  api_key: cc-legacy\n  base_url: https://api.commandcode.ai\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	gc := cfg.Gateways[GatewayCmdcode]
	if gc == nil || len(gc.Accounts) != 1 || gc.Accounts[0].APIKey != "cc-legacy" {
		t.Fatalf("migrated accounts = %+v", cfg.Gateways)
	}
	if gc.APIKey != "" {
		t.Fatalf("migrated api_key field must fold into accounts: %+v", gc)
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	rcc := reloaded.Gateways[GatewayCmdcode]
	if rcc == nil || len(rcc.Accounts) != 1 || rcc.Accounts[0].APIKey != "cc-legacy" || rcc.APIKey != "" {
		t.Fatalf("round-trip = %+v", rcc)
	}
}

func TestGatewaysRoundTrip(t *testing.T) {
	path := writeTempConfig(t, "gateways:\n  cmdcode:\n    base_url: https://api.commandcode.ai\n    accounts:\n    - name: a\n      api_key: cc-a\n  zen:\n    base_url: https://opencode.ai/zen\n    accounts:\n    - name: z\n      api_key: zen-key\n")
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveConfig(path, cfg); err != nil {
		t.Fatal(err)
	}
	reloaded, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	cc := reloaded.Gateways[GatewayCmdcode]
	zen := reloaded.Gateways[GatewayZen]
	if cc == nil || len(cc.Accounts) != 1 || cc.Accounts[0].APIKey != "cc-a" {
		t.Fatalf("cmdcode round-trip = %+v", cc)
	}
	if cc.BaseURL != "https://api.commandcode.ai" {
		t.Fatalf("cmdcode base_url = %q", cc.BaseURL)
	}
	if zen == nil || len(zen.Accounts) != 1 || zen.Accounts[0].APIKey != "zen-key" {
		t.Fatalf("zen round-trip = %+v", zen)
	}
	if zen.BaseURL != "https://opencode.ai/zen" {
		t.Fatalf("zen base_url = %q", zen.BaseURL)
	}
}
