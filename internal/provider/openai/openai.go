// Package openai implements provider.Provider with the OpenAI Responses
// API and an API key, on OpenAI or (base_url) another API in its format. Requests set store to false: OpenAI keeps no
// conversation, and ackn4th saves it with the task as usual.
//
// Requests go through a rate-limit gate shared by every agent on the same
// key (see internal/ratelimit): agents slow down when the key's
// remaining requests or tokens run low, and back off together after a
// 429, instead of each burning retries.
package openai

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/dethmasque/ackn4th/internal/provider"
	"github.com/dethmasque/ackn4th/internal/ratelimit"
)

// Name is the provider's name in config and task files.
const Name = "openai"

// KeyURL is where users create an API key.
const KeyURL = "https://platform.openai.com/api-keys"

// baseURL is pinned, so the key only ever goes to OpenAI.
const baseURL = "https://api.openai.com/v1/"

// Config selects the model and its limits.
type Config struct {
	APIKey    string
	Model     string
	MaxTokens int64
	Effort    string // reasoning effort; empty uses the model default
	// CompactAt is the prompt size, in tokens, past which the conversation
	// is summarized at the start of the next turn (0 never).
	CompactAt int64
	// BaseURL is another API in the Responses format (providers.openai.
	// base_url, already checked); empty is OpenAI.
	BaseURL string
}

// Provider talks to OpenAI.
type Provider struct {
	client openai.Client
	cfg    Config
}

// New returns a provider. OPENAI_API_KEY, OPENAI_BASE_URL and similar
// variables are never read: the key and endpoint are set explicitly.
func New(cfg Config) *Provider {
	base := cmpOr(cfg.BaseURL, baseURL)
	gate := ratelimit.For(Name + ":" + ratelimit.Fingerprint(cfg.APIKey))
	return &Provider{cfg: cfg, client: openai.NewClient(
		option.WithAPIKey(cfg.APIKey),
		option.WithBaseURL(base),
		option.WithHTTPClient(ratelimit.Client(gate)),
		option.WithMaxRetries(4),
	)}
}

func (p *Provider) Name() string  { return Name }
func (p *Provider) Model() string { return p.cfg.Model }

// ErrNoModel means no model is set: login picks one the key can use.
var ErrNoModel = errors.New("openai: no model chosen yet: run `ackn4th login --provider openai`, which picks one your key can use, or set providers.openai.model")

// Verify checks that the key works and that the model answers the kind of
// request agents send: tools offered, with the configured effort.
func (p *Provider) Verify(ctx context.Context) error {
	if p.cfg.Model == "" {
		return ErrNoModel
	}
	return p.describe(p.probe(ctx, p.cfg.Model))
}

// probe sends model a tiny request with a tool, as an agent's would be.
func (p *Provider) probe(ctx context.Context, model string) error {
	params := responses.ResponseNewParams{
		Model:           model,
		Input:           responses.ResponseNewParamsInputUnion{OfString: param.NewOpt("Reply with OK.")},
		Tools:           toolParams([]provider.ToolSpec{{Name: "noop", Description: "Does nothing.", Schema: map[string]any{"type": "object", "properties": map[string]any{}}}}),
		MaxOutputTokens: param.NewOpt(int64(16)),
		Store:           param.NewOpt(false),
	}
	p.reasoning(&params)
	_, err := p.client.Responses.New(ctx, params)
	return err
}

