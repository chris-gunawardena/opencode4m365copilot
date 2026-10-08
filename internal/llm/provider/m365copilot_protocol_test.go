package provider

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/opencode-ai/opencode/internal/llm/tools"
)

type fakeTool struct {
	info tools.ToolInfo
}

func (t fakeTool) Info() tools.ToolInfo { return t.info }
func (t fakeTool) Run(context.Context, tools.ToolCall) (tools.ToolResponse, error) {
	return tools.NewTextResponse("ok"), nil
}

func testTools() []tools.BaseTool {
	return []tools.BaseTool{
		fakeTool{tools.ToolInfo{
			Name:        "view",
			Description: "Reads a file from the local filesystem.\n\nMore details here.",
			Parameters: map[string]any{
				"file_path": map[string]any{"type": "string", "description": "The path to the file to read"},
				"offset":    map[string]any{"type": "integer", "description": "Line to start at"},
			},
			Required: []string{"file_path"},
		}},
		fakeTool{tools.ToolInfo{
			Name:        "write",
			Description: "Writes a file.",
			Parameters: map[string]any{
				"file_path": map[string]any{"type": "string"},
				"content":   map[string]any{"type": "string"},
			},
			Required: []string{"file_path", "content"},
		}},
		fakeTool{tools.ToolInfo{
			Name:        "ls",
			Description: "Lists files.",
			Parameters: map[string]any{
				"path":   map[string]any{"type": "string"},
				"ignore": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		}},
	}
}

func argsOf(t *testing.T, input string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(input), &m); err != nil {
		t.Fatalf("tool input %q is not a JSON object: %v", input, err)
	}
	return m
}

func TestM365ParseToolCalls(t *testing.T) {
	ts := testTools()
	cases := []struct {
		name  string
		text  string
		want  []string // tool names
		check func(t *testing.T, inputs []string)
	}{
		{
			name: "fenced block",
			text: "Let me look.\n\n```tool_call\n{\"name\": \"view\", \"arguments\": {\"file_path\": \"/a/main.go\"}}\n```\n",
			want: []string{"view"},
			check: func(t *testing.T, inputs []string) {
				if argsOf(t, inputs[0])["file_path"] != "/a/main.go" {
					t.Errorf("unexpected arguments %s", inputs[0])
				}
			},
		},
		{
			name: "two fenced blocks",
			text: "```tool_call\n{\"name\":\"view\",\"arguments\":{\"file_path\":\"/a\"}}\n```\n```tool_call\n{\"name\":\"ls\",\"arguments\":{\"path\":\"/\"}}\n```",
			want: []string{"view", "ls"},
		},
		{
			name: "xml tag",
			text: "Checking.\n<tool_call>\n{\"name\": \"ls\", \"arguments\": {\"path\": \".\"}}\n</tool_call>",
			want: []string{"ls"},
		},
		{
			name: "html escaped tag",
			text: "&lt;tool_call&gt;{&quot;name&quot;: &quot;ls&quot;, &quot;arguments&quot;: {}}&lt;/tool_call&gt;",
			want: []string{"ls"},
		},
		{
			name: "tag inside xml fence",
			text: "```xml\n<tool_call>{\"name\":\"view\",\"arguments\":{\"file_path\":\"x\"}}</tool_call>\n```",
			want: []string{"view"},
		},
		{
			name: "json fence fallback",
			text: "I'll read it:\n```json\n{\"name\": \"view\", \"arguments\": {\"file_path\": \"/b\"}}\n```",
			want: []string{"view"},
		},
		{
			name: "bare json fallback",
			text: "{\"name\": \"ls\", \"arguments\": {\"path\": \"/tmp\"}}",
			want: []string{"ls"},
		},
		{
			name: "openai shaped list",
			text: "```tool_call\n{\"tool_calls\": [{\"type\": \"function\", \"function\": {\"name\": \"view\", \"arguments\": \"{\\\"file_path\\\": \\\"/c\\\"}\"}}]}\n```",
			want: []string{"view"},
			check: func(t *testing.T, inputs []string) {
				if argsOf(t, inputs[0])["file_path"] != "/c" {
					t.Errorf("string arguments not decoded: %s", inputs[0])
				}
			},
		},
		{
			name: "raw newlines inside strings are repaired",
			text: "```tool_call\n{\"name\": \"write\", \"arguments\": {\"file_path\": \"/x.go\", \"content\": \"package x\n\nfunc A() {}\n\"}}\n```",
			want: []string{"write"},
			check: func(t *testing.T, inputs []string) {
				if argsOf(t, inputs[0])["content"] != "package x\n\nfunc A() {}\n" {
					t.Errorf("content not repaired: %s", inputs[0])
				}
			},
		},
		{
			name: "fence inside a JSON string doesn't end the block",
			text: "```tool_call\n{\"name\": \"write\", \"arguments\": {\"file_path\": \"/r.md\", \"content\": \"```go\\nx\\n```\"}}\n```",
			want: []string{"write"},
		},
		{
			name: "unclosed block at the end",
			text: "```tool_call\n{\"name\": \"ls\", \"arguments\": {\"path\": \"/\"}}",
			want: []string{"ls"},
		},
		{
			name: "case-insensitive tool name",
			text: "```tool_call\n{\"name\": \"View\", \"arguments\": {\"file_path\": \"/a\"}}\n```",
			want: []string{"view"},
		},
		{
			name: "arguments inline next to the name",
			text: "```tool_call\n{\"name\": \"ls\", \"path\": \"/src\"}\n```",
			want: []string{"ls"},
			check: func(t *testing.T, inputs []string) {
				if argsOf(t, inputs[0])["path"] != "/src" {
					t.Errorf("inline arguments lost: %s", inputs[0])
				}
			},
		},
		{
			name: "unknown tool in explicit block is passed through",
			text: "```tool_call\n{\"name\": \"rm_rf\", \"arguments\": {}}\n```",
			want: []string{"rm_rf"},
		},
		{
			name: "example json for an unknown tool is ignored",
			text: "Here's a config:\n```json\n{\"name\": \"my-app\", \"arguments\": {\"debug\": true}}\n```",
			want: nil,
		},
		{
			name: "plain answer",
			text: "The function `main` starts the server. Use `{}` for an empty object.",
			want: nil,
		},
		{
			name: "code answer with braces",
			text: "```go\nfunc main() {\n\tif x { y() }\n}\n```",
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := m365ParseToolCalls(tc.text, ts)
			var names, inputs []string
			for _, c := range calls {
				names = append(names, c.Name)
				inputs = append(inputs, c.Input)
				if !strings.HasPrefix(c.ID, "call_") || !c.Finished {
					t.Errorf("bad tool call metadata: %+v", c)
				}
			}
			if strings.Join(names, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("got tools %v, want %v", names, tc.want)
			}
			if tc.check != nil {
				tc.check(t, inputs)
			}
		})
	}
}

