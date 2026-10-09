package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opencode-ai/opencode/internal/llm/tools"
	"github.com/opencode-ai/opencode/internal/message"
)

// mockGraph imitates the Copilot Chat API endpoints in Microsoft Graph.
type mockGraph struct {
	t *testing.T

	mu            sync.Mutex
	conversations map[string]bool
	created       int
	requests      []mockRequest
	replies       []string // reply texts, one per chat call
	failures      []mockFailure
}

type mockRequest struct {
	path          string
	authorization string
	accept        string
	body          m365ChatRequest
	raw           map[string]any
}

type mockFailure struct {
	status     int
	retryAfter string
	body       string
}

func newMockGraph(t *testing.T, replies ...string) (*mockGraph, *httptest.Server) {
	m := &mockGraph{t: t, conversations: map[string]bool{}, replies: replies}
	server := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(server.Close)
	return m, server
}

func (m *mockGraph) serve(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.Path == "/conversations" {
		m.created++
		id := fmt.Sprintf("conv-%d", m.created)
		m.conversations[id] = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":%q,"createdDateTime":"2025-09-30T15:28:46Z","displayName":"","status":"active","turnCount":0}`, id)
		return
	}

	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "conversations" {
		http.NotFound(w, r)
		return
	}
	var body m365ChatRequest
	var raw map[string]any
	data := new(strings.Builder)
	if _, err := fmt.Fprint(data, readAll(r)); err != nil {
		m.t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(data.String()), &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	_ = json.Unmarshal([]byte(data.String()), &raw)
	m.requests = append(m.requests, mockRequest{
		path:          r.URL.Path,
		authorization: r.Header.Get("Authorization"),
		accept:        r.Header.Get("Accept"),
		body:          body,
		raw:           raw,
	})

	if len(m.failures) > 0 {
		f := m.failures[0]
		m.failures = m.failures[1:]
		if f.retryAfter != "" {
			w.Header().Set("Retry-After", f.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.status)
		fmt.Fprint(w, f.body)
		return
	}
	if !m.conversations[parts[1]] {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"error":{"code":"NotFound","message":"Conversation not found."}}`)
		return
	}
	if body.LocationHint.TimeZone == "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"code":"BadRequest","message":"locationHint is required"}}`)
		return
	}
	if len(m.replies) == 0 {
		m.t.Errorf("unexpected chat request %s", r.URL.Path)
		http.Error(w, "no reply queued", http.StatusInternalServerError)
		return
	}
	reply := m.replies[0]
	m.replies = m.replies[1:]

	final := m365ConversationResponse{
		ID:          parts[1],
		DisplayName: "first prompt",
		State:       "active",
		TurnCount:   len(m.requests),
		Messages: []m365ResponseMessage{
			{ODataType: "#microsoft.graph.copilotConversationResponseMessage", ID: "user-msg", Text: body.Message.Text},
			{ODataType: "#microsoft.graph.copilotConversationResponseMessage", ID: "reply-msg", Text: reply},
		},
	}
	switch parts[2] {
	case "chat":
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(final)
	case "chatOverStream":
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// Cumulative snapshots of the reply, like the real stream.
		for i, cut := range []int{len(reply) / 3, 2 * len(reply) / 3, len(reply)} {
			snapshot := m365ConversationResponse{
				ID:          parts[1],
				DisplayName: m365IntermediateUpdate,
				State:       "active",
				Messages: []m365ResponseMessage{
					{ODataType: "#microsoft.graph.copilotConversationResponseMessage", ID: "reply-msg", Text: reply[:cut]},
				},
			}
			encoded, _ := json.Marshal(snapshot)
			fmt.Fprintf(w, "data: %s\nid:%d\n\n", encoded, 100+i)
			flusher.Flush()
		}
		encoded, _ := json.Marshal(final)
		fmt.Fprintf(w, "data: %s\nid:200\n\n", encoded)
	default:
		http.NotFound(w, r)
	}
}

func readAll(r *http.Request) string {
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			return b.String()
		}
	}
}

func (m *mockGraph) snapshot() (int, []mockRequest) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.created, append([]mockRequest(nil), m.requests...)
}

type fakeTokens struct{ invalidated atomic.Int32 }