// PickModel finds the newest chat model this key can use that answers an
// agent's kind of request: full models before mini and nano ones. Only
// the first few candidates are tried, each with a few-token request.
func (p *Provider) PickModel(ctx context.Context) (string, error) {
	var models []openai.Model
	iter := p.client.Models.ListAutoPaging(ctx)
	for iter.Next() {
		if m := iter.Current(); chatModel(m.ID) {
			models = append(models, m)
		}
	}
	if err := iter.Err(); err != nil {
		return "", p.describe(err)
	}
	slices.SortStableFunc(models, func(a, b openai.Model) int {
		return cmp.Or(cmp.Compare(tier(a.ID), tier(b.ID)), cmp.Compare(b.Created, a.Created), strings.Compare(a.ID, b.ID))
	})
	// Each model's refusal, in the order first seen, with the models that
	// gave it: they usually all fail for the same reason.
	var reasons []string
	failed := map[string][]string{}
	for _, m := range models[:min(len(models), 5)] {
		err := p.probe(ctx, m.ID)
		if err == nil {
			return m.ID, nil
		}
		var apiErr *openai.Error
		if !errors.As(err, &apiErr) || apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusTooManyRequests {
			return "", p.describe(err) // the key or the network, not the model
		}
		// Messages name the model: without it, equal reasons group.
		why := strings.ReplaceAll(strings.TrimRight(apiErr.Message, ". "), m.ID, "this model")
		why = cmpOr(why, http.StatusText(apiErr.StatusCode))
		if failed[why] == nil {
			reasons = append(reasons, why)
		}
		failed[why] = append(failed[why], m.ID)
	}
	if len(reasons) == 0 {
		return "", errors.New("openai: this key lists no chat models: check the project's model access at https://platform.openai.com/settings")
	}
	var b strings.Builder
	for i, why := range reasons {
		if i > 0 {
			b.WriteString("; ")
		}
		fmt.Fprintf(&b, "%s: %s", strings.Join(failed[why], ", "), why)
	}
	return "", fmt.Errorf("openai: no model your key lists accepts an agent's request (%s): set providers.openai.model in config.yaml to a chat model that supports tools", b.String())
}

// dated matches a snapshot suffix (gpt-5-2025-08-07): the undated alias is
// preferred.
var dated = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

// chatModel reports whether id names a general chat model, not an audio,
// image, search or other special-purpose one.
func chatModel(id string) bool {
	if !strings.HasPrefix(id, "gpt-") || dated.MatchString(id) {
		return false
	}
	for _, s := range []string{"audio", "realtime", "tts", "transcribe", "image", "search", "instruct", "codex", "-pro", "oss", "chat-latest", "preview"} {
		if strings.Contains(id, s) {
			return false
		}
	}
	return true
}

// tier ranks full models before mini, then nano ones.
func tier(id string) int {
	switch {
	case strings.Contains(id, "nano"):
		return 2
	case strings.Contains(id, "mini"):
		return 1
	}
	return 0
}

// The conversation is kept as Responses API input items, in OpenAI's wire
// shape. With store off OpenAI keeps nothing, so reasoning comes back
// encrypted and goes in the next request with the tool results it led to.
type state struct {
	Items []json.RawMessage `json:"items,omitempty"`
	// Messages is the history as versions on Chat Completions saved it.
	Messages []message           `json:"messages,omitempty"`
	Pending  []provider.ToolCall `json:"pending,omitempty"`
	Context  int64               `json:"context,omitempty"`
}

// message is one conversation entry as older versions saved it.
type message struct {
	Role      string              `json:"role"` // user, assistant, tool
	Text      string              `json:"text,omitempty"`
	ToolCalls []provider.ToolCall `json:"tool_calls,omitempty"`
	CallID    string              `json:"call_id,omitempty"`
}

type session struct {
	p       *Provider
	system  provider.System
	tools   []responses.ToolUnionParam
	items   []json.RawMessage
	pending []provider.ToolCall
	context int64 // the last request's prompt tokens
}

func (p *Provider) NewSession(system provider.System, tools []provider.ToolSpec) provider.Session {
	return &session{p: p, system: system, tools: toolParams(tools)}
}

func (p *Provider) RestoreSession(system provider.System, tools []provider.ToolSpec, raw json.RawMessage) (provider.Session, error) {
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("openai: restore session: %w", err)
	}
	items := st.Items
	if len(items) == 0 {
		items = fromMessages(st.Messages)
	}
	for i, it := range items {
		items[i] = repairSummary(it)
	}
	return &session{p: p, system: system, tools: toolParams(tools), items: items, pending: st.Pending, context: st.Context}, nil
}