func TestM365ParseToolCallsWithoutTools(t *testing.T) {
	text := "```tool_call\n{\"name\": \"view\", \"arguments\": {}}\n```"
	if calls := m365ParseToolCalls(text, nil); len(calls) != 0 {
		t.Fatalf("expected no tool calls when no tools are available, got %v", calls)
	}
}

func TestM365VisibleText(t *testing.T) {
	full := "You asked about <File>main.go</File>[^1^]. Let me read it.\n\n" +
		"```tool_call\n{\"name\": \"view\", \"arguments\": {\"file_path\": \"/a/main.go\"}}\n```\n"
	got := m365VisibleText(full, true)
	want := "You asked about main.go. Let me read it."
	if got != want {
		t.Fatalf("visible text = %q, want %q", got, want)
	}

	code := "Use this:\n```html\n<Person>keep me</Person>\n```"
	if got := m365VisibleText(code, true); got != code {
		t.Fatalf("markup inside code blocks must be kept, got %q", got)
	}
}

// Every prefix of a streamed reply must produce visible text that is a prefix
// of the final visible text, otherwise deltas can't be emitted consistently.
func TestM365VisibleTextIsPrefixStable(t *testing.T) {
	replies := []string{
		"I'll check <Person>Jane</Person>'s notes[^2^] first.\n\n```tool_call\n{\"name\": \"ls\", \"arguments\": {\"path\": \".\"}}\n```\n```tool_call\n{\"name\": \"view\", \"arguments\": {\"file_path\": \"/x\"}}\n```",
		"Reading it now.\n```xml\n<tool_call>\n{\"name\": \"ls\", \"arguments\": {}}\n</tool_call>\n```",
		"Here is the fix:\n\n```go\nfunc a() int { return 1 < 2 }\n```\n\nDone [see docs](https://example.com).",
		"Plain text with a < b and an & sign.",
	}
	for _, reply := range replies {
		final := m365VisibleText(reply, true)
		shown := ""
		for i := 1; i <= len(reply); i++ {
			visible := m365VisibleText(reply[:i], false)
			if strings.HasPrefix(visible, shown) {
				shown = visible
			}
			if !strings.HasPrefix(final, shown) {
				t.Fatalf("reply %q: after %d bytes shown %q is not a prefix of final %q", reply, i, shown, final)
			}
			if strings.Contains(shown, "tool_call") || strings.Contains(shown, "<Person") || strings.Contains(shown, "[^") {
				t.Fatalf("reply %q: markup leaked into shown text %q", reply, shown)
			}
		}
		if shown != final {
			t.Errorf("reply %q: streamed %q but final is %q", reply, shown, final)
		}
	}
}