func (f *fakeTokens) Token(context.Context) (string, error) { return "test-token", nil }
func (f *fakeTokens) Invalidate()                           { f.invalidated.Add(1) }

func newTestM365Client(server *httptest.Server, tokens *fakeTokens, extra ...M365CopilotOption) *m365CopilotClient {
	opts := append([]M365CopilotOption{
		WithM365CopilotBaseURL(server.URL),
		WithM365CopilotTimeZone("Europe/Paris"),
		WithM365CopilotTokenSource(tokens),
	}, extra...)
	return newM365CopilotClient(providerClientOptions{
		systemMessage:      "You are OpenCode, a test assistant.",
		m365CopilotOptions: opts,
	}).(*m365CopilotClient)
}

func userMessage(id, text string) message.Message {
	return message.Message{ID: id, Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}}
}

func assistantMessage(id, text string, calls []message.ToolCall, reason message.FinishReason) message.Message {
	parts := []message.ContentPart{message.TextContent{Text: text}}
	for _, c := range calls {
		parts = append(parts, c)
	}
	parts = append(parts, message.Finish{Reason: reason, Time: time.Now().Unix()})
	return message.Message{ID: id, Role: message.Assistant, Parts: parts}
}

func toolMessage(id string, results ...message.ToolResult) message.Message {
	parts := make([]message.ContentPart, len(results))
	for i, r := range results {
		parts[i] = r
	}
	return message.Message{ID: id, Role: message.Tool, Parts: parts}
}

func collect(t *testing.T, events <-chan ProviderEvent) (string, *ProviderResponse) {
	t.Helper()
	var content strings.Builder
	var final *ProviderResponse
	for event := range events {
		switch event.Type {
		case EventContentDelta:
			content.WriteString(event.Content)
		case EventComplete:
			final = event.Response
		case EventError:
			t.Fatalf("stream error: %v", event.Error)
		}
	}
	if final == nil {
		t.Fatal("stream ended without a complete event")
	}
	return content.String(), final
}

func sessionContext(id string) context.Context {
	return context.WithValue(context.Background(), tools.SessionIDContextKey, id)
}

