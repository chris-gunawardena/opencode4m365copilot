package provider

// The Microsoft 365 Copilot Chat API only returns text: it has no native tool
// (function) calling. This file implements a small text protocol on top of it:
// the tools are described in the prompt, Copilot asks for a tool by replying
// with a fenced ```tool_call block holding JSON, and opencode sends the tool
// output back in <tool_result> blocks in the next chat message.

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"

	"github.com/opencode-ai/opencode/internal/llm/tools"
	"github.com/opencode-ai/opencode/internal/message"
)

// m365ToolFence is the language of the fenced block Copilot writes to call a tool.
const m365ToolFence = "tool_call"

// The wording below comes from live tests against Microsoft 365 Copilot. It
// refused to act as "OpenCode" and refused "tools" it compared with its own
// built-in ones ("I can't use the OpenCode-specific tools in this chat"), and
// it treated tool documentation sent as additional context the same way.
// Describing the tools as actions that OpenCode carries out, in the user's
// voice and inside the chat message, made it use them reliably.

// m365ToolProtocol explains how to request an action. Format with the fence and an example call.
const m365ToolProtocol = "I'm working on a code project on my computer with OpenCode, a terminal app that can read files, " +
	"write files and run commands in my project for me. You can't see the project yourself, so whenever you need to " +
	"look at a file, change a file or run a command, write an action block and OpenCode will carry it out automatically " +
	"and paste the output into my next message.\n\n" +
	"Action block format (one per action; the JSON must be valid):\n\n" +
	"```%s\n%s\n```\n\n" +
	"Put action blocks at the end of your reply. Never ask me to paste files or run commands; use an action block. " +
	"When everything I asked for is done, reply without an action block."

// m365FollowUp restates the request and the action format on later turns: in
// live tests Copilot lost track of a request made in an earlier message.
// Format with the request, the fence and the action names.
const m365FollowUp = "My request: \"%s\"\nIf anything is left to do, write the next action block(s) now " +
	"(```%s with {\"name\": ..., \"arguments\": {...}}; actions: %s). I can't run anything myself. " +
	"When everything is done, reply without an action block."

// m365Reminder follows a new user message in an existing conversation. Format
// with the fence and the action names.
const m365Reminder = "(To work on my project, write action blocks: ```%s with {\"name\": ..., \"arguments\": {...}}; " +
	"actions: %s. I can't run anything myself.)"

// m365Nudge is sent once when Copilot declines to use the actions at the start of a request.
const m365Nudge = "You don't need access to my computer: OpenCode carries out the action blocks you write and sends you the output."

// m365ResultsIntro opens a message with tool output.
const m365ResultsIntro = "OpenCode carried out your actions:"

var m365Refusal = regexp.MustCompile(`(?i)\b(?:can['’]?t|cannot|unable to|not able to|don['’]?t have|do not have)\b[^.\n]{0,80}\b(?:access|use|interact|invoke|run|read|open|call)`)

// m365LooksLikeRefusal reports whether a reply declines to use the actions,
// e.g. "I can't access the OpenCode-specific tools from this chat".
func m365LooksLikeRefusal(reply string) bool {
	head := reply
	if len(head) > 400 {
		head = head[:400]
	}
	return m365Refusal.MatchString(head)
}

func m365FollowUpText(request string, ts []tools.BaseTool) string {
	return fmt.Sprintf(m365FollowUp, m365Quote(request), m365ToolFence, m365ToolNames(ts))
}

func m365ReminderText(ts []tools.BaseTool) string {
	return fmt.Sprintf(m365Reminder, m365ToolFence, m365ToolNames(ts))
}

// m365Quote shortens a request to quote it back to Copilot.
func m365Quote(request string) string {
	request = strings.Join(strings.Fields(request), " ")
	if len(request) > 600 {
		request = strings.ToValidUTF8(request[:600], "") + "…"
	}
	return strings.ReplaceAll(request, `"`, `'`)
}

// newM365ToolCallID returns an ID for an emulated tool call.
func newM365ToolCallID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return "call_" + hex.EncodeToString(b)
}

func m365ToolNames(ts []tools.BaseTool) string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Info().Name)
	}
	return strings.Join(names, ", ")
}

