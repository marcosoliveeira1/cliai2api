package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Gateway names, used as model prefixes and registry keys.
const (
	GatewayCmdcode  = "cmdcode"
	GatewayOpencode = "opencode"
)

// Gateway model prefixes, including the trailing slash.
const (
	CmdcodePrefix  = GatewayCmdcode + "/"
	OpencodePrefix = GatewayOpencode + "/"
)

// ErrUnknownGateway is returned by SplitModel when the model carries a
// prefix that matches no registered gateway.
var ErrUnknownGateway = errors.New("unknown model prefix")

// Gateway is the contract every upstream provider implements. Each gateway
// owns its AccountPool for round-robin rotation and failover.
type Gateway interface {
	Name() string
	ModelPrefix() string
	Pool() *AccountPool
	BaseURL() string
	SetBaseURL(string)
	Chat(ctx context.Context, req *ChatRequest) (*http.Response, *Account, error)
	FetchModels() []ModelInfo
}

// HeaderAwareGateway receives the caller headers when upstream request
// identity (such as Zen session affinity) needs to be preserved.
type HeaderAwareGateway interface {
	ChatWithHeaders(ctx context.Context, req *ChatRequest, inbound http.Header) (*http.Response, *Account, error)
}

// SplitModel routes a model ID to its gateway and strips the prefix.
// A bare ID without "/" defaults to cmdcode for legacy compatibility.
// An empty model or an unrecognized prefix is an error.
func SplitModel(model string) (gateway, bareID string, err error) {
	if model == "" {
		return "", "", fmt.Errorf("%w: %q", ErrUnknownGateway, model)
	}
	if rest, ok := strings.CutPrefix(model, OpencodePrefix); ok {
		return GatewayOpencode, rest, nil
	}
	if rest, ok := strings.CutPrefix(model, CmdcodePrefix); ok {
		return GatewayCmdcode, rest, nil
	}
	if !strings.Contains(model, "/") {
		return GatewayCmdcode, model, nil
	}
	return "", "", fmt.Errorf("%w: %q", ErrUnknownGateway, model)
}

// Registry holds the configured gateways and resolves them by name.
type Registry struct {
	gateways map[string]Gateway
	def      string
}

func NewRegistry(defaultName string, gateways ...Gateway) *Registry {
	r := &Registry{gateways: make(map[string]Gateway, len(gateways)), def: defaultName}
	for _, g := range gateways {
		r.gateways[g.Name()] = g
	}
	return r
}

// Get returns the gateway registered under name, or nil when absent.
func (r *Registry) Get(name string) Gateway {
	return r.gateways[name]
}

// Default returns the gateway used for unprefixed models.
func (r *Registry) Default() Gateway {
	return r.gateways[r.def]
}