func TestM365StreamToolCallThenContinueConversation(t *testing.T) {
	graph, server := newMockGraph(t,
		"Let me look at the project.\n\n```tool_call\n{\"name\": \"ls\", \"arguments\": {\"path\": \"/repo\"}}\n```",
		"The project has one file: <File>main.go</File>[^1^].",
	)
	client := newTestM365Client(server, &fakeTokens{})
	ctx := sessionContext("session-1")
	ts := testTools()

	history := []message.Message{userMessage("u1", "What files are in this project?")}
	content, resp := collect(t, client.stream(ctx, history, ts))

	if content != "Let me look at the project." || resp.Content != content {
		t.Fatalf("streamed %q, response content %q", content, resp.Content)
	}
	if resp.FinishReason != message.FinishReasonToolUse || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "ls" {
		t.Fatalf("unexpected response %+v", resp)
	}
	if args := argsOf(t, resp.ToolCalls[0].Input); args["path"] != "/repo" {
		t.Fatalf("unexpected tool input %s", resp.ToolCalls[0].Input)
	}
	if resp.Usage.InputTokens == 0 || resp.Usage.OutputTokens == 0 {
		t.Errorf("expected estimated usage, got %+v", resp.Usage)
	}

	created, requests := graph.snapshot()
	if created != 1 || len(requests) != 1 {
		t.Fatalf("created %d conversations and sent %d chats", created, len(requests))
	}
	first := requests[0]
	if first.path != "/conversations/conv-1/chatOverStream" || first.accept != "text/event-stream" {
		t.Fatalf("unexpected request %s (Accept %s)", first.path, first.accept)
	}
	if first.authorization != "Bearer test-token" {
		t.Fatalf("unexpected Authorization %q", first.authorization)
	}
	if first.body.LocationHint.TimeZone != "Europe/Paris" {
		t.Fatalf("unexpected location hint %+v", first.body.LocationHint)
	}
	text := first.body.Message.Text
	for _, want := range []string{"I'm working on a code project on my computer with OpenCode", "```tool_call", "## Available actions", "### ls",
		"You are OpenCode, a test assistant.", "# My request\n\nWhat files are in this project?"} {
		if !strings.Contains(text, want) {
			t.Errorf("first message is missing %q", want)
		}
	}
	// The order that worked against the real service: protocol, instructions, request.
	if !(strings.Index(text, "## Available actions") < strings.Index(text, "You are OpenCode") &&
		strings.Index(text, "You are OpenCode") < strings.Index(text, "# My request")) {
		t.Errorf("first message parts are out of order:\n%s", text)
	}
	if len(first.body.AdditionalContext) != 0 {
		t.Errorf("tool docs in additional context made Copilot refuse the tools; got %+v", first.body.AdditionalContext)
	}
	if _, ok := first.raw["contextualResources"]; ok {
		t.Errorf("web search is on by default, contextualResources should be omitted")
	}

	// opencode ran the tool; the next request continues the same conversation.
	call := resp.ToolCalls[0]
	history = append(history,
		assistantMessage("a1", content, resp.ToolCalls, message.FinishReasonToolUse),
		toolMessage("t1", message.ToolResult{ToolCallID: call.ID, Content: "main.go"}),
	)
	content, resp = collect(t, client.stream(ctx, history, ts))
	if content != "The project has one file: main.go." {
		t.Fatalf("second reply streamed as %q", content)
	}
	if resp.FinishReason != message.FinishReasonEndTurn || len(resp.ToolCalls) != 0 {
		t.Fatalf("unexpected second response %+v", resp)
	}

	created, requests = graph.snapshot()
	if created != 1 {
		t.Fatalf("expected the conversation to be reused, created %d", created)
	}
	second := requests[1]
	if second.path != "/conversations/conv-1/chatOverStream" {
		t.Fatalf("second turn went to %s", second.path)
	}
	wantResult := fmt.Sprintf("<tool_result id=%q name=\"ls\" status=\"success\">\nmain.go\n</tool_result>", call.ID)
	if !strings.Contains(second.body.Message.Text, wantResult) {
		t.Errorf("second message doesn't hold the tool result:\n%s", second.body.Message.Text)
	}
	if strings.Contains(second.body.Message.Text, "## Available actions") || len(second.body.AdditionalContext) != 0 {
		t.Errorf("a continued conversation shouldn't resend the instructions:\n%s", second.body.Message.Text)
	}
	// Copilot forgets the request between turns, so follow-ups restate it.
	if !strings.Contains(second.body.Message.Text, `My request: "What files are in this project?"`) {
		t.Errorf("tool results should be followed by the request:\n%s", second.body.Message.Text)
	}

	// The user follows up: only the new user message is sent.
	graph.mu.Lock()
	graph.replies = append(graph.replies, "Sure.")
	graph.mu.Unlock()
	history = append(history,
		assistantMessage("a2", content, nil, message.FinishReasonEndTurn),
		userMessage("u2", "Thanks!"),
	)
	collect(t, client.stream(ctx, history, ts))
	_, requests = graph.snapshot()
	if third := requests[2].body.Message.Text; !strings.HasPrefix(third, "Thanks!") {
		t.Errorf("third message should start with the follow-up, got:\n%s", third)
	}
}

func TestM365StartsFreshConversationWhenHistoryDiverges(t *testing.T) {
	graph, server := newMockGraph(t, "First answer.", "Second answer.")
	client := newTestM365Client(server, &fakeTokens{})
	ctx := sessionContext("session-2")

	collect(t, client.stream(ctx, []message.Message{userMessage("u1", "Question one")}, nil))
	// The history was compacted into a summary, so it no longer starts with u1.
	history := []message.Message{
		userMessage("summary", "Summary of the earlier work."),
		assistantMessage("a-summary", "Understood.", nil, message.FinishReasonEndTurn),
		userMessage("u2", "Question two"),
	}
	content, _ := collect(t, client.stream(ctx, history, nil))
	if content != "Second answer." {
		t.Fatalf("got %q", content)
	}
	created, requests := graph.snapshot()
	if created != 2 {
		t.Fatalf("expected a new conversation, created %d", created)
	}
	text := requests[1].body.Message.Text
	for _, want := range []string{"# Conversation so far", "Summary of the earlier work.", "## Assistant (you)\nUnderstood.", "# My request\n\nQuestion two"} {
		if !strings.Contains(text, want) {
			t.Errorf("fresh conversation is missing %q:\n%s", want, text)
		}
	}
}

