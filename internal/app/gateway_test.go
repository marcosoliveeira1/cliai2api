package app

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

func TestSplitModel(t *testing.T) {
	for _, tt := range []struct {
		name        string
		model       string
		wantGateway string
		wantBare    string
		wantErr     bool
	}{
		{name: "opencode prefix", model: "opencode/gpt-5.5", wantGateway: "opencode", wantBare: "gpt-5.5"},
		{name: "cmdcode prefix", model: "cmdcode/deepseek-v4", wantGateway: "cmdcode", wantBare: "deepseek-v4"},
		{name: "bare model defaults to cmdcode", model: "deepseek-v4", wantGateway: "cmdcode", wantBare: "deepseek-v4"},
		{name: "unknown prefix", model: "foo/bar", wantErr: true},
		{name: "empty model", model: "", wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gotGateway, gotBare, err := SplitModel(tt.model)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("SplitModel(%q) = (%q, %q), want error", tt.model, gotGateway, gotBare)
				}
				if !errors.Is(err, ErrUnknownGateway) {
					t.Fatalf("SplitModel(%q) error = %v, want ErrUnknownGateway", tt.model, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("SplitModel(%q) error: %v", tt.model, err)
			}
			if gotGateway != tt.wantGateway {
				t.Fatalf("SplitModel(%q) gateway = %q, want %q", tt.model, gotGateway, tt.wantGateway)
			}
			if gotBare != tt.wantBare {
				t.Fatalf("SplitModel(%q) bare = %q, want %q", tt.model, gotBare, tt.wantBare)
			}
		})
	}
}

// stubGateway is a minimal Gateway implementation for registry tests. The
// compile-time assertion below pins the Gateway method set required by T1:
// Name/ModelPrefix/Pool/BaseURL/SetBaseURL/Chat/FetchModels.
type stubGateway struct {
	name    string
	prefix  string
	pool    *AccountPool
	baseURL string
}

var _ Gateway = (*stubGateway)(nil)

func (s *stubGateway) Name() string        { return s.name }
func (s *stubGateway) ModelPrefix() string { return s.prefix }
func (s *stubGateway) Pool() *AccountPool  { return s.pool }
func (s *stubGateway) BaseURL() string     { return s.baseURL }
func (s *stubGateway) SetBaseURL(url string) {
	s.baseURL = url
}
func (s *stubGateway) Chat(ctx context.Context, req *ChatRequest) (*http.Response, *Account, error) {
	return nil, nil, nil
}
func (s *stubGateway) FetchModels() []ModelInfo { return nil }

func TestRegistryGetAndDefault(t *testing.T) {
	cmdcode := &stubGateway{name: GatewayCmdcode, prefix: CmdcodePrefix, pool: NewAccountPool(nil)}
	opencode := &stubGateway{name: GatewayOpencode, prefix: OpencodePrefix, pool: NewAccountPool(nil)}
	r := NewRegistry(GatewayCmdcode, cmdcode, opencode)

	if got := r.Get(GatewayOpencode); got != opencode {
		t.Fatalf("Get(opencode) did not return the registered gateway")
	}
	if got := r.Get(GatewayCmdcode); got != cmdcode {
		t.Fatalf("Get(cmdcode) did not return the registered gateway")
	}
	if got := r.Get("foo"); got != nil {
		t.Fatalf("Get(foo) = %v, want nil", got.Name())
	}
	if got := r.Default(); got != cmdcode {
		t.Fatalf("Default() did not return the cmdcode gateway")
	}
}

func TestGatewayAccessors(t *testing.T) {
	g := &stubGateway{name: GatewayOpencode, prefix: OpencodePrefix, pool: NewAccountPool(nil)}
	if g.Name() != "opencode" {
		t.Fatalf("Name() = %q, want %q", g.Name(), "opencode")
	}
	if g.ModelPrefix() != "opencode/" {
		t.Fatalf("ModelPrefix() = %q, want %q", g.ModelPrefix(), "opencode/")
	}
	if g.Pool() == nil {
		t.Fatalf("Pool() = nil, want non-nil pool")
	}
	g.SetBaseURL("https://opencode.ai/zen")
	if g.BaseURL() != "https://opencode.ai/zen" {
		t.Fatalf("BaseURL() = %q after SetBaseURL", g.BaseURL())
	}
}
