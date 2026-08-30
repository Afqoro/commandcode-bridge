package main

// commandcode-bridge — OpenAI-compatible endpoint backed by Command Code's
// /alpha/generate gateway API (RE'd from CLI v1.38.2).
// Endpoints: /healthz, /v1/models, /v1/chat/completions (stream + non-stream).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	apiKey     = envOr("CMDC_API_KEY", "")
	baseURL    = envOr("CMDC_BASE_URL", "https://api.commandcode.ai")
	listenAddr = envOr("CMDC_LISTEN", "0.0.0.0:18766")
	cliVersion = envOr("CMDC_CLI_VERSION", "1.38.2")
	workDir    = envOr("CMDC_WORKDIR", "/home/agentuser")
)

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

// Model list from `cmd --list-models` (v1.38.2). accept_any_model: unknown ids pass through.
var modelList = []string{
	"deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "deepseek/deepseek-v4-flash-vision-exp",
	"moonshotai/kimi-k3", "moonshotai/kimi-k2.7-code", "moonshotai/kimi-k2.7-code-highspeed",
	"moonshotai/kimi-k2.6", "moonshotai/kimi-k2.5",
	"z-ai/glm-5.3-flash", "zai-org/glm-5.3", "zai-org/glm-5.2", "zai-org/glm-5.2-fast",
	"zai-org/glm-5.1", "zai-org/glm-5",
	"minimaxai/minimax-m3", "minimaxai/minimax-m2.7", "minimax/minimax-m3-free", "minimax/minimax-m2.7-free",
	"minimaxai/minimax-m2.5",
	"xiaomi/mimo-v2.5-pro", "xiaomi/mimo-v2.5",
	"qwen/qwen3.8-max", "qwen/qwen3.8-27b", "qwen/qwen3.8-flash", "qwen/qwen3.7-max",
	"qwen/qwen3.7-plus", "qwen/qwen3.7-flash", "qwen/qwen3.6-max-preview", "qwen/qwen3.6-plus",
	"stepfun/step-3.7-flash", "stepfun/step-3.5-flash",
	"tencent/hy3-paid", "tencent/hy4-preview",
	"nvidia/nemotron-3-ultra-550b-a55b",
	"thinkingmachines/inkling", "thinkingmachines/inkling-small",
	"poolside/laguna-s-2.1-free",
	"claude-sonnet-5", "claude-sonnet-4-6", "claude-fable-5", "claude-opus-5",
	"claude-opus-4-8", "claude-opus-4-7", "claude-haiku-4-5",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "gpt-5.5", "gpt-5.4", "gpt-5.3-codex", "gpt-5.4-mini",
	"google/gemini-3.7-flash", "google/gemini-3.6-flash", "google/gemini-3.5-flash",
	"google/gemini-3.5-flash-lite", "google/gemini-3.1-flash-lite",
	"sakana/fugu-ultra",
	"meta/muse-spark-1.1", "meta/muse-spark-1.2", "meta/muse-spark-1.2-contributor",
}

// ---------- upstream call ----------

func upstreamGenerate(ccReq *CCRequest) (*http.Response, error) {
	body, err := json.Marshal(ccReq)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", baseURL+"/alpha/generate", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "cli")
	req.Header.Set("x-command-code-version", cliVersion)
	req.Header.Set("x-cli-environment", "production")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		var eb CCErrorBody
		msg := string(b)
		if json.Unmarshal(b, &eb) == nil && eb.Error != nil {
			msg = eb.Error.Message
			if eb.Error.Code != "" {
				msg = eb.Error.Code + ": " + msg
			}
		}
		return nil, fmt.Errorf("upstream status %d: %s", resp.StatusCode, msg)
	}
	return resp, nil
}

// ---------- OpenAI response types ----------

type OAToolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

type OADelta struct {
	Role      string             `json:"role,omitempty"`
	Content   string             `json:"content,omitempty"`
	ToolCalls []OAToolCallDelta  `json:"tool_calls,omitempty"`
}

type OAChoice struct {
	Index        int     `json:"index"`
	Delta        OADelta `json:"delta,omitempty"`
	Message      *OADelta `json:"message,omitempty"`
	FinishReason *string `json:"finish_reason"`
}

type OAUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type OAChatResponse struct {
	ID      string      `json:"id"`
	Object  string      `json:"object"`
	Created int64       `json:"created"`
	Model   string      `json:"model"`
	Choices []OAChoice  `json:"choices"`
	Usage   *OAUsage    `json:"usage,omitempty"`
}

type OAErrDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

