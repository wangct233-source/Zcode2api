// Package server HTTP 服务与管理面板 API。
//
// 面板安全（相对原型的改进点）：
//   - 移除「admin 兜底键」：未设置 panelPassword 时面板完全锁定，
//     提示通过 ZG_PANEL_PASSWORD 或 config.yaml 设置；
//   - 会话仅内存（重启即失效）；HttpOnly + SameSite=Strict Cookie；
//   - 登录失败限速：8 次 / 15 分钟；
//   - 写操作要求自定义头 X-Requested-With: XMLHttpRequest（配合 Strict 抵御 CSRF）。
package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/wangct233-source/Zcode2api/internal/config"
	"github.com/wangct233-source/Zcode2api/internal/pool"
	"github.com/wangct233-source/Zcode2api/internal/proxy"
	"github.com/wangct233-source/Zcode2api/internal/store"
	"github.com/wangct233-source/Zcode2api/internal/updater"
	"github.com/wangct233-source/Zcode2api/internal/web"
)

// Server 聚合所有子系统。
type Server struct {
	Cfg     *config.Config
	Store   *store.Store
	Pool    *pool.Pool
	Gateway *proxy.Gateway
	Version string
	ConfigPath string

	mu      sync.Mutex
	sessions map[string]time.Time // token → 过期时间
	loginFails []time.Time       // 登录失败时间窗
}

// Handler 组装全部路由。
func (s *Server) Handler() http.Handler {
	s.mu.Lock()
	if s.sessions == nil {
		s.sessions = map[string]time.Time{}
	}
	if s.loginFails == nil {
		s.loginFails = []time.Time{}
	}
	s.mu.Unlock()

	mux := http.NewServeMux()

	// 代理 API（OpenAI 格式）与健康检查。
	mux.Handle("/v1/", s.Gateway.Handler())
	mux.HandleFunc("/health", s.Gateway.Handler().ServeHTTP)

	// 内嵌静态资源与面板。
	mux.Handle("GET /static/", web.StaticHandler())
	mux.HandleFunc("GET /admin", s.pageAdmin)
	mux.HandleFunc("GET /admin/", s.pageAdmin)

	// 管理 API。
	mux.HandleFunc("POST /admin/api/login", s.apiLogin)
	mux.HandleFunc("POST /admin/api/logout", s.auth(s.apiLogout))
	mux.HandleFunc("GET /admin/api/state", s.auth(s.apiState))
	mux.HandleFunc("POST /admin/api/accounts", s.auth(s.apiAddAccount))
	mux.HandleFunc("DELETE /admin/api/accounts/{id}", s.auth(s.apiDelAccount))
	mux.HandleFunc("POST /admin/api/config", s.auth(s.apiSaveConfig))
	mux.HandleFunc("POST /admin/api/update/check", s.auth(s.apiUpdateCheck))
	mux.HandleFunc("POST /admin/api/update/apply", s.auth(s.apiUpdateApply))

	return s.logMiddleware(mux)
}

// ---- 会话与鉴权 ----

const sessionTTL = 30 * time.Minute

func (s *Server) newSession() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[tok] = time.Now().Add(sessionTTL)
	s.mu.Unlock()
	return tok
}

func (s *Server) validSession(r *http.Request) bool {
	c, err := r.Cookie("zg_panel")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok || time.Now().After(exp) {
		delete(s.sessions, c.Value)
		return false
	}
	s.sessions[c.Value] = time.Now().Add(sessionTTL) // 滑动续期
	return true
}

// auth 包一层会话校验 + CSRF 头检查。
func (s *Server) auth(fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			if r.Header.Get("X-Requested-With") != "XMLHttpRequest" {
				http.Error(w, "缺少 CSRF 头", 403)
				return
			}
		}
		if !s.validSession(r) {
			http.Error(w, "未登录或会话过期", 401)
			return
		}
		fn(w, r)
	}
}