// m365ToolInstructions describes the protocol and every tool compactly.
func m365ToolInstructions(ts []tools.BaseTool) string {
	if len(ts) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, m365ToolProtocol, m365ToolFence, m365ExampleToolCall(ts))
	b.WriteString("\n\n## Available actions\n")
	for _, t := range ts {
		info := t.Info()
		fmt.Fprintf(&b, "\n### %s\n%s\n", info.Name, m365Summary(info.Description, 400))
		params := m365DescribeParams(info)
		if params != "" {
			b.WriteString("Arguments:\n")
			b.WriteString(params)
		} else {
			b.WriteString("Arguments: none (use {})\n")
		}
	}
	return b.String()
}

func m365ExampleToolCall(ts []tools.BaseTool) string {
	for _, t := range ts {
		if t.Info().Name == tools.ViewToolName {
			return `{"name": "view", "arguments": {"file_path": "/absolute/path/to/main.go"}}`
		}
	}
	info := ts[0].Info()
	args := map[string]any{}
	for _, name := range info.Required {
		args[name] = "..."
	}
	example, _ := json.Marshal(map[string]any{"name": info.Name, "arguments": args})
	return string(example)
}

// m365Summary returns the first paragraph of a tool description, cut at a sentence end.
func m365Summary(desc string, limit int) string {
	desc = strings.TrimSpace(desc)
	if i := strings.Index(desc, "\n\n"); i >= 0 {
		desc = desc[:i]
	}
	desc = strings.Join(strings.Fields(desc), " ")
	if len(desc) <= limit {
		return desc
	}
	cut := desc[:limit]
	if i := strings.LastIndex(cut, ". "); i > limit/3 {
		return cut[:i+1]
	}
	return strings.TrimSpace(cut) + "…"
}

func m365DescribeParams(info tools.ToolInfo) string {
	if len(info.Parameters) == 0 {
		return ""
	}
	required := map[string]bool{}
	for _, name := range info.Required {
		required[name] = true
	}
	names := make([]string, 0, len(info.Parameters))
	for name := range info.Parameters {
		names = append(names, name)
	}
	// Required parameters first, in the order the tool lists them.
	order := map[string]int{}
	for i, name := range info.Required {
		order[name] = i
	}
	sort.Slice(names, func(i, j int) bool {
		ri, rj := required[names[i]], required[names[j]]
		if ri != rj {
			return ri
		}
		if ri {
			return order[names[i]] < order[names[j]]
		}
		return names[i] < names[j]
	})

	var b strings.Builder
	for _, name := range names {
		schema, _ := info.Parameters[name].(map[string]any)
		req := ""
		if required[name] {
			req = ", required"
		}
		desc := ""
		if d, ok := schema["description"].(string); ok && d != "" {
			desc = ": " + m365Summary(d, 240)
		}
		fmt.Fprintf(&b, "- `%s` (%s%s)%s\n", name, m365DescribeType(schema), req, desc)
	}
	return b.String()
}