type OAErrorResponse struct {
	Error OAErrDetail `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeOAError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, OAErrorResponse{Error: OAErrDetail{Message: msg, Type: typ}})
}

// ---------- handlers ----------

func handleModels(w http.ResponseWriter, r *http.Request) {
	data := make([]map[string]interface{}, 0, len(modelList))
	for _, m := range modelList {
		data = append(data, map[string]interface{}{"id": m, "object": "model", "owned_by": "commandcode"})
	}
	writeJSON(w, 200, map[string]interface{}{"object": "list", "data": data})
}

func handleChat(w http.ResponseWriter, r *http.Request) {
	if apiKey == "" {
		writeOAError(w, 500, "server_error", "CMDC_API_KEY not configured")
		return
	}
	var req OARequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOAError(w, 400, "invalid_request_error", "bad JSON body: "+err.Error())
		return
	}
	ccReq, err := BuildCCRequest(&req, workDir)
	if err != nil {
		writeOAError(w, 400, "invalid_request_error", err.Error())
		return
	}
	resp, err := upstreamGenerate(ccReq)
	if err != nil {
		msg := err.Error()
		status := 502
		typ := "upstream_error"
		switch {
		case strings.Contains(msg, "insufficient credits"):
			status, typ = 402, "insufficient_credits"
		case strings.Contains(msg, "401") || strings.Contains(msg, "Unauthorized"):
			status, typ = 401, "authentication_error"
		case strings.Contains(msg, "429") || strings.Contains(msg, "rate"):
			status, typ = 429, "rate_limit_error"
		}
		writeOAError(w, status, typ, msg)
		return
	}
	defer resp.Body.Close()

	if req.Stream {
		streamSSE(w, resp.Body, req.Model)
	} else {
		bufferedRespond(w, resp.Body, req.Model)
	}
}

// acc accumulates one full completion from the NDJSON upstream stream.
type acc struct {
	mu           sync.Mutex
	content      strings.Builder
	reasoning    strings.Builder
	toolCalls    []OAToolCallDelta
	finish       string
	usage        *CCTotalUsage
}

func (a *acc) process(ev *CCStreamEvent) {
	switch ev.Type {
	case "text-delta":
		a.content.WriteString(ev.Text)
	case "reasoning-delta":
		a.reasoning.WriteString(ev.Text)
	case "tool-call":
		args := "{}"
		if len(ev.Input) > 0 {
			args = string(ev.Input)
		} else if len(ev.Args) > 0 {
			args = string(ev.Args)
		}
		a.toolCalls = append(a.toolCalls, OAToolCallDelta{
			Index: len(a.toolCalls), ID: ev.ToolCallID, Type: "function",
			// Function set below via named field workaround
		})
		a.toolCalls[len(a.toolCalls)-1].Function.Name = ev.ToolName
		a.toolCalls[len(a.toolCalls)-1].Function.Arguments = args
	case "finish":
		a.finish = MapCCFinishReason(ev.FinishReason)
		if ev.TotalUsage != nil {
			a.usage = ev.TotalUsage
		}
	}
}

func (a *acc) usageOA() *OAUsage {
	if a.usage == nil {
		return &OAUsage{}
	}
	u := &OAUsage{PromptTokens: a.usage.InputTokens, CompletionTokens: a.usage.OutputTokens}
	u.TotalTokens = u.PromptTokens + u.CompletionTokens
	return u
}

var idCounter int64

func newID() string {
	idCounter++
	return fmt.Sprintf("chatcmpl-cmdc%d", time.Now().UnixNano()%1e9)
}

// ---------- non-streaming ----------

func bufferedRespond(w http.ResponseWriter, body io.Reader, model string) {
	a := &acc{}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev CCStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type == "error" {
			writeOAError(w, 502, "upstream_error", "stream error: "+string(ev.Error))
			return
		}
		a.process(&ev)
	}
	msg := OADelta{Role: "assistant", Content: a.content.String()}
	if len(a.toolCalls) > 0 {
		msg.ToolCalls = a.toolCalls
	}
	fr := a.finish
	if fr == "" {
		fr = "stop"
	}
	writeJSON(w, 200, OAChatResponse{
		ID: newID(), Object: "chat.completion", Created: time.Now().Unix(), Model: model,
		Choices: []OAChoice{{Index: 0, Message: &msg, FinishReason: &fr}},
		Usage: a.usageOA(),
	})
}

// ---------- streaming ----------

func streamSSE(w http.ResponseWriter, body io.Reader, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOAError(w, 500, "server_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)

	send := func(v interface{}) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	id := newID()
	created := time.Now().Unix()
	base := OAChatResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: model}

	// initial role chunk
	role := base
	role.Choices = []OAChoice{{Index: 0, Delta: OADelta{Role: "assistant"}}}
	send(role)

	var errPayload string
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev CCStreamEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "text-delta":
			if ev.Text != "" {
				c := base
				c.Choices = []OAChoice{{Index: 0, Delta: OADelta{Content: ev.Text}}}
				send(c)
			}
		case "tool-call":
			args := "{}"
			if len(ev.Input) > 0 {
				args = string(ev.Input)
			} else if len(ev.Args) > 0 {
				args = string(ev.Args)
			}
			c := base
			tcd := OAToolCallDelta{Index: 0, ID: ev.ToolCallID, Type: "function"}
			tcd.Function.Name = ev.ToolName
			tcd.Function.Arguments = args
			c.Choices = []OAChoice{{Index: 0, Delta: OADelta{ToolCalls: []OAToolCallDelta{tcd}}}}
			send(c)
		case "error":
			errPayload = string(ev.Error)
		case "finish":
			fr := MapCCFinishReason(ev.FinishReason)
			c := base
			c.Choices = []OAChoice{{Index: 0, FinishReason: &fr}}
			if ev.TotalUsage != nil {
				u := OAUsage{PromptTokens: ev.TotalUsage.InputTokens, CompletionTokens: ev.TotalUsage.OutputTokens}
				u.TotalTokens = u.PromptTokens + u.CompletionTokens
				c.Usage = &u
			}
			send(c)
		}
	}
	if errPayload != "" {
		e := OAErrorResponse{Error: OAErrDetail{Message: "upstream stream error: " + errPayload, Type: "upstream_error"}}
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "data: %s\n\n", b)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func main() {
	if apiKey == "" {
		log.Fatal("CMDC_API_KEY env var is required")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "service": "commandcode-bridge"})
	})
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/v1/chat/completions", handleChat)
	log.Printf("commandcode-bridge listening on %s -> %s", listenAddr, baseURL)
	log.Fatal(http.ListenAndServe(listenAddr, mux))
}