func toolParams(specs []provider.ToolSpec) []responses.ToolUnionParam {
	var out []responses.ToolUnionParam
	for _, t := range specs {
		out = append(out, responses.ToolUnionParam{OfFunction: &responses.FunctionToolParam{
			// Strict mode needs every property required and no extra
			// keywords, which ackn4th's tool schemas don't promise.
			Name: t.Name, Description: param.NewOpt(t.Description), Parameters: t.Schema, Strict: param.NewOpt(false),
		}})
	}
	return out
}

func jsonItem(v any) json.RawMessage {
	b, _ := json.Marshal(v) // maps of strings always marshal
	return b
}

func userItem(text string) json.RawMessage {
	return jsonItem(map[string]string{"role": "user", "content": text})
}

func assistantItem(text string) json.RawMessage {
	return jsonItem(map[string]string{"role": "assistant", "content": text})
}

func callItem(c provider.ToolCall) json.RawMessage {
	return jsonItem(map[string]string{"type": "function_call", "call_id": c.ID, "name": c.Name, "arguments": string(c.Input)})
}

func resultItem(callID, text string) json.RawMessage {
	return jsonItem(map[string]string{"type": "function_call_output", "call_id": callID, "output": text})
}

// repairSummary fixes a reasoning item saved with its summary as the
// string "[]", which OpenAI refuses.
func repairSummary(it json.RawMessage) json.RawMessage {
	var r map[string]json.RawMessage
	if json.Unmarshal(it, &r) != nil || string(r["type"]) != `"reasoning"` || string(r["summary"]) != `"[]"` {
		return it
	}
	r["summary"] = json.RawMessage("[]")
	return jsonItem(r)
}

// fromMessages converts a history saved by an older version.
func fromMessages(ms []message) []json.RawMessage {
	var out []json.RawMessage
	for _, m := range ms {
		switch m.Role {
		case "user":
			out = append(out, userItem(m.Text))
		case "tool":
			out = append(out, resultItem(m.CallID, m.Text))
		case "assistant":
			if m.Text != "" {
				out = append(out, assistantItem(m.Text))
			}
			for _, c := range m.ToolCalls {
				out = append(out, callItem(c))
			}
		}
	}
	return out
}

func (s *session) PendingToolCalls() []provider.ToolCall { return s.pending }

func (s *session) State() (json.RawMessage, error) {
	return json.Marshal(state{Items: s.items, Pending: s.pending, Context: s.context})
}

func (s *session) SendText(ctx context.Context, text string, st provider.Stream) (*provider.Reply, error) {
	// Calls the runner never answered still need answers in the history.
	for _, c := range s.pending {
		s.items = append(s.items, resultItem(c.ID, "Error: not run (the turn ended)."))
	}
	s.pending = nil
	// A new turn is the only safe point to compact: no tool round is open.
	var cost *provider.Usage
	if s.p.cfg.CompactAt > 0 && s.context >= s.p.cfg.CompactAt {
		u, err := s.compact(ctx)
		if err != nil {
			return nil, err
		}
		cost = &u
	}
	s.items = append(s.items, userItem(text))
	reply, err := s.send(ctx, st)
	if reply != nil && cost != nil {
		reply.Compacted = true
		reply.Usage.Add(*cost)
	}
	return reply, err
}

// compact replaces the whole history with a summary the model writes,
// using the same instructions and tools so the request shares the cached
// prefix.
func (s *session) compact(ctx context.Context) (provider.Usage, error) {
	var cost provider.Usage
	p, input := s.params(userItem(provider.CompactPrompt))
	p.ToolChoice = responses.ResponseNewParamsToolChoiceUnion{OfToolChoiceMode: param.NewOpt(responses.ToolChoiceOptionsNone)}
	resp, err := s.p.client.Responses.New(ctx, p, input)
	if err != nil {
		return cost, fmt.Errorf("compacting context: %w", s.p.describe(err))
	}
	cost = usage(resp.Usage)
	summary := resp.OutputText()
	if summary == "" {
		return cost, errors.New("compacting context: the model returned no summary")
	}
	s.items = []json.RawMessage{
		userItem(provider.CompactedIntro + summary),
		assistantItem("Understood. I will continue from this summary and re-read files as needed."),
	}
	s.context = 0
	return cost, nil
}

