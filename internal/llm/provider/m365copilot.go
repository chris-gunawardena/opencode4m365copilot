package provider

// Microsoft 365 Copilot provider, built on the Microsoft 365 Copilot Chat API
// (preview) in Microsoft Graph:
//
//	POST /beta/copilot/conversations                         create a conversation
//	POST /beta/copilot/conversations/{id}/chat               synchronous turn
//	POST /beta/copilot/conversations/{id}/chatOverStream     streamed turn (SSE)
//
// https://learn.microsoft.com/microsoft-365/copilot/extensibility/api/ai-services/chat/overview
//
// The Chat API is stateful and has no system prompt, model selection or tool
// calling. Each opencode session is mapped to one Copilot conversation: the
// first turn carries opencode's instructions and the tool catalog, and later
// turns only send what is new (tool results or the next user message). Tool
// calling is emulated with the text protocol in m365copilot_protocol.go.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/opencode-ai/opencode/internal/config"
	toolsPkg "github.com/opencode-ai/opencode/internal/llm/tools"
	"github.com/opencode-ai/opencode/internal/logging"
	"github.com/opencode-ai/opencode/internal/m365auth"
	"github.com/opencode-ai/opencode/internal/message"
)

const (
	m365DefaultBaseURL         = "https://graph.microsoft.com/beta/copilot"
	m365DefaultMaxMessageChars = 16000
	// m365MaxContextEntries bounds how many additionalContext entries one turn sends.
	m365MaxContextEntries = 24
	// m365IntermediateUpdate is the displayName of streamed partial snapshots.
	m365IntermediateUpdate = "Intermediate Conversation Update"
)

type m365CopilotOptions struct {
	baseURL         string
	timeZone        string
	webSearch       bool
	maxMessageChars int
	tokenSource     m365auth.TokenSource
	httpClient      *http.Client
}

type M365CopilotOption func(*m365CopilotOptions)

type M365CopilotClient ProviderClient

type m365CopilotClient struct {
	providerOptions providerClientOptions
	options         m365CopilotOptions

	mu sync.Mutex
	// conversations maps an opencode session ID to its Copilot conversation.
	conversations map[string]*m365ConversationState
}

// m365ConversationState records which opencode messages a Copilot conversation already holds.
type m365ConversationState struct {
	id     string
	synced []string // IDs of the messages sent so far, in order
}

func newM365CopilotClient(opts providerClientOptions) M365CopilotClient {
	o := m365CopilotOptions{
		baseURL:         os.Getenv("M365_COPILOT_GRAPH_URL"),
		timeZone:        os.Getenv("M365_COPILOT_TIME_ZONE"),
		webSearch:       true,
		maxMessageChars: m365DefaultMaxMessageChars,
	}
	if cfg := config.Get(); cfg != nil {
		m := cfg.M365Copilot
		if m.GraphBaseURL != "" {
			o.baseURL = m.GraphBaseURL
		}
		if m.TimeZone != "" {
			o.timeZone = m.TimeZone
		}
		if m.WebSearch != nil {
			o.webSearch = *m.WebSearch
		}
		if m.MaxMessageChars > 0 {
			o.maxMessageChars = m.MaxMessageChars
		}
	}
	switch {
	case opts.apiKey != "" && opts.apiKey != m365auth.CredentialsSentinel:
		o.tokenSource = m365auth.StaticTokenSource(opts.apiKey)
	case os.Getenv(m365auth.AccessTokenEnv) != "":
		o.tokenSource = m365auth.StaticTokenSource(os.Getenv(m365auth.AccessTokenEnv))
	default:
		o.tokenSource = m365auth.NewCacheTokenSource()
	}
	for _, apply := range opts.m365CopilotOptions {
		apply(&o)
	}
	if o.baseURL == "" {
		o.baseURL = m365DefaultBaseURL
	}
	o.baseURL = strings.TrimRight(o.baseURL, "/")
	if o.timeZone == "" {
		o.timeZone = m365DetectTimeZone()
	}
	if o.httpClient == nil {
		// No overall timeout: streamed answers can take minutes. Requests are bounded by their context.
		o.httpClient = &http.Client{}
	}
	return &m365CopilotClient{
		providerOptions: opts,
		options:         o,
		conversations:   map[string]*m365ConversationState{},
	}
}

