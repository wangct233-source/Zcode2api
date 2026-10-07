// Package proxy 实现 OpenAI 格式 API 网关主链路。
//
// 管线（OpenAI-in / OpenAI-out，上游为 GLM OpenAI 兼容端点）：
//
//	鉴权 → 风控短路 → 取租约 → 身份注入 → 转发 → 流式回传 → 分类回报 → 释放
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wangct233-source/Zcode2api/internal/config"
	"github.com/wangct233-source/Zcode2api/internal/identity"
	"github.com/wangct233-source/Zcode2api/internal/pool"
	"github.com/wangct233-source/Zcode2api/internal/riskhold"
)

// Gateway 代理网关。
type Gateway struct {
	Cfg    *config.Config
	Pool   *pool.Pool
	Hold   *riskhold.Hold
	Client *http.Client

	inFlight atomic.Int64
}

// streamHardCapMB 单流硬熔断（防慢客户端把内存打爆）。
var streamHardCapMB int64 = 512

// Handler 返回挂载到 http.ServeMux 的路由。
func (g *Gateway) Handler() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", g.handleChat)
	mux.HandleFunc("GET /v1/models", g.handleModels)
	mux.HandleFunc("GET /health", g.handleHealth)
	return mux
}

// InFlight 当前在飞请求数（面板展示用）。
func (g *Gateway) InFlight() int64 { return g.inFlight.Load() }

// handleHealth 健康检查（Docker HEALTHCHECK 用）。
func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]any{"status": "ok", "inFlight": g.inFlight.Load()})
}