func (s *session) SendToolResults(ctx context.Context, results []provider.ToolResult, extra string, st provider.Stream) (*provider.Reply, error) {
	byID := map[string]provider.ToolResult{}
	for _, r := range results {
		byID[r.CallID] = r
	}
	for _, c := range s.pending {
		r, ok := byID[c.ID]
		content := r.Content
		if !ok {
			content = "Error: this call was not run."
		} else if r.IsError && !strings.HasPrefix(content, "Error") {
			content = "Error: " + content
		}
		s.items = append(s.items, resultItem(c.ID, content))
	}
	s.pending = nil
	if extra != "" {
		s.items = append(s.items, userItem(extra))
	}
	return s.send(ctx, st)
}

// params builds a request for the conversation plus extra items. The
// items are sent as they were saved, through the returned option.
func (s *session) params(extra ...json.RawMessage) (responses.ResponseNewParams, option.RequestOption) {
	p := responses.ResponseNewParams{
		Model:        s.p.cfg.Model,
		Instructions: param.NewOpt(s.system.Text()),
		Tools:        s.tools,
		Store:        param.NewOpt(false),
		// Agents with the same system prompt and tools share a cache entry;
		// the key keeps them on the same cache shard.
		PromptCacheKey: param.NewOpt("ackn4th:" + ratelimit.Fingerprint(s.system.Shared)),
	}
	if s.p.cfg.MaxTokens > 0 {
		p.MaxOutputTokens = param.NewOpt(s.p.cfg.MaxTokens)
	}
	s.p.reasoning(&p)
	input := append(slices.Clip(s.items), extra...)
	return p, option.WithJSONSet("input", input)
}

// reasoning sets the effort and asks for the encrypted reasoning back.
// Without an effort nothing is set: models that don't reason refuse both.
func (p *Provider) reasoning(params *responses.ResponseNewParams) {
	if e := p.cfg.Effort; e != "" {
		params.Reasoning = shared.ReasoningParam{Effort: shared.ReasoningEffort(e)}
		params.Include = []responses.ResponseIncludable{responses.ResponseIncludableReasoningEncryptedContent}
	}
}

func usage(u responses.ResponseUsage) provider.Usage {
	return provider.Usage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CachedTokens: u.InputTokensDetails.CachedTokens}
}

func (s *session) send(ctx context.Context, st provider.Stream) (*provider.Reply, error) {
	p, input := s.params()
	stream := s.p.client.Responses.NewStreaming(ctx, p, input)
	var resp *responses.Response
	var failed string
	for stream.Next() {
		ev := stream.Current()
		switch ev.Type {
		case "response.output_text.delta":
			if st.Text != nil && ev.Delta != "" {
				st.Text(ev.Delta)
			}
		case "response.reasoning_text.delta": // compatible APIs show their reasoning
			if st.Thinking != nil && ev.Delta != "" {
				st.Thinking(ev.Delta)
			}
		case "response.output_item.added":
			if ev.Item.Type == "function_call" && st.ToolInput != nil {
				st.ToolInput(ev.Item.Name, "")
			}
		case "response.function_call_arguments.delta":
			if st.ToolInput != nil && ev.Delta != "" {
				st.ToolInput("", ev.Delta)
			}
		case "response.completed", "response.incomplete":
			r := ev.Response
			resp = &r
		case "response.failed":
			failed = cmpOr(ev.Response.Error.Message, "the response failed")
		case "error":
			failed = cmpOr(ev.Message, "the stream failed")
		}
	}
	if err := stream.Err(); err != nil {
		return nil, s.p.describe(err)
	}
	if failed != "" {
		return nil, errors.New("openai: " + failed)
	}
	if resp == nil {
		return nil, errors.New("openai: empty response")
	}
	return s.record(resp), nil
}