func m365DescribeType(schema map[string]any) string {
	if schema == nil {
		return "any"
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		vals := make([]string, 0, len(enum))
		for _, v := range enum {
			encoded, _ := json.Marshal(v)
			vals = append(vals, string(encoded))
		}
		return "one of " + strings.Join(vals, ", ")
	}
	typ, _ := schema["type"].(string)
	switch typ {
	case "array":
		items, _ := schema["items"].(map[string]any)
		return "array of " + m365DescribeType(items)
	case "object":
		props, _ := schema["properties"].(map[string]any)
		if len(props) == 0 {
			return "object"
		}
		keys := make([]string, 0, len(props))
		for k := range props {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fields := make([]string, 0, len(keys))
		for _, k := range keys {
			sub, _ := props[k].(map[string]any)
			fields = append(fields, fmt.Sprintf("%s: %s", k, m365DescribeType(sub)))
		}
		return "object {" + strings.Join(fields, ", ") + "}"
	case "":
		return "any"
	default:
		return typ
	}
}

// m365FormatToolCall renders a tool call the way Copilot is asked to write them,
// for transcripts of earlier turns.
func m365FormatToolCall(call message.ToolCall) string {
	var args any = json.RawMessage("{}")
	if strings.TrimSpace(call.Input) != "" && json.Valid([]byte(call.Input)) {
		args = json.RawMessage(call.Input)
	} else if strings.TrimSpace(call.Input) != "" {
		args = call.Input
	}
	encoded, err := json.Marshal(map[string]any{"name": call.Name, "arguments": args})
	if err != nil {
		encoded = []byte(fmt.Sprintf(`{"name": %q, "arguments": {}}`, call.Name))
	}
	return "```" + m365ToolFence + "\n" + string(encoded) + "\n```"
}

// m365ToolResultHeader opens a tool result block. The closing tag is "</tool_result>".
func m365ToolResultHeader(result message.ToolResult, name string) string {
	status := "success"
	if result.IsError {
		status = "error"
	}
	return fmt.Sprintf("<tool_result id=%q name=%q status=%q>", result.ToolCallID, name, status)
}

// m365TruncateMiddle shortens s to about limit characters, keeping its start and end.
func m365TruncateMiddle(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	keepHead := limit * 2 / 3
	keepTail := limit - keepHead
	head := strings.ToValidUTF8(s[:keepHead], "")
	tail := strings.ToValidUTF8(s[len(s)-keepTail:], "")
	return fmt.Sprintf("%s\n\n[... %d characters omitted ...]\n\n%s", head, len(s)-keepHead-keepTail, tail)
}

// ---------------------------------------------------------------------------
// Parsing Copilot's replies

// m365ToolBlock is the position of a tool call block inside a reply.
type m365ToolBlock struct {
	start, end int // byte range of the whole block, including fences or tags
	body       string
	closed     bool
}

// m365FindToolBlocks finds ```tool_call fences and <tool_call> tags in order.
// An unclosed block runs to the end of the text (it is still being streamed).
func m365FindToolBlocks(s string) []m365ToolBlock {
	var blocks []m365ToolBlock
	for pos := 0; pos < len(s); {
		fStart, fBody := m365FindToolFence(s, pos)
		tStart, tBody, escaped := m365FindToolTag(s, pos)
		switch {
		case fStart < 0 && tStart < 0:
			return blocks
		case fStart >= 0 && (tStart < 0 || fStart < tStart):
			end, bodyEnd, closed := m365FindFenceClose(s, fBody)
			blocks = append(blocks, m365ToolBlock{start: fStart, end: end, body: s[fBody:bodyEnd], closed: closed})
			pos = end
		default:
			closeTag := "</tool_call"
			if escaped {
				closeTag = "&lt;/tool_call"
			}
			end, bodyEnd, closed := len(s), len(s), false
			if i := m365IndexFold(s[tBody:], closeTag); i >= 0 {
				bodyEnd = tBody + i
				closer := "&gt;"
				if !escaped {
					closer = ">"
				}
				if j := strings.Index(s[bodyEnd:], closer); j >= 0 {
					end = bodyEnd + j + len(closer)
				} else {
					end = len(s)
				}
				closed = true
			}
			body := s[tBody:bodyEnd]
			if escaped {
				body = html.UnescapeString(body)
			}
			blocks = append(blocks, m365ToolBlock{start: tStart, end: end, body: body, closed: closed})
			pos = end
		}
	}
	return blocks
}

// m365FindToolFence returns the start of the next ```tool_call fence line at or
// after pos, and where its body starts.
func m365FindToolFence(s string, pos int) (start, bodyStart int) {
	for {
		i := strings.Index(s[pos:], "```")
		if i < 0 {
			return -1, -1
		}
		i += pos
		lineStart := strings.LastIndexByte(s[:i], '\n') + 1
		if strings.TrimLeft(s[lineStart:i], " \t") == "" {
			nl := strings.IndexByte(s[i:], '\n')
			if nl >= 0 {
				info := strings.TrimLeft(s[i:i+nl], "`")
				if m365IsToolFenceInfo(info) {
					return lineStart, i + nl + 1
				}
			}
		}
		pos = i + 3
	}
}

func m365IsToolFenceInfo(info string) bool {
	info = strings.ToLower(strings.TrimSpace(info))
	info = strings.NewReplacer("_", "", "-", "", " ", "").Replace(info)
	return info == "toolcall" || info == "tooluse" || info == "action"
}

// m365FindFenceClose finds the closing ``` line of a fenced block whose body starts at bodyStart.
func m365FindFenceClose(s string, bodyStart int) (end, bodyEnd int, closed bool) {
	for pos := bodyStart; pos <= len(s); {
		lineEnd := strings.IndexByte(s[pos:], '\n')
		line := s[pos:]
		if lineEnd >= 0 {
			line = s[pos : pos+lineEnd]
		}
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "```") && strings.Trim(trimmed, "`") == "" {
			if lineEnd < 0 {
				return len(s), pos, true
			}
			return pos + lineEnd + 1, pos, true
		}
		if lineEnd < 0 {
			break
		}
		pos += lineEnd + 1
	}
	return len(s), len(s), false
}

