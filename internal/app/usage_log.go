package app

import (
	"bytes"
	"encoding/json"
	"log"
)

// loggedUsageRecorder logs one line per finished chat request with the client
// key, model, upstream account and tokens, so the WebUI Logs tab shows who is
// spending what, and on which upstream account.
type loggedUsageRecorder struct {
	inner   usageRecorder
	keyName string
	model   string
	account string
}

func (r *loggedUsageRecorder) Record(prompt, completion, cacheRead, cacheWrite int) {
	r.inner.Record(prompt, completion, cacheRead, cacheWrite)
	log.Printf("[INFO] usage key=%q model=%q account=%q prompt=%d completion=%d cache_read=%d cache_write=%d",
		r.keyName, r.model, r.account, prompt, completion, cacheRead, cacheWrite)
}

// openAIUsageSniffer watches a relayed OpenAI-compatible response (JSON or
// SSE) and extracts its token usage without altering the bytes. Zen output is
// relayed verbatim, so this is the only place its usage can be counted.
type openAIUsageSniffer struct {
	line     []byte
	body     bytes.Buffer
	overflow bool

	found                         bool
	prompt, completion, cacheRead int
}

// openAIUsageSnifferBodyCap bounds the buffered non-streaming body; past it
// only SSE "data:" lines are inspected.
const openAIUsageSnifferBodyCap = 4 << 20

// openAIUsageSnifferLineCap drops a pathological unterminated line instead of
// growing without bound.
const openAIUsageSnifferLineCap = 1 << 20

func (s *openAIUsageSniffer) Write(p []byte) {
	if !s.overflow {
		if s.body.Len()+len(p) > openAIUsageSnifferBodyCap {
			s.overflow = true
			s.body = bytes.Buffer{}
		} else {
			s.body.Write(p)
		}
	}
	s.line = append(s.line, p...)
	for {
		i := bytes.IndexByte(s.line, '\n')
		if i < 0 {
			break
		}
		s.consumeLine(s.line[:i])
		s.line = s.line[i+1:]
	}
	if len(s.line) > openAIUsageSnifferLineCap {
		s.line = nil
	}
}

func (s *openAIUsageSniffer) consumeLine(line []byte) {
	line = bytes.TrimSpace(line)
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if !bytes.Contains(payload, []byte(`"usage"`)) {
		return
	}
	s.parse(payload)
}

type sniffedUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// parse reads chat-completions usage ({"usage":{prompt_tokens,…}}) and the
// Responses API shape ({"response":{"usage":{input_tokens,…}}}). The last
// non-empty usage wins, matching streams that report it on the final chunk.
func (s *openAIUsageSniffer) parse(data []byte) {
	var payload struct {
		Usage    *sniffedUsage `json:"usage"`
		Response *struct {
			Usage *sniffedUsage `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	u := payload.Usage
	if u == nil && payload.Response != nil {
		u = payload.Response.Usage
	}
	if u == nil {
		return
	}
	prompt, completion, cached := u.PromptTokens, u.CompletionTokens, 0
	if prompt == 0 && completion == 0 {
		prompt, completion = u.InputTokens, u.OutputTokens
	}
	if u.PromptTokensDetails != nil {
		cached = u.PromptTokensDetails.CachedTokens
	} else if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	if prompt == 0 && completion == 0 {
		return
	}
	s.found = true
	s.prompt, s.completion, s.cacheRead = prompt, completion, cached
}

// Usage finishes the scan (a trailing unterminated line, or a plain JSON body)
// and returns what was found; zeros when the response carried no usage.
func (s *openAIUsageSniffer) Usage() (prompt, completion, cacheRead int) {
	if len(s.line) > 0 {
		s.consumeLine(s.line)
		s.line = nil
	}
	if !s.found && !s.overflow && s.body.Len() > 0 {
		s.parse(bytes.TrimSpace(s.body.Bytes()))
	}
	return s.prompt, s.completion, s.cacheRead
}