// apiLogin 面板登录。
func (s *Server) apiLogin(w http.ResponseWriter, r *http.Request) {
	// 未设置密码 = 面板锁定（无任何兜底键）。
	want := s.Cfg.Auth.PanelPassword
	if want == "" {
		http.Error(w, "面板未设置密码，已被锁定。请设置环境变量 ZG_PANEL_PASSWORD 或编辑 config.yaml 的 auth.panelPassword", 403)
		return
	}
	// 登录限速：8 次 / 15 分钟。
	now := time.Now()
	s.mu.Lock()
	keep := s.loginFails[:0]
	for _, t := range s.loginFails {
		if now.Sub(t) < 15*time.Minute {
			keep = append(keep, t)
		}
	}
	s.loginFails = keep
	if len(s.loginFails) >= 8 {
		s.mu.Unlock()
		http.Error(w, "登录失败次数过多，请 15 分钟后再试", 429)
		return
	}
	s.mu.Unlock()

	var req struct{ Password string `json:"password"` }
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || !constTimeEqual(req.Password, want) {
		s.mu.Lock()
		s.loginFails = append(s.loginFails, now)
		s.mu.Unlock()
		http.Error(w, "密码错误", 401)
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name: "zg_panel", Value: s.newSession(), Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) apiLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("zg_panel"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiState 面板首页数据：账号、统计、版本、配置摘要。
func (s *Server) apiState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"version":   s.Version,
		"accounts":  s.Pool.Snapshot(),
		"inFlight":  s.Gateway.InFlight(),
		"models":    s.Cfg.Models,
		"provider":  s.Cfg.Provider.Name,
		"upstream":  s.Cfg.Provider.OpenAIBase,
		"panelLocked": s.Cfg.Auth.PanelPassword == "",
		"updater":   map[string]any{"enabled": s.Cfg.Updater.Enabled, "repo": s.Cfg.Updater.Repo},
		"now":       time.Now().Format("2006-01-02 15:04:05"),
	})
}

// apiAddAccount 添加账号（手动粘贴 API Key 或 JWT）。
func (s *Server) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		Secret string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Secret == "" {
		http.Error(w, "参数不完整", 400)
		return
	}
	if req.Kind != "api_key" && req.Kind != "jwt" {
		req.Kind = "api_key"
	}
	if err := s.Store.Add(store.AccountRecord{Name: req.Name, Kind: req.Kind, Secret: req.Secret}); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiDelAccount 删除账号。
func (s *Server) apiDelAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Store.Remove(id); err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiSaveConfig 保存面板可改配置并热生效。
func (s *Server) apiSaveConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyAPIKey *string `json:"proxyApiKey"`
		MinSpacingMs *int64 `json:"minSpacingMs"`
		MaxConcurrentPerAccount *int `json:"maxConcurrentPerAccount"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "参数错误", 400)
		return
	}
	if req.ProxyAPIKey != nil {
		s.Cfg.Auth.ProxyAPIKey = *req.ProxyAPIKey
	}
	if req.MinSpacingMs != nil {
		s.Cfg.Pool.MinSpacingMs = *req.MinSpacingMs
	}
	if req.MaxConcurrentPerAccount != nil && *req.MaxConcurrentPerAccount > 0 {
		s.Cfg.Pool.MaxConcurrentPerAccount = *req.MaxConcurrentPerAccount
	}
	s.Pool.UpdateConfig(s.Cfg.Pool)
	if err := config.Save(s.ConfigPath, s.Cfg); err != nil {
		http.Error(w, "写回配置失败: "+err.Error(), 500)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// apiUpdateCheck 检查更新。
func (s *Server) apiUpdateCheck(w http.ResponseWriter, r *http.Request) {
	rel, err := updater.CheckLatest(s.Cfg.Updater.Repo)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if rel == nil {
		writeJSON(w, map[string]any{"ok": true, "latest": false, "version": s.Version})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "latest": true, "version": rel.TagName})
}

// apiUpdateApply 应用更新（下载→校验→替换二进制，需重启容器/进程生效）。
func (s *Server) apiUpdateApply(w http.ResponseWriter, r *http.Request) {
	rel, err := updater.CheckLatest(s.Cfg.Updater.Repo)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	if rel == nil {
		writeJSON(w, map[string]any{"ok": true, "updated": false, "message": "已是最新版本"})
		return
	}
	ver, err := updater.Apply(rel, nil)
	if err != nil {
		writeJSON(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, map[string]any{"ok": true, "updated": true, "version": ver,
		"message": "新版本已就位，请重启容器完成升级"})
}

// pageAdmin 返回内嵌的管理面板页面。
func (s *Server) pageAdmin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/admin" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(web.PanelHTML())
}

// logMiddleware 简单访问日志。
func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if r.URL.Path != "/health" {
			fmt.Printf("%s %s %s\n", time.Now().Format("15:04:05"), r.Method, r.URL.Path)
		}
		_ = start
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func constTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}