func m365FindToolTag(s string, pos int) (start, bodyStart int, escaped bool) {
	for {
		raw := m365IndexFold(s[pos:], "<tool_call")
		esc := m365IndexFold(s[pos:], "&lt;tool_call")
		if raw < 0 && esc < 0 {
			return -1, -1, false
		}
		escaped = esc >= 0 && (raw < 0 || esc < raw)
		open, closer := "<tool_call", ">"
		i := raw
		if escaped {
			open, closer, i = "&lt;tool_call", "&gt;", esc
		}
		i += pos
		after := i + len(open)
		// The tag name must end here: "<tool_call>" or "<tool_call id=...>", not "<tool_calls>".
		if after < len(s) && (s[after] == '>' || s[after] == ' ' || s[after] == '\n' || s[after] == '&') {
			if j := strings.Index(s[after:], closer); j >= 0 {
				return i, after + j + len(closer), escaped
			}
			return i, len(s), escaped
		}
		if after >= len(s) {
			return i, len(s), escaped
		}
		pos = after
	}
}

func m365IndexFold(s, substr string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(substr))
}

// m365ParseToolCalls extracts the tool calls Copilot asked for in a complete reply.
func m365ParseToolCalls(text string, available []tools.BaseTool) []message.ToolCall {
	if len(available) == 0 {
		return nil
	}
	var calls []message.ToolCall
	for _, block := range m365FindToolBlocks(text) {
		calls = append(calls, m365ParseCallBody(block.body, available, false)...)
	}
	if len(calls) > 0 {
		return calls
	}

	// Fallback: Copilot sometimes ignores the fence language and writes the JSON
	// in a ```json block or as plain text. Only accept objects that name a known
	// tool and carry arguments, so example JSON isn't run by mistake.
	for _, body := range m365CodeBlocks(text) {
		calls = append(calls, m365ParseCallBody(body, available, true)...)
	}
	if len(calls) > 0 {
		return calls
	}
	return m365ParseCallBody(text, available, true)
}

// m365CodeBlocks returns the bodies of all fenced code blocks.
func m365CodeBlocks(text string) []string {
	var bodies []string
	for pos := 0; pos < len(text); {
		i := strings.Index(text[pos:], "```")
		if i < 0 {
			break
		}
		i += pos
		nl := strings.IndexByte(text[i:], '\n')
		if nl < 0 {
			break
		}
		end, bodyEnd, closed := m365FindFenceClose(text, i+nl+1)
		bodies = append(bodies, text[i+nl+1:bodyEnd])
		if !closed {
			break
		}
		pos = end
	}
	return bodies
}

// m365ParseCallBody decodes every tool call object in body. With strict set,
// objects must name a known tool and have an arguments field.
func m365ParseCallBody(body string, available []tools.BaseTool, strict bool) []message.ToolCall {
	var calls []message.ToolCall
	for _, value := range m365DecodeJSONValues(body) {
		for _, obj := range m365CallObjects(value) {
			call, ok := m365CallFromObject(obj, available, strict)
			if ok {
				calls = append(calls, call)
			}
		}
	}
	return calls
}

// m365DecodeJSONValues decodes all top-level JSON objects and arrays in s,
// skipping any text between them and repairing common mistakes.
func m365DecodeJSONValues(s string) []any {
	var values []any
	for pos := 0; pos < len(s); {
		i := strings.IndexAny(s[pos:], "{[")
		if i < 0 {
			break
		}
		i += pos
		end, ok := m365MatchBracket(s, i)
		if !ok {
			end = len(s)
		}
		candidate := s[i:end]
		var v any
		if err := json.Unmarshal([]byte(candidate), &v); err != nil {
			if err := json.Unmarshal([]byte(m365RepairJSON(candidate)), &v); err != nil {
				pos = i + 1
				continue
			}
		}
		values = append(values, v)
		pos = end
	}
	return values
}

// m365MatchBracket returns the index just after the bracket that closes s[start],
// treating string literals correctly even if they contain raw newlines.
func m365MatchBracket(s string, start int) (int, bool) {
	depth := 0
	inString, escaped := false, false
	for i := start; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1, true
			}
		}
	}
	return len(s), false
}

