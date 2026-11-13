package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
)

// System One is Command Code's "decision model" endpoint: typed questions
// (noul | choice | score) about a state go in, probabilities come out. It is
// not a chat model, so it gets its own route instead of /v1/chat/completions.
// The request body is relayed as-is; only the model id is normalized.
const (
	systemOneUpstreamPath  = "/provider/v1/systemone"
	maxSystemOneBodyBytes  = 10 * 1024 * 1024
	maxSystemOneQuestions  = 100
	defaultSystemOneModel  = "typesafe/jev"
	systemOneRequestFormat = `{"model": "typesafe/jev", "state": <text or JSON>, "questions": {"<name>": {"type": "noul" | "choice" | "score", "instructions": "...", "criteria": ...}}}`
)

// systemOneModels mirrors the official CLI's alias table.
var systemOneModels = map[string]string{
	"jev":             "typesafe/jev",
	"jev-latest":      "typesafe/jev",
	"typesafe/jev":    "typesafe/jev",
	"typesafe-ai/jev": "typesafe/jev",
}

// resolveSystemOneModel accepts the CLI aliases, optionally behind the
// cmdcode/ gateway prefix used elsewhere in this proxy. Empty means default.
func resolveSystemOneModel(model string) (string, bool) {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return defaultSystemOneModel, true
	}
	m = strings.TrimPrefix(m, CmdcodePrefix)
	id, ok := systemOneModels[m]
	return id, ok
}

type systemOneRequest struct {
	Model     string                     `json:"model"`
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

type systemOneQuestion struct {
	Type     string          `json:"type"`
	Criteria json.RawMessage `json:"criteria"`
}

// validate catches the shape errors the CLI rejects locally, so callers get a
// clear 400 instead of an opaque upstream failure.
func (r *systemOneRequest) validate() error {
	if len(r.Questions) == 0 {
		return errors.New("questions must name at least one question")
	}
	if len(r.Questions) > maxSystemOneQuestions {
		return fmt.Errorf("at most %d questions per call", maxSystemOneQuestions)
	}
	for name, raw := range r.Questions {
		if name == "" {
			return errors.New("question names must not be empty")
		}
		var q systemOneQuestion
		if err := json.Unmarshal(raw, &q); err != nil {
			return fmt.Errorf("questions.%s: %v", name, err)
		}
		switch q.Type {
		case "noul":
		case "choice":
			var criteria map[string]json.RawMessage
			if json.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) == 0 {
				return fmt.Errorf("questions.%s: choice requires criteria as an object of options", name)
			}
		case "score":
			var criteria []json.RawMessage
			if json.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) < 2 {
				return fmt.Errorf("questions.%s: score requires criteria as an array of at least 2 levels", name)
			}
		default:
			return fmt.Errorf("questions.%s: type must be noul, choice or score", name)
		}
	}
	return nil
}

type systemOneUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func handleSystemOne(cc *CCClient, usage *UsageTracker) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxSystemOneBodyBytes)

		in, err := io.ReadAll(r.Body)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "bad request body: "+err.Error())
			return
		}
		// raw keeps unknown fields so they are relayed untouched; req is the
		// typed view used for validation.
		var raw map[string]json.RawMessage
		var req systemOneRequest
		if err := json.Unmarshal(in, &raw); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "a decision model takes a JSON request: "+err.Error()+". Expected: "+systemOneRequestFormat)
			return
		}
		if err := json.Unmarshal(in, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request: "+err.Error()+". Expected: "+systemOneRequestFormat)
			return
		}
		model, ok := resolveSystemOneModel(req.Model)
		if !ok {
			writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("model %q is not a System One decision model (try %q)", req.Model, defaultSystemOneModel))
			return
		}
		if len(req.State) == 0 {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "state is required. Expected: "+systemOneRequestFormat)
			return
		}
		if err := req.validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid request: "+err.Error()+". Expected: "+systemOneRequestFormat)
			return
		}

		raw["model"], _ = json.Marshal(model)
		body, err := json.Marshal(raw)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "server_error", "marshal request: "+err.Error())
			return
		}

		resp, acct, err := cc.postWithFailover(r.Context(), systemOneUpstreamPath, body)
		if err != nil {
			var upstreamErr *upstreamAPIError
			if errors.As(err, &upstreamErr) {
				log.Printf("%s gateway=%s account=%q systemone request failed: %v", colorize("[ERROR]", ansiRed), GatewayCmdcode, accountLogName(acct), upstreamErr)
				if upstreamErr.RetryAfter != "" {
					w.Header().Set("Retry-After", upstreamErr.RetryAfter)
				}
				if upstreamErr.RequestID != "" {
					w.Header().Set("x-request-id", upstreamErr.RequestID)
				}
				writeErrorWithCode(w, upstreamErr.Status, upstreamErr.Type, upstreamErr.Code, upstreamErr.Message)
				return
			}
			log.Printf("%s gateway=%s account=%q systemone request failed: %v", colorize("[ERROR]", ansiRed), GatewayCmdcode, accountLogName(acct), err)
			writeError(w, http.StatusBadGateway, "server_error", "upstream error: "+err.Error())
			return
		}
		defer resp.Body.Close()

		out, err := io.ReadAll(resp.Body)
		if err != nil {
			writeError(w, http.StatusBadGateway, "server_error", "read upstream response: "+err.Error())
			return
		}
		log.Printf("[INFO] systemone model=%q account=%q questions=%d", model, accountLogName(acct), len(req.Questions))

		var parsed struct {
			Usage systemOneUsage `json:"usage"`
		}
		if json.Unmarshal(out, &parsed) == nil {
			usage.RecorderForGateway(GatewayCmdcode, acct, clientKeyIDFrom(r.Context())).Record(parsed.Usage.InputTokens, parsed.Usage.OutputTokens, 0, 0)
			if err := usage.save(); err != nil {
				log.Printf("%s save usage failed: %v", colorize("[ERROR]", ansiRed), err)
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(out)
	}
}