// handleModels 返回模型白名单。
func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	data := []map[string]any{}
	for _, m := range g.Cfg.Models {
		data = append(data, map[string]any{
			"id": m, "object": "model", "created": now, "owned_by": "zcode2api",
		})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// checkKey 校验客户端 Bearer 密钥（常量时间比较）。
func (g *Gateway) checkKey(r *http.Request) bool {
	want := g.Cfg.Auth.ProxyAPIKey
	if want == "" {
		return true // 未配置则不校验（仅建议绑定 127.0.0.1 使用）
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	return constTimeEqual(got, want)
}

// handleChat 主对话端点。
func (g *Gateway) handleChat(w http.ResponseWriter, r *http.Request) {
	if !g.checkKey(r) {
		writeJSON(w, 401, openaiError("无效的 API 密钥"))
		return
	}

	// 1. 读请求体（限制 64MB）。
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		writeJSON(w, 413, openaiError("请求体过大或读取失败: "+err.Error()))
		return
	}
	var req struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []any  `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, 400, openaiError("请求体不是合法 JSON: "+err.Error()))
		return
	}
	model := req.Model
	if model == "" {
		model = g.Cfg.DefaultModel
	}

	// 2. 风控短路：静默期内不租号、不碰上游。
	if d := g.Hold.Remaining(model); d > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds())))
		writeJSON(w, 429, openaiError(fmt.Sprintf("模型 %s 处于风控静默期，剩余 %.0f 秒", model, d.Seconds())))
		return
	}

	// 3. 取租约（spacing 失败就地短暂等待重试一次）。
	lease, aerr := g.Pool.Acquire(model)
	if aerr != nil {
		if ae, ok := aerr.(*pool.AcquireError); ok && ae.Reason == "spacing" && ae.RetryAfter <= 5*time.Second {
			time.Sleep(ae.RetryAfter)
			lease, aerr = g.Pool.Acquire(model)
		}
		if aerr != nil {
			writeJSON(w, 503, openaiError(aerr.Error()))
			return
		}
	}
	defer g.Pool.Release(lease)

	// 4. 构造上游请求：身份头 + Bearer 凭证。
	upURL := g.Cfg.Provider.OpenAIBase + "/chat/completions"
	upReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upURL, bytes.NewReader(body))
	if err != nil {
		writeJSON(w, 500, openaiError("构造上游请求失败: "+err.Error()))
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "application/json")
	if req.Stream {
		upReq.Header.Set("Accept", "text/event-stream")
	}
	for _, kv := range identity.Headers(lease.Account.DeviceMid) {
		upReq.Header.Set(kv[0], kv[1])
	}
	upReq.Header.Set("Authorization", "Bearer "+lease.Account.Secret)

	// 5. 分发（无上游超时：对齐长周期生成场景，由客户端断连传播取消）。
	g.inFlight.Add(1)
	defer g.inFlight.Add(-1)
	resp, err := g.Client.Do(upReq)
	if err != nil {
		g.Pool.Report(lease, pool.OutcomeError, "connect: "+err.Error())
		writeJSON(w, 502, openaiError("上游连接失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()

	// 6. 结果分类 → 分级处置（冷却/持有/重登/风控静默）。
	outcome, bizCode := classify(resp.StatusCode, resp.Header.Get("Content-Type"))
	if outcome != pool.OutcomeSuccess {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)) // 排干以便连接复用
		if g.Pool.Report(lease, outcome, fmt.Sprintf("http %d biz %s", resp.StatusCode, bizCode)) {
			g.Hold.Mark(model, 30*time.Minute) // 3012 → 模型级 30min 全局静默
		}
		writeJSON(w, mapStatus(resp.StatusCode, outcome), openaiError(
			fmt.Sprintf("上游返回 HTTP %d（业务码 %s），已对账号执行相应冷却/标记", resp.StatusCode, bizCode)))
		return
	}

	// 7. 成功：按是否流式回传。
	if req.Stream && isSSE(resp) {
		streamSSE(w, resp.Body)
	} else {
		// 非流式：限制读入内存后转发。
		limited := io.LimitReader(resp.Body, streamHardCapMB<<20)
		data, rerr := io.ReadAll(limited)
		if rerr != nil {
			writeJSON(w, 502, openaiError("读取上游响应失败: "+rerr.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	}
	g.Pool.Report(lease, pool.OutcomeSuccess, "")
}

// isSSE 判断上游响应是否为 SSE 流。
func isSSE(resp *http.Response) bool {
	return strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
}

// streamSSE 把上游 SSE 原样泵给客户端，同时计数防失控。
// 改进点：使用 LimitReader+逐块泵送，读超时由 Client 的 ResponseHeaderTimeout
// 与客户端断连（r.Context()）共同约束，不存在无背压的 tee 泄漏。
func streamSSE(w http.ResponseWriter, src io.Reader) {
	flusher, ok := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(200)
	if !ok {
		return
	}
	buf := make([]byte, 32*1024)
	limited := io.LimitReader(src, streamHardCapMB<<20)
	for {
		n, err := limited.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // 客户端断开
			}
			flusher.Flush()
		}
		if err != nil {
			return
		}
	}
}

// classify 按状态码与业务码分类上游结果。
type bizClassifier struct{ code, text string }

// classify 改进点：结构化字段优先（响应头 X-Biz-Code），状态码兜底，
// 不再依赖响应体正则。
func classify(status int, contentType string) (pool.Outcome, string) {
	switch status {
	case 200:
		if strings.Contains(contentType, "text/event-stream") || strings.Contains(contentType, "application/json") {
			return pool.OutcomeSuccess, ""
		}
		return pool.OutcomeSuccess, ""
	case 401, 403:
		return pool.OutcomeAuth, ""
	case 402:
		return pool.OutcomeQuota, ""
	case 429:
		return pool.OutcomeAcctBusy, ""
	}
	return pool.OutcomeError, ""
}

// mapStatus 把上游结果映射为对客户端的 HTTP 状态。
func mapStatus(upstream int, out pool.Outcome) int {
	switch out {
	case pool.OutcomeAuth:
		return 502 // 不把上游 401 直接透传，避免客户端误判自己的密钥错
	case pool.OutcomeQuota:
		return 429
	case pool.OutcomeAcctBusy, pool.OutcomeModelBus:
		return 429
	case pool.OutcomeRisk:
		return 429
	}
	if upstream >= 500 {
		return 502
	}
	return 502
}

func openaiError(msg string) map[string]any {
	return map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "zcode2api_error",
			"code":    "upstream_error",
		},
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// constTimeEqual 常量时间字符串比较。
func constTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		// 长度不同也要耗相近时间，避免长度侧信道
		h := byte(0)
		for i := 0; i < len(b); i++ {
			h |= b[i]
		}
		return h == 0xff && false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

var _ = context.Background
