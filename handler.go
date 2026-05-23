package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

var upstreamURL string
var apiKey string
var debug bool

func debugLog(format string, args ...interface{}) {
	if debug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

// writeOllamaError 将上游错误转换为 Ollama 格式返回
func writeOllamaError(w http.ResponseWriter, statusCode int, upstreamErr string) {
	// 尝试从上游 JSON 错误中提取 message 字段
	var parsed struct {
		Message string `json:"message"`
	}
	if json.Unmarshal([]byte(upstreamErr), &parsed) == nil && parsed.Message != "" {
		upstreamErr = parsed.Message
	}
	ollamaErr := fmt.Sprintf(`{"error":"upstream error: %s"}`, escapeJSON(upstreamErr))
	debugLog("→ Ollama error response: %s", ollamaErr)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	w.Write([]byte(ollamaErr))
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

// Listener: POST /api/chat  →  POST /v1/chat/completions (upstream)
func handleOllamaChat(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	debugLog("← POST /api/chat  body:\n%s", prettyJSON(bodyBytes))

	var req OllamaChatRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeOllamaError(w, 400, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	openaiReq := OpenAIChatRequest{
		Model:    req.Model,
		Messages: make([]ChatMessage, len(req.Messages)),
		Stream:   req.Stream,
	}
	for i, m := range req.Messages {
		openaiReq.Messages[i] = ChatMessage{Role: m.Role, Content: m.Content}
	}
	if v, ok := req.Options["temperature"]; ok {
		if t, ok := v.(float64); ok {
			openaiReq.Temperature = t
		}
	}
	if v, ok := req.Options["top_p"]; ok {
		if t, ok := v.(float64); ok {
			openaiReq.TopP = t
		}
	}

	if req.Stream {
		doUpstreamChatStream(w, &openaiReq, req.Model)
	} else {
		doUpstreamChat(w, &openaiReq, req.Model)
	}
}

func doUpstreamChat(w http.ResponseWriter, openaiReq *OpenAIChatRequest, model string) {
	upstreamBody, _ := json.Marshal(openaiReq)
	debugLog("→ POST /v1/chat/completions  body:\n%s", prettyJSON(upstreamBody))

	openaiResp, err := callOpenAI("/v1/chat/completions", bytes.NewReader(upstreamBody))
	if err != nil {
		writeOllamaError(w, 502, fmt.Sprintf("upstream connection error: %v", err))
		return
	}
	defer openaiResp.Body.Close()

	respBytes, _ := io.ReadAll(openaiResp.Body)
	openaiResp.Body.Close()
	openaiResp.Body = io.NopCloser(bytes.NewReader(respBytes))
	debugLog("← upstream %s  status=%d  body:\n%s", "/v1/chat/completions", openaiResp.StatusCode, prettyJSON(respBytes))

	if openaiResp.StatusCode != 200 {
		writeOllamaError(w, openaiResp.StatusCode, string(respBytes))
		return
	}

	var chatResp OpenAIChatResponse
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		writeOllamaError(w, 500, fmt.Sprintf("parse upstream response: %v", err))
		return
	}

	ollamaResp := OllamaChatResponse{
		Model:     model,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Done:      true,
	}
	if len(chatResp.Choices) > 0 {
		if chatResp.Choices[0].Message != nil {
			ollamaResp.Message = OllamaMessage{
				Role:    chatResp.Choices[0].Message.Role,
				Content: chatResp.Choices[0].Message.Content,
			}
		}
		if chatResp.Choices[0].FinishReason != nil {
			ollamaResp.DoneReason = *chatResp.Choices[0].FinishReason
		}
	}
	if chatResp.Usage != nil {
		ollamaResp.EvalCount = chatResp.Usage.CompletionTokens
		ollamaResp.PromptEvalCount = chatResp.Usage.PromptTokens
	}

	out, _ := json.Marshal(ollamaResp)
	debugLog("→ Ollama response:\n%s", prettyJSON(out))

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func doUpstreamChatStream(w http.ResponseWriter, openaiReq *OpenAIChatRequest, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOllamaError(w, 500, "streaming not supported by client")
		return
	}

	upstreamBody, _ := json.Marshal(openaiReq)
	debugLog("→ POST /v1/chat/completions (stream)  body:\n%s", prettyJSON(upstreamBody))

	openaiResp, err := callOpenAI("/v1/chat/completions", bytes.NewReader(upstreamBody))
	if err != nil {
		writeOllamaError(w, 502, fmt.Sprintf("upstream connection error: %v", err))
		return
	}
	defer openaiResp.Body.Close()

	debugLog("← upstream stream status=%d", openaiResp.StatusCode)
	if openaiResp.StatusCode != 200 {
		respBytes, _ := io.ReadAll(openaiResp.Body)
		writeOllamaError(w, openaiResp.StatusCode, string(respBytes))
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	scanner := bufio.NewScanner(openaiResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			debugLog("  skippable upstream line: %s", line)
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			debugLog("  upstream [DONE]")
			break
		}

		debugLog("  upstream chunk: %s", data)

		var chunk OpenAIChatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			debugLog("  parse error: %v", err)
			continue
		}

		ollamaChunk := OllamaChatStreamResponse{
			Model:     model,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Message:   OllamaMessage{},
		}

		if len(chunk.Choices) > 0 {
			if chunk.Choices[0].Delta != nil {
				ollamaChunk.Message.Role = chunk.Choices[0].Delta.Role
				ollamaChunk.Message.Content = chunk.Choices[0].Delta.Content
			}
			if chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
				ollamaChunk.Done = true
			}
		}

		dataOut, _ := json.Marshal(ollamaChunk)
		debugLog("  → Ollama chunk: %s", string(dataOut))
		fmt.Fprintln(w, string(dataOut))
		flusher.Flush()

		if ollamaChunk.Done {
			break
		}
	}
}

