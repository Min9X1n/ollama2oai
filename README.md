# ollama2oai
一个轻量级的 Go 语言代理服务器，将 Ollama API 请求转换为 OpenAI 兼容 API 格式。制作它的最大原因是为了使用Citespace中的Ollama功能。

A lightweight Go-based proxy server that converts Ollama API requests into an OpenAI-compatible API format. The main reason for making it was to use Ollama features in Citespace.

## 功能

将 Ollama 客户端的请求无缝转发到 OpenAI 兼容的后端 API：

Seamlessly forward requests from Ollama clients to OpenAI-compatible backend APIs:

| Ollama API | OpenAI API | 说明 | Description |
|------------|------------|------|-------------|
| `POST /api/chat` | `POST /v1/chat/completions` | 聊天补全 | Chat completion |
| `POST /api/generate` | `POST /v1/chat/completions` | 文本生成（prompt 包装为 user 消息）| Text generation (prompt wrapped as user message) |
| `POST /api/embed` | `POST /v1/embeddings` | 文本嵌入 | Text embeddings |
| `GET /api/tags` | `GET /v1/models` | 获取模型列表 | List available models |

## 安装 | Installation

```bash
go build -o ollama2oai
```

## 使用 | Usage

```bash
./ollama2oai -upstream http://localhost:8080 -port 11435
```

### 参数 | Parameters

| 参数 | 默认值 | 说明 | Parameter | Default | Description |
|------|--------|------|-----------|---------|-------------|
| `-port` | `11435` | 监听端口（模拟 Ollama 服务）| `-port` | `11435` | Listening port (simulates Ollama service) |
| `-upstream` | `http://localhost:8080` | 上游 OpenAI 兼容 API 地址 | `-upstream` | `http://localhost:8080` | Upstream OpenAI-compatible API address |
| `-api-key` | (空) | 上游 API 密钥（可选）| `-api-key` | (empty) | Upstream API key (optional) |
| `-debug` | `false` | 启用调试日志 | `-debug` | `false` | Enable debug logging |

## 示例 | Examples

### 1. 对接本地 vLLM | Connect to local vLLM

```bash
./ollama2oai -upstream http://localhost:11434
```

### 2. 对接 OpenAI API | Connect to OpenAI API
注意：无需添加“v1”
```bash
./ollama2oai -upstream https://api.openai.com -api-key sk-your-key
```

## 工作原理 | How It Works

1. 监听本地端口，模拟 Ollama API 服务
2. 接收 Ollama 格式的请求
3. 转换为 OpenAI API 格式
4. 转发到上游服务
5. 将响应转换回 Ollama 格式返回
---
1. Listen on a local port, simulating the Ollama API service
2. Receive requests in Ollama format
3. Convert to OpenAI API format
4. Forward to the upstream service
5. Convert the response back to Ollama format and return
