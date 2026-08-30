package main

// wire.go — OpenAI Chat Completions → Command Code /alpha/generate wire format.
// Wire format reverse-engineered from command-code CLI v1.38.2 (dist/cli.mjs).

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ---------- OpenAI request types ----------

type OAContentPart struct {
	Type     string          `json:"type"`
	Text     string          `json:"text,omitempty"`
	ImageURL json.RawMessage `json:"image_url,omitempty"`
}

type OAToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type OAMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"` // string or []parts
	ToolCalls  []OAToolCall    `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type OAFunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type OATool struct {
	Type     string         `json:"type"`
	Function OAFunctionDef  `json:"function"`
}

type OARequest struct {
	Model       string      `json:"model"`
	Messages    []OAMessage `json:"messages"`
	Tools       []OATool    `json:"tools,omitempty"`
	ToolChoice  interface{} `json:"tool_choice,omitempty"`
	Stream      bool        `json:"stream,omitempty"`
	MaxTokens   int         `json:"max_tokens,omitempty"`
	Temperature *float64    `json:"temperature,omitempty"`
}

// ---------- Command Code wire types ----------

type CCConfig struct {
	WorkingDir    string   `json:"workingDir"`
	Date          string   `json:"date"`
	Environment   string   `json:"environment"`
	Structure     []string `json:"structure"`
	IsGitRepo     bool     `json:"isGitRepo"`
	CurrentBranch string   `json:"currentBranch"`
	MainBranch    string   `json:"mainBranch"`
	GitStatus     string   `json:"gitStatus"`
	RecentCommits []string `json:"recentCommits"`
}

type CCBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`

	// text block
	// tool-call block
	ToolCallID string       `json:"toolCallId,omitempty"`
	ToolName   string       `json:"toolName,omitempty"`
	Input      interface{}  `json:"input,omitempty"`

	// tool-result block
	Output *CCOutput `json:"output,omitempty"`
}

type CCOutput struct {
	Type  string      `json:"type"` // "text" | "error-text"
	Value interface{} `json:"value"`
}

type CCMessage struct {
	Role    string    `json:"role"`
	Content []CCBlock `json:"content"`
}

type CCWireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type CCParams struct {
	Model           string       `json:"model"`
	Messages        []CCMessage  `json:"messages"`
	Tools           []CCWireTool `json:"tools"`
	System          string       `json:"system"`
	MaxTokens       int          `json:"max_tokens"`
	Stream          bool         `json:"stream"`
	Temperature     *float64     `json:"temperature,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
}

type CCRequest struct {
	Config         CCConfig  `json:"config"`
	Memory         *string   `json:"memory"`
	Taste          *string   `json:"taste"`
	Skills         *string   `json:"skills"`
	PermissionMode string    `json:"permissionMode"`
	ThreadID       *string   `json:"threadId,omitempty"` // must be omitted, never null
	Mode           string    `json:"mode"`
	Params         CCParams  `json:"params"`
}

// ---------- translation ----------

func buildCCConfig(workingDir string) CCConfig {
	return CCConfig{
		WorkingDir:    workingDir,
		Date:          time.Now().UTC().Format("2006-01-02"),
		Environment:   "linux",
		Structure:     []string{},
		IsGitRepo:     false,
		CurrentBranch: "",
		MainBranch:    "main",
		GitStatus:     "Working tree clean",
		RecentCommits: []string{},
	}
}

// parseOpenAIContent handles content as string, []parts, or null.
func parseOpenAIContent(raw json.RawMessage) (text string) {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []OAContentPart
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

// TranslateMessages converts OpenAI messages to CC wire messages.
// System messages are collected and returned separately (joined).
func TranslateMessages(msgs []OAMessage) (wire []CCMessage, system string) {
	var sysParts []string
	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			if t := parseOpenAIContent(m.Content); t != "" {
				sysParts = append(sysParts, t)
			}
		case "user":
			wire = append(wire, CCMessage{Role: "user", Content: []CCBlock{{Type: "text", Text: parseOpenAIContent(m.Content)}}})
		case "assistant":
			var blocks []CCBlock
			if t := parseOpenAIContent(m.Content); t != "" {
				blocks = append(blocks, CCBlock{Type: "text", Text: t})
			}
			for _, tc := range m.ToolCalls {
				var input interface{}
			if err := json.Unmarshal([]byte(tc.Function.Arguments), &input); err != nil {
				input = map[string]interface{}{"_raw": tc.Function.Arguments}
			}
			blocks = append(blocks, CCBlock{Type: "tool-call", ToolCallID: tc.ID, ToolName: tc.Function.Name, Input: input})
			}
			if blocks == nil {
				blocks = []CCBlock{{Type: "text", Text: ""}}
			}
			wire = append(wire, CCMessage{Role: "assistant", Content: blocks})
		case "tool":
			name := m.Name
			if name == "" {
				name = m.ToolCallID
			}
			wire = append(wire, CCMessage{Role: "user", Content: []CCBlock{{
				Type: "tool-result", ToolCallID: m.ToolCallID,
				Output: &CCOutput{Type: "text", Value: parseOpenAIContent(m.Content)},
			}}})
			_ = name
		default:
			wire = append(wire, CCMessage{Role: m.Role, Content: []CCBlock{{Type: "text", Text: parseOpenAIContent(m.Content)}}})
		}
	}
	return wire, strings.Join(sysParts, "\n\n")
}

func TranslateTools(tools []OATool) []CCWireTool {
	out := make([]CCWireTool, 0) // upstream requires an array, never null
	for _, t := range tools {
		if t.Type != "function" {
			continue
		}
		schema := t.Function.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, CCWireTool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

func BuildCCRequest(req *OARequest, workingDir string) (*CCRequest, error) {
	wire, system := TranslateMessages(req.Messages)
	if len(wire) == 0 {
		return nil, fmt.Errorf("no messages after translation")
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 64000 // CLI default wv=64e3
	}
	model := req.Model
	if model == "" {
		model = "deepseek/deepseek-v4-flash"
	}
	return &CCRequest{
		Config:         buildCCConfig(workingDir),
		Memory:         nil,
		Taste:          nil,
		Skills:         nil,
		PermissionMode: "standard",
		ThreadID:       nil,
		Mode:           "agent",
		Params: CCParams{
			Model:       model,
			Messages:    wire,
			Tools:       TranslateTools(req.Tools),
			System:      system,
			MaxTokens:   maxTokens,
			Stream:      true, // always stream from upstream; buffer if client wants non-stream
			Temperature: req.Temperature,
		},
	}, nil
}

// ---------- upstream NDJSON events ----------

type CCStreamEvent struct {
	Type            string          `json:"type"`
	Text            string          `json:"text,omitempty"`
	ToolName        string          `json:"toolName,omitempty"`
	ToolCallID      string          `json:"toolCallId,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	Args            json.RawMessage `json:"args,omitempty"`
	ProviderExec    bool            `json:"providerExecuted,omitempty"`
	FinishReason    string          `json:"finishReason,omitempty"`
	RawFinishReason string          `json:"rawFinishReason,omitempty"`
	TotalUsage      *CCTotalUsage   `json:"totalUsage,omitempty"`
	Error           json.RawMessage `json:"error,omitempty"`
}

type CCTotalUsage struct {
	InputTokens       int `json:"inputTokens"`
	OutputTokens      int `json:"outputTokens"`
	InputTokenDetails *struct {
		CacheReadTokens  int `json:"cacheReadTokens"`
		CacheWriteTokens int `json:"cacheWriteTokens"`
	} `json:"inputTokenDetails,omitempty"`
}

// Upstream HTTP error body: {"success":false,"error":{"code","status","message"}}
type CCErrorBody struct {
	Success bool `json:"success"`
	Error   *struct {
		Code    string `json:"code"`
		Status  int    `json:"status"`
		Message string `json:"message"`
	} `json:"error"`
}

// MapCCFinishReason maps CC finish reasons to OpenAI finish_reason.
func MapCCFinishReason(cc string) string {
	switch strings.ToLower(cc) {
	case "tool-calls", "tool_use", "tool_calls":
		return "tool_calls"
	case "length", "max_tokens":
		return "length"
	default:
		return "stop"
	}
}