func WithM365CopilotBaseURL(baseURL string) M365CopilotOption {
	return func(o *m365CopilotOptions) { o.baseURL = baseURL }
}

func WithM365CopilotTimeZone(timeZone string) M365CopilotOption {
	return func(o *m365CopilotOptions) { o.timeZone = timeZone }
}

func WithM365CopilotWebSearch(enabled bool) M365CopilotOption {
	return func(o *m365CopilotOptions) { o.webSearch = enabled }
}

func WithM365CopilotMaxMessageChars(n int) M365CopilotOption {
	return func(o *m365CopilotOptions) { o.maxMessageChars = n }
}

func WithM365CopilotTokenSource(ts m365auth.TokenSource) M365CopilotOption {
	return func(o *m365CopilotOptions) { o.tokenSource = ts }
}

func WithM365CopilotHTTPClient(client *http.Client) M365CopilotOption {
	return func(o *m365CopilotOptions) { o.httpClient = client }
}

// m365DetectTimeZone returns the system's IANA time zone name, which the Chat API requires.
func m365DetectTimeZone() string {
	if tz := os.Getenv("TZ"); tz != "" && !strings.HasPrefix(tz, ":") && strings.Contains(tz, "/") {
		return tz
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			return target[i+len("zoneinfo/"):]
		}
	}
	if data, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(data)); tz != "" {
			return tz
		}
	}
	if name := time.Local.String(); name != "" && name != "Local" {
		return name
	}
	return "UTC"
}

// ---------------------------------------------------------------------------
// Wire types

type m365ChatRequest struct {
	Message             m365MessageParam      `json:"message"`
	AdditionalContext   []m365ContextMessage  `json:"additionalContext,omitempty"`
	LocationHint        m365LocationHint      `json:"locationHint"`
	ContextualResources *m365ContextResources `json:"contextualResources,omitempty"`
}

type m365MessageParam struct {
	Text string `json:"text"`
}

type m365ContextMessage struct {
	Text        string `json:"text"`
	Description string `json:"description,omitempty"`
}

type m365LocationHint struct {
	TimeZone string `json:"timeZone"`
}

type m365ContextResources struct {
	WebContext *m365WebContext `json:"webContext,omitempty"`
}

type m365WebContext struct {
	IsWebEnabled bool `json:"isWebEnabled"`
}

type m365ConversationResponse struct {
	ID          string                `json:"id"`
	DisplayName string                `json:"displayName"`
	State       string                `json:"state"`
	TurnCount   int                   `json:"turnCount"`
	Messages    []m365ResponseMessage `json:"messages"`
	Error       *m365GraphError       `json:"error,omitempty"`
}

type m365ResponseMessage struct {
	ODataType    string            `json:"@odata.type"`
	ID           string            `json:"id"`
	Text         string            `json:"text"`
	Attributions []m365Attribution `json:"attributions,omitempty"`
}

type m365Attribution struct {
	AttributionType     string `json:"attributionType"`
	ProviderDisplayName string `json:"providerDisplayName"`
	AttributionSource   string `json:"attributionSource"`
	SeeMoreWebURL       string `json:"seeMoreWebUrl"`
}

type m365GraphError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// m365APIError is a failed Copilot API call.
type m365APIError struct {
	StatusCode int
	Status     string
	Code       string
	Message    string
	RetryAfter time.Duration
	Operation  string
}

