# mcp-sse-bridge

一个小型 Go 桥接程序，将 MCP JSON-RPC 的 stdio 流量转发到上游 SSE 服务。它会
短路客户端的 `initialize` 以避免启动超时，同时把请求转发到上游，并在会话失效时自动重连。

## 功能
- 支持 stdio JSON-RPC（jsonl 或 `Content-Length`）转发到 legacy SSE。
- 上游 Session 失效时自动重连。
- 以最佳努力方式转发并等待上游 `initialize` 完成。

## 环境要求
- Go 1.25+（见 `go.mod`）。
- 可访问的上游 SSE 地址。

## 构建
```bash
go build -o mcp-sse-bridge .
```

## 运行
```bash
REMOTE_SSE_URL="https://host/mcp-servers/example" \
REMOTE_BEARER_TOKEN="..." \
./mcp-sse-bridge
```

## 冒烟测试
```bash
REMOTE_SSE_URL="https://host/mcp-servers/example" \
REMOTE_BEARER_TOKEN="..." \
./scripts/smoke.sh
```

## 配置
必填：
- `REMOTE_SSE_URL`：上游 SSE 地址，需要发送 `endpoint` 和 `message` 事件。

可选：
- `REMOTE_BEARER_TOKEN`：上游鉴权 Bearer Token。
- `UPSTREAM_TIMEOUT`：JSON-RPC 等待超时（默认 120s）。
- `UPSTREAM_INIT_TIMEOUT`：等待上游 `initialize` 超时（默认 8s）。
- `FORWARD_INITIALIZE`：是否转发客户端 `initialize` 到上游（默认 false）。
- `RECONNECT_POST_DELAY_MS`：重连后重试前的等待毫秒数（默认 300）。
- `ONE_SHOT_TOOLS`：每次 `tools/call` 使用独立 SSE 会话（默认 true）。
- `ONE_SHOT_MAX_RETRIES`：one-shot 最大尝试次数（默认 3）。
- `LOCAL_TOOLS_LIST`：是否返回本地硬编码的 `tools/list`（默认 false）。

## 说明
- stderr 输出日志，stdout 输出 JSON-RPC。
- 重连会切换到新的 SSE 通道，并补发上游 initialize。
- `LOCAL_TOOLS_LIST=false` 时，`tools/list` 会转发到上游。

## 仓库结构
- `main.go`：入口。
- `bridge/`：stdio 循环与上游协调。
- `sse/`：SSE 连接与会话管理。
- `rpc/`：JSON-RPC 类型与 stdio framing。
- `go.mod`：模块定义。