// outItem is the part of an output item ackn4th reads.
type outItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Summary          json.RawMessage `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

// record adds a response's output to the history and returns the reply.
func (s *session) record(resp *responses.Response) *provider.Reply {
	reply := &provider.Reply{Usage: usage(resp.Usage), ContextTokens: resp.Usage.InputTokens}
	s.context = resp.Usage.InputTokens
	var refusal string
	for _, it := range resp.Output {
		var o outItem
		if json.Unmarshal([]byte(it.RawJSON()), &o) != nil {
			continue
		}
		switch o.Type {
		case "reasoning":
			if o.EncryptedContent == "" {
				// Compatible APIs (DeepSeek) return reasoning as text instead,
				// and refuse a tool loop's history without it.
				if it := plainReasoning(o); it != nil {
					s.items = append(s.items, it)
				}
				continue // OpenAI's is useless without store
			}
			summary := o.Summary // an array, sent back as it came
			if len(summary) == 0 || string(summary) == "null" {
				summary = json.RawMessage("[]")
			}
			s.items = append(s.items, jsonItem(map[string]any{
				"type": "reasoning", "id": o.ID, "summary": summary, "encrypted_content": o.EncryptedContent,
			}))
		case "message":
			var text strings.Builder
			for _, c := range o.Content {
				text.WriteString(c.Text)
				refusal += c.Refusal
			}
			if text.Len() > 0 {
				reply.Text += text.String()
				s.items = append(s.items, assistantItem(text.String()))
			}
		case "function_call":
			args := json.RawMessage(o.Arguments)
			if !json.Valid(args) {
				args = json.RawMessage("{}")
			}
			call := provider.ToolCall{ID: o.CallID, Name: o.Name, Input: args}
			s.items = append(s.items, callItem(call))
			reply.ToolCalls = append(reply.ToolCalls, call)
		}
	}
	s.pending = reply.ToolCalls
	switch why := resp.IncompleteDetails.Reason; {
	case len(reply.ToolCalls) > 0:
		reply.StopReason = provider.StopToolUse
	case refusal != "":
		reply.StopReason, reply.StopDetail = provider.StopRefusal, refusal
	case why == "max_output_tokens":
		reply.StopReason = provider.StopMaxTokens
	case why == "content_filter":
		reply.StopReason, reply.StopDetail = provider.StopRefusal, "content filter"
	default:
		reply.StopReason = provider.StopEnd
	}
	return reply
}

// plainReasoning keeps a reasoning item's text, or nil when it has none.
func plainReasoning(o outItem) json.RawMessage {
	var parts []map[string]string
	for _, c := range o.Content {
		if c.Text != "" {
			parts = append(parts, map[string]string{"type": "reasoning_text", "text": c.Text})
		}
	}
	if len(parts) == 0 {
		return nil
	}
	it := map[string]any{"type": "reasoning", "summary": []any{}, "content": parts}
	if o.ID != "" {
		it["id"] = o.ID
	}
	return jsonItem(it)
}

// describe turns API errors into short messages. The key is never included.
func (p *Provider) describe(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		msg := apiErr.Message
		switch apiErr.StatusCode {
		case http.StatusUnauthorized:
			if u, perr := url.Parse(p.cfg.BaseURL); perr == nil && u.Host != "" {
				// Another API's message often says which key it saw (****1a2b).
				return fmt.Errorf("openai: %s rejected the API key (%s): run `ackn4th login --provider openai --reauth` with a valid one", u.Host, cmpOr(msg, "HTTP 401"))
			}
			return errors.New("openai: the API key was rejected: run `ackn4th login --provider openai --reauth` with a new one from " + KeyURL)
		case http.StatusPaymentRequired: // DeepSeek, out of balance
			return errors.New("openai: out of API credits: top up the account's balance: " + cmpOr(msg, "insufficient balance"))
		case http.StatusTooManyRequests:
			if strings.Contains(apiErr.Code, "insufficient_quota") {
				return errors.New("openai: out of API credits: add billing at https://platform.openai.com/settings/organization/billing")
			}
			return errors.New("openai: rate limited even after backing off; try fewer parallel agents, or resume later: " + msg)
		case http.StatusNotFound:
			return fmt.Errorf("openai: model not available to this key: %s", msg)
		}
		return fmt.Errorf("openai: %s (HTTP %d)", cmpOr(msg, http.StatusText(apiErr.StatusCode)), apiErr.StatusCode)
	}
	return fmt.Errorf("openai: %w", err)
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