func (e *m365APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Microsoft 365 Copilot %s failed", e.Operation)
	if e.Status != "" {
		fmt.Fprintf(&b, " (%s)", e.Status)
	}
	if e.Code != "" || e.Message != "" {
		b.WriteString(": ")
		b.WriteString(strings.TrimSpace(strings.Trim(e.Code+": "+e.Message, ": ")))
	}
	switch e.StatusCode {
	case http.StatusUnauthorized:
		b.WriteString(". Sign in again with `opencode m365 login`.")
	case http.StatusForbidden:
		b.WriteString(". The Chat API needs a Microsoft 365 Copilot license and consent to its Graph permissions " +
			"(Sites.Read.All, Mail.Read, People.Read.All, OnlineMeetingTranscript.Read.All, Chat.Read, " +
			"ChannelMessage.Read.All, ExternalItem.Read.All); some of them need an administrator to consent.")
	case http.StatusRequestEntityTooLarge:
		b.WriteString(". Try a lower m365copilot.maxMessageChars.")
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// Turn planning

// m365Turn is one Chat API call: which conversation to post to and what to send.
type m365Turn struct {
	sessionID      string
	conversationID string // empty: create a conversation first
	reused         bool   // continues an existing conversation
	request        m365ChatRequest
	// synced is the list of message IDs the conversation holds once this turn succeeds.
	// nil means the conversation can't be continued later.
	synced []string
	// contextChars approximates the whole conversation's size, for token estimates.
	contextChars int
}

func m365MessageIDs(messages []message.Message) []string {
	ids := make([]string, len(messages))
	for i, msg := range messages {
		if msg.ID == "" {
			return nil
		}
		ids[i] = msg.ID
	}
	return ids
}

// continuation returns the messages that are new since the conversation was last
// synced, or false if the history no longer matches what Copilot has seen.
func (s *m365ConversationState) continuation(messages []message.Message) ([]message.Message, bool) {
	n := len(s.synced)
	if len(messages) <= n {
		return nil, false
	}
	for i, id := range s.synced {
		if messages[i].ID != id {
			return nil, false
		}
	}
	rest := messages[n:]
	// The first new message is normally Copilot's own reply, which it already has.
	if rest[0].Role == message.Assistant {
		switch rest[0].FinishReason() {
		case message.FinishReasonCanceled, message.FinishReasonError:
			// The reply was cut short, so Copilot's copy may not match ours.
			return nil, false
		}
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return nil, false
	}
	for _, msg := range rest {
		if msg.Role == message.Assistant {
			return nil, false
		}
	}
	return rest, true
}

func (c *m365CopilotClient) planTurn(ctx context.Context, messages []message.Message, tools []toolsPkg.BaseTool, allowReuse bool) m365Turn {
	sessionID, _ := ctx.Value(toolsPkg.SessionIDContextKey).(string)
	ids := m365MessageIDs(messages)
	turn := m365Turn{sessionID: sessionID}
	if sessionID != "" && ids != nil {
		turn.synced = ids
	}

	toolNames := m365ToolCallNames(messages)
	if allowReuse && turn.synced != nil {
		c.mu.Lock()
		state := c.conversations[sessionID]
		c.mu.Unlock()
		if state != nil {
			if rest, ok := state.continuation(messages); ok {
				turn.conversationID = state.id
				turn.reused = true
				turn.request = c.buildContinuationRequest(rest, tools, toolNames)
				turn.contextChars = c.estimateContextChars(messages, tools)
				return turn
			}
		}
	}
	turn.request = c.buildFreshRequest(messages, tools, toolNames)
	turn.contextChars = c.estimateContextChars(messages, tools)
	return turn
}

func (c *m365CopilotClient) remember(turn m365Turn) {
	if turn.sessionID == "" || turn.synced == nil || turn.conversationID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.conversations[turn.sessionID] = &m365ConversationState{id: turn.conversationID, synced: turn.synced}
}

func (c *m365CopilotClient) forget(sessionID string) {
	if sessionID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.conversations, sessionID)
}

// m365ToolCallNames maps tool call IDs to tool names, to label tool results.
func m365ToolCallNames(messages []message.Message) map[string]string {
	names := map[string]string{}
	for _, msg := range messages {
		for _, call := range msg.ToolCalls() {
			names[call.ID] = call.Name
		}
	}
	return names
}

// m365Part is a piece of the chat message. Movable parts are moved to
// additionalContext when the message would be too long.
type m365Part struct {
	text        string
	moveRank    int    // 0: never moved; lower ranks are moved first
	description string // additionalContext description if moved
	placeholder string // what stays in the message if moved
}

// m365Assemble joins parts into the message text, moving parts to additional
// context until the text fits in max characters.
func m365Assemble(parts []m365Part, max int, extra []m365ContextMessage) (string, []m365ContextMessage) {
	const sep = "\n\n"
	moved := make([]bool, len(parts))
	length := func() int {
		n := 0
		for i, p := range parts {
			t := p.text
			if moved[i] {
				t = p.placeholder
			}
			if t == "" {
				continue
			}
			if n > 0 {
				n += len(sep)
			}
			n += len(t)
		}
		return n
	}
	order := make([]int, 0, len(parts))
	for i, p := range parts {
		if p.moveRank > 0 {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		pa, pb := parts[order[a]], parts[order[b]]
		if pa.moveRank != pb.moveRank {
			return pa.moveRank < pb.moveRank
		}
		return len(pa.text) > len(pb.text)
	})
	for _, i := range order {
		if length() <= max {
			break
		}
		if len(parts[i].text) > len(parts[i].placeholder) {
			moved[i] = true
		}
	}

	var text []string
	var context []m365ContextMessage
	for i, p := range parts {
		t := p.text
		if moved[i] {
			t = p.placeholder
			context = append(context, m365Chunk(p.text, p.description, max)...)
		}
		if t != "" {
			text = append(text, t)
		}
	}
	context = append(context, extra...)
	if len(context) > m365MaxContextEntries {
		logging.Warn("Microsoft 365 Copilot: dropping additional context entries", "entries", len(context), "max", m365MaxContextEntries)
		context = context[:m365MaxContextEntries]
	}
	return strings.Join(text, sep), context
}

// m365Chunk splits text into additionalContext entries of at most max characters.
func m365Chunk(text, description string, max int) []m365ContextMessage {
	if max <= 0 || len(text) <= max {
		return []m365ContextMessage{{Text: text, Description: description}}
	}
	var chunks []string
	for len(text) > max {
		cut := strings.LastIndexByte(text[:max], '\n')
		if cut < max/2 {
			cut = max
		}
		chunks = append(chunks, strings.ToValidUTF8(text[:cut], ""))
		text = text[cut:]
	}
	if text != "" {
		chunks = append(chunks, strings.ToValidUTF8(text, ""))
	}
	out := make([]m365ContextMessage, len(chunks))
	for i, chunk := range chunks {
		out[i] = m365ContextMessage{Text: chunk, Description: fmt.Sprintf("%s (part %d of %d)", description, i+1, len(chunks))}
	}
	return out
}

// buildFreshRequest starts a new Copilot conversation: it sends the
// instructions, the tool catalog, a transcript of any earlier messages and the
// latest input.
func (c *m365CopilotClient) buildFreshRequest(messages []message.Message, tools []toolsPkg.BaseTool, toolNames map[string]string) m365ChatRequest {
	max := c.options.maxMessageChars
	// The latest input is everything after the last assistant message.
	split := len(messages)
	for split > 0 && messages[split-1].Role != message.Assistant {
		split--
	}
	earlier, current := messages[:split], messages[split:]
	if len(current) == 0 {
		// History ends with an assistant message, e.g. a resumed turn: ask Copilot to carry on.
		earlier, current = messages, nil
	}

	var parts []m365Part
	if system := strings.TrimSpace(c.providerOptions.systemMessage); system != "" {
		parts = append(parts, m365Part{text: "# Instructions\n\n" + system})
	}
	if len(tools) > 0 {
		parts = append(parts, m365Part{
			text:        m365ToolInstructions(tools),
			moveRank:    3,
			description: "Local tool catalog",
			placeholder: fmt.Sprintf(m365ToolProtocol, m365ExampleToolCall(tools)) +
				"\n\nThe tools and their arguments are listed in the additional context \"Local tool catalog\".",
		})
	}
	if len(earlier) > 0 {
		transcript := m365Transcript(earlier, toolNames, max*4)
		parts = append(parts, m365Part{
			text:        "# Conversation so far\n\n" + transcript,
			moveRank:    1,
			description: "Transcript of the earlier conversation",
			placeholder: "# Conversation so far\n\nThe earlier conversation is attached as additional context \"Transcript of the earlier conversation\".",
		})
	}
	if len(current) == 0 {
		parts = append(parts, m365Part{text: "# Current request\n\nContinue with the task."})
	} else {
		parts = append(parts, m365Part{text: "# Current request"})
		parts = append(parts, c.inputParts(current, toolNames)...)
	}
	if len(tools) > 0 {
		parts = append(parts, m365Part{text: fmt.Sprintf(m365ToolReminder, m365ToolNames(tools))})
	}

	var extra []m365ContextMessage
	if len(tools) > 0 {
		extra = m365Chunk(m365ToolDocs(tools), "Reference documentation for OpenCode's local tools", max)
	}
	text, context := m365Assemble(parts, max, extra)
	return c.newRequest(text, context)
}

// buildContinuationRequest sends only the new messages to an existing conversation.
func (c *m365CopilotClient) buildContinuationRequest(newMessages []message.Message, tools []toolsPkg.BaseTool, toolNames map[string]string) m365ChatRequest {
	parts := c.inputParts(newMessages, toolNames)
	if len(tools) > 0 {
		parts = append(parts, m365Part{text: fmt.Sprintf(m365ToolReminder, m365ToolNames(tools))})
	}
	text, context := m365Assemble(parts, c.options.maxMessageChars, nil)
	return c.newRequest(text, context)
}

func (c *m365CopilotClient) newRequest(text string, context []m365ContextMessage) m365ChatRequest {
	req := m365ChatRequest{
		Message:           m365MessageParam{Text: text},
		AdditionalContext: context,
		LocationHint:      m365LocationHint{TimeZone: c.options.timeZone},
	}
	if !c.options.webSearch {
		// Web search grounding is on by default and has to be turned off on every turn.
		req.ContextualResources = &m365ContextResources{WebContext: &m365WebContext{IsWebEnabled: false}}
	}
	return req
}

// inputParts renders user messages and tool results that Copilot hasn't seen yet.
func (c *m365CopilotClient) inputParts(messages []message.Message, toolNames map[string]string) []m365Part {
	max := c.options.maxMessageChars
	var parts []m365Part
	for _, msg := range messages {
		switch msg.Role {
		case message.User:
			text := strings.TrimSpace(msg.Content().String())
			if n := len(msg.BinaryContent()) + len(msg.ImageURLContent()); n > 0 {
				text += fmt.Sprintf("\n\n(The user attached %d image(s), which Microsoft 365 Copilot can't receive through this API.)", n)
			}
			parts = append(parts, m365Part{
				text:        text,
				moveRank:    4,
				description: "The user's message",
				placeholder: "The user's message is attached as additional context \"The user's message\".",
			})
		case message.Tool:
			results := msg.ToolResults()
			if len(results) == 0 {
				continue
			}
			parts = append(parts, m365Part{text: "OpenCode ran the tools you requested. Results:"})
			for _, result := range results {
				name := toolNames[result.ToolCallID]
				if name == "" {
					name = result.Name
				}
				header := m365ToolResultHeader(result, name)
				content := m365TruncateMiddle(result.Content, max*3)
				if strings.TrimSpace(content) == "" {
					content = "(no output)"
				}
				description := fmt.Sprintf("Output of tool call %s (%s)", result.ToolCallID, name)
				parts = append(parts, m365Part{
					text:        header + "\n" + content + "\n</tool_result>",
					moveRank:    2,
					description: description,
					placeholder: fmt.Sprintf("%s\n(The output is attached as additional context %q.)\n</tool_result>", header, description),
				})
			}
			parts = append(parts, m365Part{text: "Continue with the task: call more tools if you need to, otherwise reply with your answer."})
		case message.Assistant:
			// Only reached for fresh conversations through the transcript.
		}
	}
	return parts
}

// m365Transcript renders earlier messages as text, keeping the most recent
// limit characters if it is too long.
func m365Transcript(messages []message.Message, toolNames map[string]string, limit int) string {
	var b strings.Builder
	for _, msg := range messages {
		switch msg.Role {
		case message.User:
			fmt.Fprintf(&b, "## User\n%s\n\n", strings.TrimSpace(msg.Content().String()))
		case message.Assistant:
			b.WriteString("## Assistant (you)\n")
			if text := strings.TrimSpace(msg.Content().String()); text != "" {
				b.WriteString(text)
				b.WriteString("\n")
			}
			for _, call := range msg.ToolCalls() {
				b.WriteString(m365FormatToolCall(call))
				b.WriteString("\n")
			}
			b.WriteString("\n")
		case message.Tool:
			b.WriteString("## Tool results\n")
			for _, result := range msg.ToolResults() {
				name := toolNames[result.ToolCallID]
				fmt.Fprintf(&b, "%s\n%s\n</tool_result>\n", m365ToolResultHeader(result, name), m365TruncateMiddle(result.Content, 4000))
			}
			b.WriteString("\n")
		}
	}
	s := strings.TrimSpace(b.String())
	if limit > 0 && len(s) > limit {
		s = "[... earlier messages omitted ...]\n\n" + strings.ToValidUTF8(s[len(s)-limit:], "")
	}
	return s
}

func (c *m365CopilotClient) estimateContextChars(messages []message.Message, tools []toolsPkg.BaseTool) int {
	n := len(c.providerOptions.systemMessage) + len(m365ToolInstructions(tools))
	for _, msg := range messages {
		n += len(msg.Content().String())
		for _, call := range msg.ToolCalls() {
			n += len(call.Name) + len(call.Input) + 40
		}
		for _, result := range msg.ToolResults() {
			n += len(result.Content) + 60
		}
	}
	return n
}

// m365EstimateTokens approximates a token count from characters. The Chat API
// doesn't report usage; this keeps opencode's context meter and auto-compact working.
func m365EstimateTokens(chars int) int64 {
	return int64((chars + 3) / 4)
}

// ---------------------------------------------------------------------------
// Running a turn

type m365TurnResult struct {
	text         string
	attributions []m365Attribution
}

func (c *m365CopilotClient) send(ctx context.Context, messages []message.Message, tools []toolsPkg.BaseTool) (*ProviderResponse, error) {
	result, err := c.converse(ctx, messages, tools, false, nil, func() bool { return true })
	if err != nil {
		return nil, err
	}
	return c.response(result, m365VisibleText(result.text, true), messages, tools), nil
}

func (c *m365CopilotClient) stream(ctx context.Context, messages []message.Message, tools []toolsPkg.BaseTool) <-chan ProviderEvent {
	events := make(chan ProviderEvent)
	go func() {
		defer close(events)
		emit := func(event ProviderEvent) bool {
			select {
			case events <- event:
				return true
			case <-ctx.Done():
				return false
			}
		}

		emitted := ""
		showUpTo := func(visible string) {
			if len(visible) > len(emitted) && strings.HasPrefix(visible, emitted) {
				delta := visible[len(emitted):]
				emitted = visible
				emit(ProviderEvent{Type: EventContentDelta, Content: delta})
			}
		}
		onText := func(full string) { showUpTo(m365VisibleText(full, false)) }
		// Only retry while nothing has been shown, so retries can't duplicate text.
		canRetry := func() bool { return emitted == "" }

		result, err := c.converse(ctx, messages, tools, true, onText, canRetry)
		if err != nil {
			emit(ProviderEvent{Type: EventError, Error: err})
			return
		}
		final := m365VisibleText(result.text, true)
		if !strings.HasPrefix(final, emitted) {
			logging.Debug("Microsoft 365 Copilot rewrote streamed text; keeping what was shown")
		}
		showUpTo(final)
		emit(ProviderEvent{Type: EventComplete, Response: c.response(result, emitted, messages, tools)})
	}()
	return events
}

func (c *m365CopilotClient) response(result *m365TurnResult, content string, messages []message.Message, tools []toolsPkg.BaseTool) *ProviderResponse {
	toolCalls := m365ParseToolCalls(result.text, tools)
	finish := message.FinishReasonEndTurn
	if len(toolCalls) > 0 {
		finish = message.FinishReasonToolUse
	}
	if content == "" && len(toolCalls) == 0 && strings.TrimSpace(result.text) == "" {
		content = "(Microsoft 365 Copilot returned an empty response.)"
	}
	return &ProviderResponse{
		Content:   content,
		ToolCalls: toolCalls,
		Usage: TokenUsage{
			InputTokens:  m365EstimateTokens(c.estimateContextChars(messages, tools)),
			OutputTokens: m365EstimateTokens(len(result.text)),
		},
		FinishReason: finish,
	}
}

// converse plans and runs a turn, retrying on throttling and transient errors
// and starting a new conversation if the existing one can't be continued.
func (c *m365CopilotClient) converse(ctx context.Context, messages []message.Message, tools []toolsPkg.BaseTool, streaming bool, onText func(string), canRetry func() bool) (*m365TurnResult, error) {
	turn := c.planTurn(ctx, messages, tools, true)
	c.debugRequest(ctx, messages, turn)
	refreshedToken := false
	for attempt := 1; ; attempt++ {
		result, err := c.execute(ctx, &turn, streaming, onText)
		if err == nil {
			c.remember(turn)
			c.debugResponse(ctx, messages, result)
			return result, nil
		}
		c.forget(turn.sessionID)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var apiErr *m365APIError
		if !errors.As(err, &apiErr) || !canRetry() {
			return nil, err
		}

		var wait time.Duration
		switch {
		case apiErr.StatusCode == http.StatusUnauthorized && !refreshedToken:
			refreshedToken = true
			c.options.tokenSource.Invalidate()
		case turn.reused && apiErr.Operation == "chat" &&
			(apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusBadRequest || apiErr.StatusCode == http.StatusConflict):
			// The conversation expired or reached its limit: replay the history into a new one.
			logging.Info("Microsoft 365 Copilot conversation can't be continued, starting a new one", "error", err)
			turn = c.planTurn(ctx, messages, tools, false)
		case apiErr.StatusCode == http.StatusTooManyRequests || apiErr.StatusCode >= 500:
			if attempt > maxRetries {
				return nil, err
			}
			wait = apiErr.RetryAfter
			if wait <= 0 {
				wait = time.Duration(2000*(1<<(attempt-1))) * time.Millisecond
			}
			if wait > 60*time.Second {
				wait = 60 * time.Second
			}
			logging.WarnPersist(fmt.Sprintf("Microsoft 365 Copilot is busy (%s), retrying... attempt %d of %d", apiErr.Status, attempt, maxRetries),
				logging.PersistTimeArg, wait+100*time.Millisecond)
		default:
			return nil, err
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
		}
	}
}

// execute creates the conversation if needed and sends the turn's message.
func (c *m365CopilotClient) execute(ctx context.Context, turn *m365Turn, streaming bool, onText func(string)) (*m365TurnResult, error) {
	if turn.conversationID == "" {
		id, err := c.createConversation(ctx)
		if err != nil {
			return nil, err
		}
		turn.conversationID = id
	}
	endpoint := "chat"
	if streaming {
		endpoint = "chatOverStream"
	}
	body, err := json.Marshal(turn.request)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, "chat", fmt.Sprintf("/conversations/%s/%s", turn.conversationID, endpoint), body, streaming)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	prompt := turn.request.Message.Text
	if !streaming {
		var conv m365ConversationResponse
		if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
			return nil, fmt.Errorf("invalid Microsoft 365 Copilot response: %w", err)
		}
		text, attributions := m365ResponseText(conv, prompt, nil)
		return &m365TurnResult{text: text, attributions: attributions}, nil
	}

	result := &m365TurnResult{}
	seen := map[string]bool{}
	err = m365ReadSSE(resp.Body, func(data []byte) error {
		var conv m365ConversationResponse
		if err := json.Unmarshal(data, &conv); err != nil {
			logging.Debug("Microsoft 365 Copilot: skipping unparsable stream event", "error", err)
			return nil
		}
		if conv.Error != nil {
			return &m365APIError{Operation: "chat", Code: conv.Error.Code, Message: conv.Error.Message}
		}
		text, attributions := m365ResponseText(conv, prompt, seen)
		for _, msg := range conv.Messages {
			seen[msg.ID] = true
		}
		if text == "" || text == result.text {
			return nil
		}
		result.text = text
		result.attributions = attributions
		if onText != nil {
			onText(text)
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return result, nil
}

func (c *m365CopilotClient) createConversation(ctx context.Context) (string, error) {
	createCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	resp, err := c.do(createCtx, "create conversation", "/conversations", []byte("{}"), false)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var conv m365ConversationResponse
	if err := json.NewDecoder(resp.Body).Decode(&conv); err != nil {
		return "", fmt.Errorf("invalid Microsoft 365 Copilot conversation: %w", err)
	}
	if conv.ID == "" {
		return "", errors.New("Microsoft 365 Copilot created a conversation without an id")
	}
	logging.Debug("Created Microsoft 365 Copilot conversation", "id", conv.ID)
	return conv.ID, nil
}

// do sends an authenticated POST and returns the response if it succeeded.
func (c *m365CopilotClient) do(ctx context.Context, operation, path string, body []byte, streaming bool) (*http.Response, error) {
	token, err := c.options.tokenSource.Token(ctx)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.options.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	resp, err := c.options.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	apiErr := &m365APIError{StatusCode: resp.StatusCode, Status: resp.Status, Operation: operation}
	var graphErr struct {
		Error m365GraphError `json:"error"`
	}
	if json.Unmarshal(data, &graphErr) == nil && (graphErr.Error.Code != "" || graphErr.Error.Message != "") {
		apiErr.Code, apiErr.Message = graphErr.Error.Code, graphErr.Error.Message
	} else {
		apiErr.Message = strings.TrimSpace(string(data))
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			apiErr.RetryAfter = time.Duration(secs) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			apiErr.RetryAfter = time.Until(at)
		}
	}
	return nil, apiErr
}

// m365ResponseText extracts Copilot's reply from a conversation snapshot,
// leaving out the echo of the message that was sent. seen holds the IDs of
// messages from earlier stream events, which are all part of the reply.
func m365ResponseText(conv m365ConversationResponse, prompt string, seen map[string]bool) (string, []m365Attribution) {
	msgs := conv.Messages
	start := 0
	normalizedPrompt := strings.Join(strings.Fields(prompt), " ")
	for i, msg := range msgs {
		if normalizedPrompt != "" && strings.Join(strings.Fields(msg.Text), " ") == normalizedPrompt {
			start = i + 1
		}
	}
	// The complete conversation lists the user's message first. Partial
	// snapshots only hold the reply.
	if start == 0 && len(msgs) >= 2 && conv.DisplayName != m365IntermediateUpdate && !seen[msgs[0].ID] {
		start = 1
	}
	var texts []string
	var attributions []m365Attribution
	for _, msg := range msgs[start:] {
		if strings.TrimSpace(msg.Text) == "" {
			continue
		}
		texts = append(texts, msg.Text)
		attributions = append(attributions, msg.Attributions...)
	}
	return strings.Join(texts, "\n\n"), attributions
}

// m365ReadSSE calls onData with the data of each server-sent event. Besides
// standard SSE it accepts JSON that is pretty-printed over several lines after
// "data:", as shown in the API documentation.
func m365ReadSSE(r io.Reader, onData func([]byte) error) error {
	reader := bufio.NewReaderSize(r, 64*1024)
	var data bytes.Buffer
	hasData := false
	flush := func() error {
		if !hasData {
			return nil
		}
		payload := bytes.TrimSpace(data.Bytes())
		data.Reset()
		hasData = false
		if len(payload) == 0 || string(payload) == "[DONE]" {
			return nil
		}
		return onData(payload)
	}
	for {
		line, readErr := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			switch {
			case line == "":
				if err := flush(); err != nil {
					return err
				}
			case strings.HasPrefix(line, "data:"):
				value := strings.TrimPrefix(line[len("data:"):], " ")
				// A new event starting without a blank line in between.
				if hasData && json.Valid(bytes.TrimSpace(data.Bytes())) {
					if err := flush(); err != nil {
						return err
					}
				}
				if hasData {
					data.WriteByte('\n')
				}
				data.WriteString(value)
				hasData = true
			case strings.HasPrefix(line, ":"):
				// Comment or keep-alive.
			case strings.HasPrefix(line, "id:"), strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "retry:"):
			default:
				if hasData {
					data.WriteByte('\n')
					data.WriteString(line)
				}
			}
		}
		if readErr == io.EOF {
			return flush()
		}
		if readErr != nil {
			return readErr
		}
	}
}