func TestM365CanceledReplyStartsFreshConversation(t *testing.T) {
	graph, server := newMockGraph(t, "Partial answer that was cut off.", "Recovered.")
	client := newTestM365Client(server, &fakeTokens{})
	ctx := sessionContext("session-3")

	collect(t, client.stream(ctx, []message.Message{userMessage("u1", "Hi")}, nil))
	history := []message.Message{
		userMessage("u1", "Hi"),
		assistantMessage("a1", "Partial", nil, message.FinishReasonCanceled),
		userMessage("u2", "Try again"),
	}
	collect(t, client.stream(ctx, history, nil))
	if created, _ := graph.snapshot(); created != 2 {
		t.Fatalf("a canceled reply should start a new conversation, created %d", created)
	}
}

func TestM365SendUsesSynchronousEndpoint(t *testing.T) {
	graph, server := newMockGraph(t, "Fix login redirect bug")
	client := newTestM365Client(server, &fakeTokens{})

	// Title generation: no session in the context and no message IDs.
	resp, err := client.send(context.Background(), []message.Message{{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "the login page redirects in a loop"}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "Fix login redirect bug" || resp.FinishReason != message.FinishReasonEndTurn {
		t.Fatalf("unexpected response %+v", resp)
	}
	_, requests := graph.snapshot()
	if requests[0].path != "/conversations/conv-1/chat" {
		t.Fatalf("expected the synchronous endpoint, got %s", requests[0].path)
	}
	if strings.Contains(requests[0].body.Message.Text, "tool_call") {
		t.Errorf("no tools were given, so the protocol shouldn't be described")
	}
	if len(client.conversations) != 0 {
		t.Errorf("turns without a session shouldn't be remembered")
	}
}

func TestM365RetriesAfterUnauthorizedAndThrottling(t *testing.T) {
	graph, server := newMockGraph(t, "Done.")
	graph.failures = []mockFailure{
		{status: http.StatusUnauthorized, body: `{"error":{"code":"InvalidAuthenticationToken","message":"Access token has expired."}}`},
		{status: http.StatusTooManyRequests, retryAfter: "1", body: `{"error":{"code":"TooManyRequests","message":"Slow down."}}`},
	}
	tokens := &fakeTokens{}
	client := newTestM365Client(server, tokens)

	start := time.Now()
	content, _ := collect(t, client.stream(sessionContext("s"), []message.Message{userMessage("u1", "Hi")}, nil))
	if content != "Done." {
		t.Fatalf("got %q", content)
	}
	if tokens.invalidated.Load() != 1 {
		t.Errorf("expected the token to be invalidated once after the 401, got %d", tokens.invalidated.Load())
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("expected to wait for Retry-After, took %s", elapsed)
	}
	if _, requests := graph.snapshot(); len(requests) != 3 {
		t.Errorf("expected 3 chat attempts, got %d", len(requests))
	}
}

func TestM365ForbiddenErrorExplainsRequirements(t *testing.T) {
	graph, server := newMockGraph(t)
	graph.failures = []mockFailure{{status: http.StatusForbidden, body: `{"error":{"code":"Forbidden","message":"User is not licensed."}}`}}
	client := newTestM365Client(server, &fakeTokens{})

	var err error
	for event := range client.stream(sessionContext("s"), []message.Message{userMessage("u1", "Hi")}, nil) {
		if event.Type == EventError {
			err = event.Error
		}
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"403", "User is not licensed.", "Microsoft 365 Copilot license"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

// Graph wraps a refusal from Copilot's backend in a 500, as seen on a tenant
// whose user has no Microsoft 365 license. It must not be retried.
func TestM365WrappedForbiddenIsNotRetried(t *testing.T) {
	graph, server := newMockGraph(t)
	graph.failures = []mockFailure{{status: http.StatusInternalServerError,
		body: `{"error":{"code":"internalServerError","message":"Got Non-2xx response from IC3. Status = 403 (Forbidden)"}}`}}
	client := newTestM365Client(server, &fakeTokens{})

	start := time.Now()
	var err error
	for event := range client.stream(sessionContext("s"), []message.Message{userMessage("u1", "Hi")}, nil) {
		if event.Type == EventError {
			err = event.Error
		}
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("a wrapped 403 shouldn't be retried, took %s", elapsed)
	}
	for _, want := range []string{"IC3. Status = 403", "Microsoft 365 Copilot license", "Exchange Online and Teams"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
	if _, requests := graph.snapshot(); len(requests) != 1 {
		t.Errorf("expected exactly one attempt, got %d", len(requests))
	}
}

// A real tenant whose Copilot license isn't active yet answers the chat call
// with 403 and a conversation explaining why, instead of a Graph error.
func TestM365ForbiddenConversationShowsCopilotReply(t *testing.T) {
	graph, server := newMockGraph(t)
	graph.failures = []mockFailure{{status: http.StatusForbidden,
		body: `{"id":"c1","state":"active","turnCount":1,"messages":[{"id":"u","text":"Hi"},{"id":"r","text":"It looks like you don\u2019t have a valid license. To get access, please check with your administrator."}]}`}}
	client := newTestM365Client(server, &fakeTokens{})
	var err error
	for event := range client.stream(sessionContext("s"), []message.Message{userMessage("u1", "Hi")}, nil) {
		if event.Type == EventError {
			err = event.Error
		}
	}
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "Copilot replied: It looks like you don’t have a valid license.") || strings.Contains(err.Error(), `"messages"`) {
		t.Errorf("error should show Copilot's reply, not raw JSON: %v", err)
	}
}

// Copilot sometimes declines to use the tools at the start of a request; it is
// asked once more in the same conversation.
func TestM365NudgesAfterRefusal(t *testing.T) {
	graph, server := newMockGraph(t,
		"I can't access the OpenCode-specific tools from this chat, so I can't read notes.txt.",
		"```tool_call\n{\"name\": \"view\", \"arguments\": {\"file_path\": \"/repo/notes.txt\"}}\n```",
	)
	client := newTestM365Client(server, &fakeTokens{})
	content, resp := collect(t, client.stream(sessionContext("s"), []message.Message{userMessage("u1", "Read notes.txt")}, testTools()))

	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "view" || resp.FinishReason != message.FinishReasonToolUse {
		t.Fatalf("expected the nudged reply's tool call, got %+v", resp)
	}
	if !strings.Contains(content, "I can't access") || !strings.Contains(content, m365NudgeNote) || resp.Content != content {
		t.Errorf("the refusal and the note should both be shown, got %q", content)
	}
	created, requests := graph.snapshot()
	if created != 1 || len(requests) != 2 || requests[1].path != requests[0].path {
		t.Fatalf("expected a second turn in the same conversation, got %d conversations and %d requests", created, len(requests))
	}
	if nudge := requests[1].body.Message.Text; !strings.Contains(nudge, "You don't need access to my computer") || !strings.Contains(nudge, `My request: "Read notes.txt"`) {
		t.Errorf("unexpected nudge:\n%s", nudge)
	}
}

func TestM365DoesNotNudgeAfterToolResults(t *testing.T) {
	graph, server := newMockGraph(t, "I can't access that URL, it returned 403.")
	client := newTestM365Client(server, &fakeTokens{})
	history := []message.Message{
		userMessage("u1", "Fetch the page"),
		assistantMessage("a1", "", []message.ToolCall{{ID: "c1", Name: "ls", Input: "{}"}}, message.FinishReasonToolUse),
		toolMessage("t1", message.ToolResult{ToolCallID: "c1", Content: "403", IsError: true}),
	}
	collect(t, client.stream(sessionContext("s"), history, testTools()))
	if _, requests := graph.snapshot(); len(requests) != 1 {
		t.Fatalf("a final answer after tool results must not be nudged, got %d requests", len(requests))
	}
}

func TestM365LooksLikeRefusal(t *testing.T) {
	for text, want := range map[string]bool{
		"I can’t access the OpenCode-specific tools or the project files described in your prompt.": true,
		"I can't interact with the OpenCode-specific `tool_call` interface from this chat.":         true,
		"I don't have access to the `/Users/demo/project` filesystem you described.":                true,
		"```tool_call\n{\"name\": \"ls\", \"arguments\": {}}\n```":                                  false,
		"The function returns early when the cache is empty.":                                       false,
	} {
		if got := m365LooksLikeRefusal(text); got != want {
			t.Errorf("m365LooksLikeRefusal(%q) = %v, want %v", text, got, want)
		}
	}
}

func TestM365ExpiredConversationIsReplayed(t *testing.T) {
	graph, server := newMockGraph(t, "One.", "Two.")
	client := newTestM365Client(server, &fakeTokens{})
	ctx := sessionContext("s")

	collect(t, client.stream(ctx, []message.Message{userMessage("u1", "First")}, nil))
	graph.mu.Lock()
	delete(graph.conversations, "conv-1") // the server forgot the conversation
	graph.mu.Unlock()

	history := []message.Message{
		userMessage("u1", "First"),
		assistantMessage("a1", "One.", nil, message.FinishReasonEndTurn),
		userMessage("u2", "Second"),
	}
	content, _ := collect(t, client.stream(ctx, history, nil))
	if content != "Two." {
		t.Fatalf("got %q", content)
	}
	created, requests := graph.snapshot()
	if created != 2 || requests[len(requests)-1].path != "/conversations/conv-2/chatOverStream" {
		t.Fatalf("expected a replay into conv-2, created %d, last request %s", created, requests[len(requests)-1].path)
	}
	if text := requests[len(requests)-1].body.Message.Text; !strings.Contains(text, "# Conversation so far") || !strings.Contains(text, "Second") {
		t.Errorf("replay should carry the transcript:\n%s", text)
	}
}

func TestM365LargeToolOutputMovesToAdditionalContext(t *testing.T) {
	graph, server := newMockGraph(t, "Reading.\n```tool_call\n{\"name\":\"view\",\"arguments\":{\"file_path\":\"/big.txt\"}}\n```", "It's big.")
	client := newTestM365Client(server, &fakeTokens{}, WithM365CopilotMaxMessageChars(3000))
	ctx := sessionContext("s")
	ts := testTools()

	history := []message.Message{userMessage("u1", "Read /big.txt")}
	_, resp := collect(t, client.stream(ctx, history, ts))
	big := strings.Repeat("0123456789abcdef\n", 400) // 6800 characters
	history = append(history,
		assistantMessage("a1", "Reading.", resp.ToolCalls, message.FinishReasonToolUse),
		toolMessage("t1", message.ToolResult{ToolCallID: resp.ToolCalls[0].ID, Content: big}),
	)
	collect(t, client.stream(ctx, history, ts))

	_, requests := graph.snapshot()
	first, second := requests[0].body, requests[1].body
	if len(first.Message.Text) > 3000 {
		t.Errorf("first message has %d characters, over the limit", len(first.Message.Text))
	}
	if len(second.Message.Text) > 3000 {
		t.Errorf("second message has %d characters, over the limit", len(second.Message.Text))
	}
	if !strings.Contains(second.Message.Text, "attached as additional context") {
		t.Errorf("expected a pointer to the moved output:\n%s", second.Message.Text)
	}
	var moved strings.Builder
	for _, c := range second.AdditionalContext {
		if !strings.Contains(c.Description, "Output of action") {
			t.Errorf("unexpected context entry %q", c.Description)
		}
		if len(c.Text) > 3000 {
			t.Errorf("context entry has %d characters, over the limit", len(c.Text))
		}
		moved.WriteString(c.Text)
	}
	if moved.String() != big+"\n</tool_result>" && !strings.Contains(moved.String(), big) {
		t.Errorf("the tool output wasn't moved intact (%d characters moved)", moved.Len())
	}
}

func TestM365WebSearchCanBeDisabled(t *testing.T) {
	graph, server := newMockGraph(t, "Only work data.")
	client := newTestM365Client(server, &fakeTokens{}, WithM365CopilotWebSearch(false))
	collect(t, client.stream(sessionContext("s"), []message.Message{userMessage("u1", "Hi")}, nil))
	_, requests := graph.snapshot()
	resources, _ := requests[0].raw["contextualResources"].(map[string]any)
	web, _ := resources["webContext"].(map[string]any)
	if enabled, ok := web["isWebEnabled"].(bool); !ok || enabled {
		t.Fatalf("expected contextualResources.webContext.isWebEnabled=false, got %v", requests[0].raw["contextualResources"])
	}
}

func TestM365ProviderIsRegistered(t *testing.T) {
	p, err := NewProvider("m365copilot", WithAPIKey("static-token"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*baseProvider[M365CopilotClient]); !ok {
		t.Fatalf("unexpected provider type %T", p)
	}
}