func TestM365ToolInstructions(t *testing.T) {
	text := m365ToolInstructions(testTools())
	for _, want := range []string{
		"```tool_call",
		"### view",
		"- `file_path` (string, required): The path to the file to read",
		"- `offset` (integer)",
		"- `ignore` (array of string)",
		"Reads a file from the local filesystem.",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("tool instructions missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "More details here") {
		t.Errorf("compact catalog should only hold the first paragraph")
	}
	if m365ToolInstructions(nil) != "" {
		t.Errorf("no tools should give no instructions")
	}
}

func TestM365ReadSSE(t *testing.T) {
	// The shape shown in the API documentation: pretty-printed JSON after
	// "data:" and an id line before the blank line.
	docs := "data: {\n  \"id\": \"c1\",\n  \"displayName\": \"Intermediate Conversation Update\",\n  \"messages\": [\n    {\"id\": \"m1\", \"text\": \"Hel\"}\n  ]\n}\nid:137\n\n" +
		"data: {\n  \"id\": \"c1\",\n  \"messages\": []\n}\nid:141\n\n"
	// What Graph actually sends: one compact line per event.
	compact := "data: {\"id\":\"c1\",\"messages\":[{\"id\":\"m1\",\"text\":\"Hel\"}]}\r\n\r\n: keep-alive\r\n\r\ndata: {\"id\":\"c1\",\"messages\":[]}\r\n\r\n"
	// Events without a blank line between them, and no trailing newline.
	squashed := "data: {\"id\":\"c1\",\"messages\":[{\"id\":\"m1\",\"text\":\"Hel\"}]}\ndata: {\"id\":\"c1\",\"messages\":[]}"

	for name, stream := range map[string]string{"docs": docs, "compact": compact, "squashed": squashed} {
		var events []m365ConversationResponse
		err := m365ReadSSE(strings.NewReader(stream), func(data []byte) error {
			var conv m365ConversationResponse
			if err := json.Unmarshal(data, &conv); err != nil {
				t.Fatalf("%s: event %q is not JSON: %v", name, data, err)
			}
			events = append(events, conv)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(events) != 2 || len(events[0].Messages) != 1 || events[0].Messages[0].Text != "Hel" {
			t.Fatalf("%s: unexpected events %+v", name, events)
		}
	}
}

func TestM365ResponseText(t *testing.T) {
	prompt := "What meeting do I have?"
	final := m365ConversationResponse{
		DisplayName: prompt,
		Messages: []m365ResponseMessage{
			{ID: "u1", Text: prompt},
			{ID: "r1", Text: "You have a standup."},
		},
	}
	if got, _ := m365ResponseText(final, prompt, nil); got != "You have a standup." {
		t.Fatalf("final: got %q", got)
	}

	// The echo isn't recognized by text (Copilot rewrote it), but it comes first.
	final.Messages[0].Text = "what meeting do i have"
	if got, _ := m365ResponseText(final, prompt, map[string]bool{"r1": true}); got != "You have a standup." {
		t.Fatalf("final with rewritten echo: got %q", got)
	}

	partial := m365ConversationResponse{
		DisplayName: m365IntermediateUpdate,
		Messages:    []m365ResponseMessage{{ID: "r1", Text: "You have"}},
	}
	if got, _ := m365ResponseText(partial, prompt, nil); got != "You have" {
		t.Fatalf("partial: got %q", got)
	}
}

func TestM365RepairJSON(t *testing.T) {
	for in, want := range map[string]string{
		"{\"a\": \"x\ny\"}":      `{"a": "x\ny"}`,
		`{"a": [1, 2,], }`:       `{"a": [1, 2]}`,
		`{"a": {"b": "c"`:        `{"a": {"b": "c"}}`,
		"{\"a\": \"tab\there\"}": `{"a": "tab\there"}`,
	} {
		got := m365RepairJSON(in)
		if got != want {
			t.Errorf("repair(%q) = %q, want %q", in, got, want)
		}
		if !json.Valid([]byte(got)) {
			t.Errorf("repair(%q) is not valid JSON: %q", in, got)
		}
	}
}
