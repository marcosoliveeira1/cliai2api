package app

import (
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// AccountConfig is one upstream Command Code credential in config.yaml.
type AccountConfig struct {
	Name    string `yaml:"name"`
	APIKey  string `yaml:"api_key"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil means enabled
}

func (a AccountConfig) IsEnabled() bool {
	return a.Enabled == nil || *a.Enabled
}

// Default upstream base URLs for the configured gateways.
const (
	DefaultCmdcodeBaseURL = "https://api.commandcode.ai"
	DefaultZenBaseURL     = "https://opencode.ai/zen"
)

// GatewayZen is the config key for the Zen upstream under gateways:. The
// models it serves use the "opencode/" prefix (GatewayOpencode in
// gateway.go); the config key stays "zen" per the design YAML.
const GatewayZen = "zen"

// GatewayConfig is one upstream provider section under gateways: in
// config.yaml. It mirrors the legacy commandcode shape (base_url + accounts).
// Config keys are "cmdcode" and "zen"; the zen gateway serves the "opencode/"
// model prefix (wired in a later task).
type GatewayConfig struct {
	// APIKey is the legacy single-account field inside a gateway section. It
	// is migrated into Accounts on load and cleared on save once accounts
	// exist.
	APIKey   string          `yaml:"api_key,omitempty"`
	BaseURL  string          `yaml:"base_url,omitempty"`
	Accounts []AccountConfig `yaml:"accounts,omitempty"`
}

// ClientKeyConfig is one local bearer key that clients use to call this
// gateway.
type ClientKeyConfig struct {
	Name    string `yaml:"name"`
	Key     string `yaml:"key"`
	Enabled *bool  `yaml:"enabled,omitempty"` // nil means enabled
}

func (k ClientKeyConfig) IsEnabled() bool {
	return k.Enabled == nil || *k.Enabled
}

type Config struct {
	APIKey string `yaml:"api_key,omitempty"`
	// APIKeys is the list of local client keys. The legacy single api_key
	// field above is migrated into it on load and cleared on save once the
	// list is non-empty.
	APIKeys []ClientKeyConfig `yaml:"api_keys,omitempty"`
	// AdminPassword guards the WebUI admin API. Generated on first start when
	// empty; changeable at runtime via the admin API.
	AdminPassword string `yaml:"admin_password,omitempty"`
	// WebUI controls whether the binary serves the embedded admin interface.
	// nil means enabled.
	WebUI *bool  `yaml:"webui,omitempty"`
	Host  string `yaml:"host"`
	Port  int    `yaml:"port"`

	CommandCode struct {
		// APIKey is the legacy single-account field. It is migrated into
		// Gateways[cmdcode].Accounts on load and cleared on save once the
		// new format holds accounts. Kept only for legacy read/migration;
		// new writes always use Gateways.
		APIKey   string          `yaml:"api_key,omitempty"`
		BaseURL  string          `yaml:"base_url,omitempty"`
		Accounts []AccountConfig `yaml:"accounts,omitempty"`
	} `yaml:"commandcode,omitempty"`

	// Gateways holds one section per upstream provider ("cmdcode", "zen").
	// It is the source of truth going forward; commandcode above is only
	// read for legacy migration.
	Gateways map[string]*GatewayConfig `yaml:"gateways,omitempty"`

	ExcludeModels  []string `yaml:"exclude_models"`
	Debug          bool     `yaml:"-"` // runtime flag, not persisted
	migratedLegacy bool     `yaml:"-"`

	// mu guards the fields that the admin API mutates while request handlers
	// read them (ExcludeModels, CommandCode/Gateways base URLs,
	// AdminPassword).
	mu sync.RWMutex
}

// gatewaySectionHasData reports whether a gateway section carries anything
// worth persisting (accounts, base URL, or the legacy single-key field).
func gatewaySectionHasData(gc *GatewayConfig) bool {
	return gc != nil && (len(gc.Accounts) > 0 || gc.BaseURL != "" || gc.APIKey != "")
}

// migrateSectionAPIKeys folds a gateway section's legacy single api_key field
// into its accounts list. Callers must hold c.mu (load/save paths).
func migrateSectionAPIKeys(gc *GatewayConfig) {
	if gc == nil {
		return
	}
	if len(gc.Accounts) == 0 && gc.APIKey != "" {
		gc.Accounts = []AccountConfig{{Name: "default", APIKey: gc.APIKey}}
	}
}

// promoteLegacyLocked folds the legacy commandcode section into
// Gateways["cmdcode"] and zeroes the legacy section. The new section always
// wins on the base URL (a T2-written value must survive a legacy mirror);
// legacy accounts are authoritative only when the new section has none
// (AccountPool.SyncToConfig writes the legacy list until T8). It returns
// true when a migration happened.
func (c *Config) promoteLegacyLocked() bool {
	if len(c.CommandCode.Accounts) == 0 && c.CommandCode.BaseURL == "" && c.CommandCode.APIKey == "" {
		return false
	}
	if c.Gateways == nil {
		c.Gateways = map[string]*GatewayConfig{}
	}
	gc := c.Gateways[GatewayCmdcode]
	if gc == nil {
		gc = &GatewayConfig{}
		c.Gateways[GatewayCmdcode] = gc
	}
	if len(gc.Accounts) == 0 {
		gc.Accounts = c.CommandCode.Accounts
	}
	if gc.BaseURL == "" {
		gc.BaseURL = c.CommandCode.BaseURL
	}
	if len(gc.Accounts) == 0 && gc.APIKey == "" {
		gc.APIKey = c.CommandCode.APIKey
	}
	c.CommandCode = struct {
		APIKey   string          `yaml:"api_key,omitempty"`
		BaseURL  string          `yaml:"base_url,omitempty"`
		Accounts []AccountConfig `yaml:"accounts,omitempty"`
	}{}
	return true
}

// mirrorCmdcodeLocked refreshes the legacy commandcode section as a read
// mirror of Gateways["cmdcode"] so existing direct readers (app wiring,
// admin handlers) keep working until T8/T9 take over. It runs after the
// marshal in saveConfig so the file carries gateways only. Callers must
// hold c.mu.
func (c *Config) mirrorCmdcodeLocked() {
	c.CommandCode = struct {
		APIKey   string          `yaml:"api_key,omitempty"`
		BaseURL  string          `yaml:"base_url,omitempty"`
		Accounts []AccountConfig `yaml:"accounts,omitempty"`
	}{}
	if gc := c.Gateways[GatewayCmdcode]; gc != nil {
		c.CommandCode.BaseURL = gc.BaseURL
		c.CommandCode.Accounts = gc.Accounts
	}
}

func (c *Config) WebUIEnabled() bool {
	return c.WebUI == nil || *c.WebUI
}

func (c *Config) Excludes() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.ExcludeModels
}

func (c *Config) SetExcludes(list []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ExcludeModels = list
}

// GatewayBaseURL returns the base URL of the named gateway ("cmdcode",
// "zen"). Empty when unset.
func (c *Config) GatewayBaseURL(name string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if gc := c.Gateways[name]; gc != nil {
		return gc.BaseURL
	}
	return ""
}

// SetGatewayBaseURL sets the base URL of the named gateway, creating its
// section (and the map) when missing. "cmdcode" also writes the legacy
// section, which stays the live copy for pre-T8 callers until T8.
func (c *Config) SetGatewayBaseURL(name, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Gateways == nil {
		c.Gateways = map[string]*GatewayConfig{}
	}
	gc, ok := c.Gateways[name]
	if !ok || gc == nil {
		gc = &GatewayConfig{}
		c.Gateways[name] = gc
	}
	gc.BaseURL = url
	if name == GatewayCmdcode {
		c.CommandCode.BaseURL = url
	}
}

// UpstreamBaseURL returns the cmdcode gateway base URL (legacy accessor,
// kept for the pre-T8 callers; delegates to the gateway sections).
func (c *Config) UpstreamBaseURL() string { return c.GatewayBaseURL(GatewayCmdcode) }

// SetUpstreamBaseURL sets the cmdcode gateway base URL (legacy accessor).
func (c *Config) SetUpstreamBaseURL(url string) { c.SetGatewayBaseURL(GatewayCmdcode, url) }

func (c *Config) adminPassword() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.AdminPassword
}

func (c *Config) setAdminPassword(password string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.AdminPassword = password
}

func defaultConfig() (*Config, error) {
	apiKey, err := genAPIKey()
	if err != nil {
		return nil, err
	}
	adminPassword, err := genAdminPassword()
	if err != nil {
		return nil, err
	}
	// 生成的客户端密钥只写入 api_keys 列表；旧字段 api_key 仅用于兼容
	// 已有的手工配置，不再出现在新生成的文件里。
	c := &Config{
		AdminPassword: adminPassword,
		Host:          "localhost",
		Port:          11434,
		ExcludeModels: []string{"gpt-", "claude-", "gemini-"},
	}
	c.APIKeys = []ClientKeyConfig{{Name: "default", Key: apiKey}}
	c.Gateways = map[string]*GatewayConfig{
		GatewayCmdcode: {BaseURL: DefaultCmdcodeBaseURL},
		GatewayZen:     {BaseURL: DefaultZenBaseURL},
	}
	return c, nil
}

func genAPIKey() (string, error) {
	key, err := randomHex(24)
	if err != nil {
		return "", fmt.Errorf("generate api key: %w", err)
	}
	return "ccgw-" + key, nil
}

func genAdminPassword() (string, error) {
	// 12 位纯随机字符即可，不加可读前缀
	return randomPassword(12)
}

// loadConfig reads config.yaml and migrates the legacy single-key fields into
// the accounts and client-keys lists. A legacy commandcode section (accounts,
// base_url, api_key) is promoted into Gateways["cmdcode"] and mirrored back
// so existing readers keep working until T8 takes over the wiring.
func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if migrated := cfg.promoteLegacyLocked(); migrated {
		cfg.migratedLegacy = true
		if _, ok := cfg.Gateways[GatewayZen]; !ok {
			cfg.Gateways[GatewayZen] = &GatewayConfig{BaseURL: DefaultZenBaseURL}
		}
	}
	for _, gc := range cfg.Gateways {
		migrateSectionAPIKeys(gc)
		if len(gc.Accounts) > 0 {
			gc.APIKey = ""
		}
	}
	if len(cfg.APIKeys) == 0 && cfg.APIKey != "" {
		cfg.APIKeys = []ClientKeyConfig{{Name: "default", Key: cfg.APIKey}}
	}
	cfg.mirrorCmdcodeLocked()
	return &cfg, nil
}

// persistLegacyMigration writes the gateway-form config immediately after a
// legacy file is loaded, so migration does not depend on later admin changes.
func persistLegacyMigration(path string, cfg *Config) error {
	if !cfg.migratedLegacy {
		return nil
	}
	if err := saveConfig(path, cfg); err != nil {
		return err
	}
	cfg.migratedLegacy = false
	return nil
}

func saveConfig(path string, cfg *Config) error {
	cfg.mu.Lock()
	// Non-empty lists are the source of truth; keeping the legacy fields
	// would resurrect deleted keys on the next load.
	if cfg.promoteLegacyLocked() {
		if _, ok := cfg.Gateways[GatewayZen]; !ok {
			cfg.Gateways[GatewayZen] = &GatewayConfig{BaseURL: DefaultZenBaseURL}
		}
	}
	for _, gc := range cfg.Gateways {
		if gc == nil {
			continue
		}
		if len(gc.Accounts) > 0 {
			gc.APIKey = ""
		}
	}
	if len(cfg.APIKeys) > 0 {
		cfg.APIKey = ""
	}
	// Persist gateways only: zero the in-memory legacy mirror (used by the
	// pre-T8 callers) for the marshal, then rebuild it from gateways so
	// live readers keep working after the save.
	cfg.CommandCode = struct {
		APIKey   string          `yaml:"api_key,omitempty"`
		BaseURL  string          `yaml:"base_url,omitempty"`
		Accounts []AccountConfig `yaml:"accounts,omitempty"`
	}{}
	data, err := yaml.Marshal(cfg)
	cfg.mirrorCmdcodeLocked()
	cfg.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func writeConfigTemplate(path string, cfg *Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	template := "# cmdcode2api configuration\n" +
		"# See README.md for all options.\n" +
		"\n" +
		"# exclude_models is enabled by default for premium/non-open-source models\n" +
		"# (e.g., GPT, Claude, Gemini) that may be unavailable on certain plans.\n" +
		"# Remove entries below or set exclude_models: [] to make all models available.\n" +
		"\n" +
		"# commandcode.accounts holds one or more upstream API keys; requests are\n" +
		"# rotated across them. api_keys holds the local bearer keys that clients\n" +
		"# use to call this gateway; both are editable in the WebUI at /webui.\n" +
		"\n" +
		string(data)
	return os.WriteFile(path, []byte(template), 0600)
}
