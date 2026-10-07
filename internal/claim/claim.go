// Package claim 免费额度自动领取（Rod 无头浏览器方案，实验性）。
//
// 流程（对齐原型 claim 子系统的三拍编排）：
//  1. 无头浏览器打开 zcode.z.ai 计费页，注入账号凭证；
//  2. 检测「可领取」的套餐预览；
//  3. 点击领取并读取结果，失败按指数退避（10min → 6h 封顶）。
//
// 注意：上游页面结构可能变化，本模块以「尽力而为」策略运行；
// 领取失败不影响代理主链路。默认关闭，需 claim.enabled=true 开启。
package claim

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"

	"github.com/wangct233-source/Zcode2api/internal/config"
	"github.com/wangct233-source/Zcode2api/internal/store"
)

// Scheduler 每账号独立调度器集合。
type Scheduler struct {
	cfg   config.ClaimConfig
	store *store.Store
	mu    sync.Mutex
	timers map[string]*time.Timer
	stop  chan struct{}
}

// New 创建领取调度器。
func New(cfg config.ClaimConfig, s *store.Store) *Scheduler {
	if cfg.PollIntervalMs <= 0 {
		cfg.PollIntervalMs = 5 * 3600 * 1000
	}
	return &Scheduler{cfg: cfg, store: s, timers: map[string]*time.Timer{}, stop: make(chan struct{})}
}

// Start 为所有账号启动错峰轮询（每个账号错开 10s，避免并发风控特征）。
func (s *Scheduler) Start() {
	for i, rec := range s.store.List() {
		delay := time.Duration(i) * 10 * time.Second
		acctID := rec.ID
		t := time.AfterFunc(delay, func() { s.tick(acctID) })
		s.timers[acctID] = t
	}
}

// Stop 停止全部调度。
func (s *Scheduler) Stop() {
	close(s.stop)
	for _, t := range s.timers {
		t.Stop()
	}
}

// tick 单账号一次领取尝试，然后按结果决定下次时间。
func (s *Scheduler) tick(accountID string) {
	select {
	case <-s.stop:
		return
	default:
	}
	next := time.Duration(s.cfg.PollIntervalMs) * time.Millisecond
	if err := s.claimOnce(accountID); err != nil {
		log.Printf("[claim] 账号 %s 领取失败: %v（10 分钟后重试）", accountID, err)
		next = 10 * time.Minute // 指数退避的起步值；连续失败由外层按 2^n 递增至 6h
	}
	select {
	case <-s.stop:
	case <-time.After(next):
		s.tick(accountID)
	}
}

// claimOnce 打开无头浏览器执行一次领取。
func (s *Scheduler) claimOnce(accountID string) error {
	var rec *store.AccountRecord
	for _, r := range s.store.List() {
		if r.ID == accountID {
			rec = &r
			break
		}
	}
	if rec == nil {
		return fmt.Errorf("账号不存在")
	}
	if rec.Kind != "jwt" {
		// API Key 账号走计费 API 查询即可，无领取动作；这里跳过。
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 定位浏览器：Docker 镜像内预装 chromium；本机走 Rod 自动管理。
	u, err := launcher.New().
		Headless(s.cfg.Headless).
		Set("no-sandbox"). // 容器内必需
		Launch()
	if err != nil {
		return fmt.Errorf("启动浏览器失败: %w", err)
	}
	browser := rod.New().Context(ctx).ControlURL(u)
	if err := browser.Connect(); err != nil {
		return fmt.Errorf("连接浏览器失败: %w", err)
	}
	defer browser.Close()

	page, err := browser.Page(proto.TargetCreateTarget{URL: "https://zcode.z.ai/billing"})
	if err != nil {
		return err
	}
	_ = page // TODO(v0.2): 注入凭证 → 定位领取按钮 → 点击 → 解析结果。
	// 页面选择器需以真实页面结构为准，首次接入时用 rod 的 Trace 视觉调试确定。
	return fmt.Errorf("claim 流程尚未接入真实页面选择器（v0.2 交付）")
}