// ---------------------------------------------------------------------------
// Debug logging

func (c *m365CopilotClient) debugRequest(ctx context.Context, messages []message.Message, turn m365Turn) {
	cfg := config.Get()
	if cfg == nil || !cfg.Debug {
		return
	}
	sessionID, _ := ctx.Value(toolsPkg.SessionIDContextKey).(string)
	logging.Debug("Microsoft 365 Copilot request", "conversation", turn.conversationID, "reused", turn.reused,
		"message_chars", len(turn.request.Message.Text), "context_entries", len(turn.request.AdditionalContext))
	if sessionID != "" {
		if path := logging.WriteRequestMessageJson(sessionID, (len(messages)+1)/2, turn.request); path != "" {
			logging.Debug("Prepared messages", "filepath", filepath.Clean(path))
		}
	}
}

func (c *m365CopilotClient) debugResponse(ctx context.Context, messages []message.Message, result *m365TurnResult) {
	cfg := config.Get()
	if cfg == nil || !cfg.Debug {
		return
	}
	logging.Debug("Microsoft 365 Copilot reply", "text", m365TruncateMiddle(result.text, 4000))
	sessionID, _ := ctx.Value(toolsPkg.SessionIDContextKey).(string)
	if sessionID != "" {
		logging.WriteChatResponseJson(sessionID, (len(messages)+1)/2, map[string]any{
			"text":         result.text,
			"attributions": result.attributions,
		})
	}
}