// Listener: POST /api/generate → prompt → POST /v1/chat/completions (upstream)
// Ollama's /api/generate maps to chat completions because most modern
// APIs only support /v1/chat/completions, not legacy /v1/completions.
func handleOllamaGenerate(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	debugLog("← POST /api/generate  body:\n%s", prettyJSON(bodyBytes))

	var req OllamaGenerateRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeOllamaError(w, 400, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	// Wrap prompt as a single user message for chat completions
	chatReq := OpenAIChatRequest{
		Model:    req.Model,
		Messages: []ChatMessage{{Role: "user", Content: req.Prompt}},
		Stream:   req.Stream,
	}
	if v, ok := req.Options["temperature"]; ok {
		if t, ok := v.(float64); ok {
			chatReq.Temperature = t
		}
	}
	if v, ok := req.Options["top_p"]; ok {
		if t, ok := v.(float64); ok {
			chatReq.TopP = t
		}
	}

	if req.Stream {
		doUpstreamGenerateStream(w, &chatReq, req.Model)
	} else {
		doUpstreamGenerate(w, &chatReq, req.Model)
	}
}

func doUpstreamGenerate(w http.ResponseWriter, chatReq *OpenAIChatRequest, model string) {
	upstreamBody, _ := json.Marshal(chatReq)
	debugLog("→ POST /v1/chat/completions (from /api/generate)  body:\n%s", prettyJSON(upstreamBody))

	openaiResp, err := callOpenAI("/v1/chat/completions", bytes.NewReader(upstreamBody))
	if err != nil {
		writeOllamaError(w, 502, fmt.Sprintf("upstream connection error: %v", err))
		return
	}
	defer openaiResp.Body.Close()

	respBytes, _ := io.ReadAll(openaiResp.Body)
	openaiResp.Body.Close()
	openaiResp.Body = io.NopCloser(bytes.NewReader(respBytes))
	debugLog("← upstream %s  status=%d  body:\n%s", "/v1/chat/completions", openaiResp.StatusCode, prettyJSON(respBytes))

	if openaiResp.StatusCode != 200 {
		writeOllamaError(w, openaiResp.StatusCode, string(respBytes))
		return
	}

	var chatResp OpenAIChatResponse
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		writeOllamaError(w, 500, fmt.Sprintf("parse upstream response: %v", err))
		return
	}

	text := ""
	if len(chatResp.Choices) > 0 && chatResp.Choices[0].Message != nil {
		text = chatResp.Choices[0].Message.Content
	}

	ollamaResp := OllamaGenerateResponse{
		Model:     model,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Response:  text,
		Done:      true,
	}
	if chatResp.Usage != nil {
		ollamaResp.EvalCount = chatResp.Usage.CompletionTokens
		ollamaResp.PromptEvalCount = chatResp.Usage.PromptTokens
	}

	out, _ := json.Marshal(ollamaResp)
	debugLog("→ Ollama response:\n%s", prettyJSON(out))

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

func doUpstreamGenerateStream(w http.ResponseWriter, chatReq *OpenAIChatRequest, model string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOllamaError(w, 500, "streaming not supported by client")
		return
	}

	upstreamBody, _ := json.Marshal(chatReq)
	debugLog("→ POST /v1/chat/completions (stream, from /api/generate)  body:\n%s", prettyJSON(upstreamBody))

	openaiResp, err := callOpenAI("/v1/chat/completions", bytes.NewReader(upstreamBody))
	if err != nil {
		writeOllamaError(w, 502, fmt.Sprintf("upstream connection error: %v", err))
		return
	}
	defer openaiResp.Body.Close()

	debugLog("← upstream stream status=%d", openaiResp.StatusCode)
	if openaiResp.StatusCode != 200 {
		respBytes, _ := io.ReadAll(openaiResp.Body)
		writeOllamaError(w, openaiResp.StatusCode, string(respBytes))
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-cache")

	scanner := bufio.NewScanner(openaiResp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			debugLog("  skippable upstream line: %s", line)
			continue
		}
		data := strings.TrimPrefix(line, "data: ")
		if data == "[DONE]" {
			debugLog("  upstream [DONE]")
			break
		}

		debugLog("  upstream chunk: %s", data)

		var chunk OpenAIChatResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			debugLog("  parse error: %v", err)
			continue
		}

		content := ""
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta != nil {
			content = chunk.Choices[0].Delta.Content
		}

		ollamaChunk := OllamaGenerateResponse{
			Model:     model,
			CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Response:  content,
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil && *chunk.Choices[0].FinishReason != "" {
			ollamaChunk.Done = true
		}

		dataOut, _ := json.Marshal(ollamaChunk)
		debugLog("  → Ollama chunk: %s", string(dataOut))
		fmt.Fprintln(w, string(dataOut))
		flusher.Flush()

		if ollamaChunk.Done {
			break
		}
	}
}