var m365TrailingComma = regexp.MustCompile(`,\s*([}\]])`)

// m365RepairJSON escapes raw control characters inside strings, removes
// trailing commas and closes unterminated brackets.
func m365RepairJSON(s string) string {
	var b bytes.Buffer
	var stack []byte
	inString, escaped := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inString {
			switch {
			case escaped:
				escaped = false
				b.WriteByte(c)
			case c == '\\':
				escaped = true
				b.WriteByte(c)
			case c == '"':
				inString = false
				b.WriteByte(c)
			case c == '\n':
				b.WriteString(`\n`)
			case c == '\r':
				b.WriteString(`\r`)
			case c == '\t':
				b.WriteString(`\t`)
			case c < 0x20:
				fmt.Fprintf(&b, `\u%04x`, c)
			default:
				b.WriteByte(c)
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
		b.WriteByte(c)
	}
	if inString {
		b.WriteByte('"')
	}
	for i := len(stack) - 1; i >= 0; i-- {
		b.WriteByte(stack[i])
	}
	return m365TrailingComma.ReplaceAllString(b.String(), "$1")
}

// m365CallObjects flattens the shapes a tool call can arrive in: a single
// object, an array of objects, or {"tool_calls": [...]}.
func m365CallObjects(v any) []map[string]any {
	switch t := v.(type) {
	case []any:
		var out []map[string]any
		for _, item := range t {
			out = append(out, m365CallObjects(item)...)
		}
		return out
	case map[string]any:
		for _, key := range []string{"tool_calls", "toolCalls", "calls"} {
			if list, ok := t[key].([]any); ok {
				return m365CallObjects(list)
			}
		}
		// OpenAI style: {"type": "function", "function": {"name": ..., "arguments": "..."}}
		if fn, ok := t["function"].(map[string]any); ok {
			return []map[string]any{fn}
		}
		return []map[string]any{t}
	}
	return nil
}

func m365CallFromObject(obj map[string]any, available []tools.BaseTool, strict bool) (message.ToolCall, bool) {
	var name string
	nameKey := ""
	for _, key := range []string{"name", "tool", "tool_name", "toolName", "function"} {
		if v, ok := obj[key].(string); ok && v != "" {
			name, nameKey = v, key
			break
		}
	}
	if name == "" {
		return message.ToolCall{}, false
	}
	canonical, known := m365CanonicalToolName(name, available)
	if !known && strict {
		return message.ToolCall{}, false
	}

	var args any
	hasArgs := false
	for _, key := range []string{"arguments", "args", "input", "parameters", "params"} {
		if v, ok := obj[key]; ok {
			args, hasArgs = v, true
			break
		}
	}
	if !hasArgs {
		if strict {
			return message.ToolCall{}, false
		}
		// {"name": "ls", "path": "."}: everything but the name is the arguments.
		rest := map[string]any{}
		for k, v := range obj {
			if k != nameKey && k != "id" && k != "type" {
				rest[k] = v
			}
		}
		args = rest
	}

	input := "{}"
	switch a := args.(type) {
	case nil:
	case string:
		// Arguments encoded as a JSON string, OpenAI style.
		trimmed := strings.TrimSpace(a)
		if json.Valid([]byte(trimmed)) && strings.HasPrefix(trimmed, "{") {
			input = trimmed
		} else if repaired := m365RepairJSON(trimmed); strings.HasPrefix(trimmed, "{") && json.Valid([]byte(repaired)) {
			input = repaired
		} else if trimmed != "" {
			input = a // let the tool report the bad input
		}
	default:
		encoded, err := json.Marshal(a)
		if err == nil {
			input = string(encoded)
		}
	}
	return message.ToolCall{
		ID:       newM365ToolCallID(),
		Name:     canonical,
		Input:    input,
		Type:     "function",
		Finished: true,
	}, true
}

func m365CanonicalToolName(name string, available []tools.BaseTool) (string, bool) {
	name = strings.TrimSpace(name)
	name = strings.TrimPrefix(name, "functions.")
	for _, t := range available {
		if t.Info().Name == name {
			return name, true
		}
	}
	for _, t := range available {
		if strings.EqualFold(t.Info().Name, name) {
			return t.Info().Name, true
		}
	}
	return name, false
}

// ---------------------------------------------------------------------------
// Turning Copilot's text into what opencode shows

var (
	// Copilot wraps entities it recognizes in tags such as <Person>Jane</Person>.
	m365EntityTag = regexp.MustCompile(`</?(?:Person|People|File|Event|Meeting|Email|Message|Chat|Channel|Team|Site|Page|Task|Contact|Group|Organization|Link)(?:\s[^<>]*)?>`)
	// Citation markers such as [^1^] or [^external^].
	m365Citation   = regexp.MustCompile(`\[\^[^\^\]\s]{1,24}\^\]`)
	m365BlankLines = regexp.MustCompile(`\n[ \t]*\n(?:[ \t]*\n)+`)
	m365EmptyFence = regexp.MustCompile("(?m)^[ \\t]*```[A-Za-z0-9_+.-]*[ \\t]*\\n(?:[ \\t]*\\n)*[ \\t]*```[ \\t]*(?:\\n|$)")
	// Trailing text that may be the start of a tag, an entity or a citation.
	m365PartialTag      = regexp.MustCompile(`</?[A-Za-z_]*$`)
	m365PartialEntity   = regexp.MustCompile(`&[A-Za-z]{0,6}$`)
	m365PartialCitation = regexp.MustCompile(`\[(?:\^[^\]\s]{0,24})?$`)
)

// m365VisibleText returns the part of a (possibly incomplete) reply that should
// be shown: tool call blocks and Copilot's entity markup are removed. While
// streaming (final=false) it also holds back trailing text that might turn out
// to be markup, so what has been shown stays a prefix of the final text.
func m365VisibleText(full string, final bool) string {
	s := full
	if blocks := m365FindToolBlocks(s); len(blocks) > 0 {
		var b strings.Builder
		last := 0
		for _, block := range blocks {
			b.WriteString(s[last:block.start])
			last = block.end
		}
		b.WriteString(s[last:])
		s = b.String()
	}
	if !final {
		s = m365HoldBack(s)
	}
	s = m365SanitizeMarkup(s)
	s = m365EmptyFence.ReplaceAllString(s, "")
	s = m365BlankLines.ReplaceAllString(s, "\n\n")
	// Trailing whitespace is trimmed from the final text, so while streaming it
	// is held back until more text follows.
	return strings.TrimSpace(s)
}

func m365HoldBack(s string) string {
	// Cutting one construct can expose another (e.g. "```xml\n<"), so repeat until stable.
	for {
		prev := s
		for _, re := range []*regexp.Regexp{m365PartialTag, m365PartialEntity, m365PartialCitation} {
			if loc := re.FindStringIndex(s); loc != nil && loc[1]-loc[0] <= 40 {
				s = s[:loc[0]]
			}
		}
		// A fence line that hasn't finished yet might become ```tool_call.
		lineStart := strings.LastIndexByte(s, '\n') + 1
		if strings.HasPrefix(strings.TrimLeft(s[lineStart:], " \t"), "`") {
			s = s[:lineStart]
		}
		// A fence that just opened with nothing after it yet might only hold a tool call.
		if open := m365OpenFenceStart(s); open >= 0 && !strings.Contains(strings.TrimRight(s[open:], " \t\n"), "\n") {
			s = s[:open]
		}
		if s == prev {
			return s
		}
	}
}

// m365OpenFenceStart returns where the last code fence starts if that fence is
// still open, or -1.
func m365OpenFenceStart(s string) int {
	open := -1
	for pos := 0; pos < len(s); {
		lineEnd := strings.IndexByte(s[pos:], '\n')
		line := s[pos:]
		if lineEnd >= 0 {
			line = s[pos : pos+lineEnd]
		}
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			if open >= 0 {
				open = -1
			} else {
				open = pos
			}
		}
		if lineEnd < 0 {
			break
		}
		pos += lineEnd + 1
	}
	return open
}

// m365SanitizeMarkup strips Copilot's entity tags and citation markers outside code blocks.
func m365SanitizeMarkup(s string) string {
	if !strings.ContainsAny(s, "<[") {
		return s
	}
	var b strings.Builder
	inFence := false
	for _, line := range strings.SplitAfter(s, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			b.WriteString(line)
			continue
		}
		if inFence {
			b.WriteString(line)
			continue
		}
		line = m365EntityTag.ReplaceAllString(line, "")
		line = m365Citation.ReplaceAllString(line, "")
		b.WriteString(line)
	}
	return b.String()
}