// Listener: POST /api/embed  →  POST /v1/embeddings (upstream)
func handleOllamaEmbed(w http.ResponseWriter, r *http.Request) {
	bodyBytes, _ := io.ReadAll(r.Body)
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	debugLog("← POST /api/embed  body:\n%s", prettyJSON(bodyBytes))

	var req OllamaEmbedRequest
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		writeOllamaError(w, 400, fmt.Sprintf("invalid request body: %v", err))
		return
	}

	openaiReq := OpenAIEmbedRequest{
		Model: req.Model,
		Input: req.Input,
	}

	upstreamBody, _ := json.Marshal(openaiReq)
	debugLog("→ POST /v1/embeddings  body:\n%s", prettyJSON(upstreamBody))

	openaiResp, err := callOpenAI("/v1/embeddings", bytes.NewReader(upstreamBody))
	if err != nil {
		writeOllamaError(w, 502, fmt.Sprintf("upstream connection error: %v", err))
		return
	}
	defer openaiResp.Body.Close()

	respBytes, _ := io.ReadAll(openaiResp.Body)
	openaiResp.Body.Close()
	openaiResp.Body = io.NopCloser(bytes.NewReader(respBytes))
	debugLog("← upstream %s  status=%d  body:\n%s", "/v1/embeddings", openaiResp.StatusCode, prettyJSON(respBytes))

	if openaiResp.StatusCode != 200 {
		writeOllamaError(w, openaiResp.StatusCode, string(respBytes))
		return
	}

	var embResp OpenAIEmbedResponse
	if err := json.Unmarshal(respBytes, &embResp); err != nil {
		writeOllamaError(w, 500, fmt.Sprintf("parse upstream response: %v", err))
		return
	}

	ollamaResp := OllamaEmbedResponse{
		Embeddings: make([][]float64, len(embResp.Data)),
	}
	for i, d := range embResp.Data {
		ollamaResp.Embeddings[i] = d.Embedding
	}

	out, _ := json.Marshal(ollamaResp)
	debugLog("→ Ollama response:\n%s", prettyJSON(out))

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// Listener: GET /api/tags  →  GET /v1/models (upstream)
func handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	debugLog("← GET /api/tags")
	debugLog("→ GET %s/v1/models", upstreamURL)

	httpReq, err := http.NewRequest("GET", upstreamURL+"/v1/models", nil)
	if err != nil {
		writeOllamaError(w, 500, fmt.Sprintf("internal error: %v", err))
		return
	}
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	if debug {
		dump, _ := httputil.DumpRequestOut(httpReq, false)
		debugLog("  request:\n%s", string(dump))
	}

	client := &http.Client{Timeout: 30 * time.Second}
	openaiResp, err := client.Do(httpReq)
	if err != nil {
		writeOllamaError(w, 502, fmt.Sprintf("upstream connection error: %v", err))
		return
	}
	defer openaiResp.Body.Close()

	respBytes, _ := io.ReadAll(openaiResp.Body)
	debugLog("← upstream status=%d  body:\n%s", openaiResp.StatusCode, prettyJSON(respBytes))

	if openaiResp.StatusCode != 200 {
		writeOllamaError(w, openaiResp.StatusCode, string(respBytes))
		return
	}

	var modelResp OpenAIModelListResponse
	if err := json.Unmarshal(respBytes, &modelResp); err != nil {
		writeOllamaError(w, 500, fmt.Sprintf("parse upstream response: %v", err))
		return
	}

	ollamaResp := OllamaTagsResponse{
		Models: make([]OllamaModel, len(modelResp.Data)),
	}
	for i, d := range modelResp.Data {
		ollamaResp.Models[i] = OllamaModel{
			Name:       d.ID,
			ModifiedAt: time.Unix(d.Created, 0).UTC().Format(time.RFC3339),
			Size:       0,
		}
	}

	out, _ := json.Marshal(ollamaResp)
	debugLog("→ Ollama response:\n%s", prettyJSON(out))

	w.Header().Set("Content-Type", "application/json")
	w.Write(out)
}

// callOpenAI sends a POST JSON request to the upstream OpenAI-compatible API.
func callOpenAI(path string, body io.Reader) (*http.Response, error) {
	httpReq, err := http.NewRequest("POST", upstreamURL+path, body)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	return client.Do(httpReq)
}

func prettyJSON(data []byte) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, data, "  ", "  "); err != nil {
		return string(data)
	}
	return buf.String()
}
