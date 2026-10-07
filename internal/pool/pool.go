// Package pool 实现多账号池：租约模型、三重并发闸门、配额感知轮询、分级冷却。
//
// 并发正确性（继承原型的教科书设计）：
//   acquire() 中「资格检查」与「计数器自增」之间没有 await 点（无锁切换），
//   两个并发请求不可能抢到同一个槽位；租约必须显式 Release。
package pool

import (
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wangct233-source/Zcode2api/internal/config"
	"github.com/wangct233-source/Zcode2api/internal/store"
)

// Lease 一次账号租用。
type Lease struct {
	Account store.AccountRecord
	Model   string
	// overflow 表示本次派发越过了并发闸（下注行为，失败即冷却）。
	Overflow bool

	acquiredAt time.Time
	lastSentAt time.Time
}

// AcquireError 取租约失败的归因。
type AcquireError struct {
	Reason string // no_accounts | relogin | quota_hold | account_cooldown | all_gates_full | spacing
	Msg    string
	// RetryAfter 建议客户端稍后重试的等待时长（spacing/all_gates_full 时有值）。
	RetryAfter time.Duration
}

func (e *AcquireError) Error() string { return e.Msg }

// Pool 账号池。
type Pool struct {
	mu      sync.Mutex
	store   *store.Store
	cfg     config.PoolConfig
	runtime map[string]*acctRT // accountID → 运行态
	rrCur   int                // round-robin 游标（内存态，重启归零）
}

type acctRT struct {
	inFlight       int
	lastDispatch   time.Time
	cooldownUntil  time.Time
	modelCooldown  map[string]time.Time
	modelInFlight  map[string]int
	relogin        bool
	lastErrorTag   string
	lastErrorUntil time.Time
}

// New 创建账号池。
func New(s *store.Store, cfg config.PoolConfig) *Pool {
	return &Pool{store: s, cfg: cfg, runtime: map[string]*acctRT{}}
}

// UpdateConfig 热更新池参数（面板改配置后调用）。
func (p *Pool) UpdateConfig(cfg config.PoolConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg = cfg
}

func (p *Pool) rt(id string) *acctRT {
	rt, ok := p.runtime[id]
	if !ok {
		rt = &acctRT{modelCooldown: map[string]time.Time{}, modelInFlight: map[string]int{}}
		p.runtime[id] = rt
	}
	return rt
}

// Acquire 按策略选取一个可用账号并返回租约。
//
// 选号策略（配额感知）：
//  1. 过滤掉 冷却中/配额持有中/需重登/并发满 的账号；
//  2. 已知剩余额度的账号按「余量最小优先」排序（未知垫底）；
//  3. 平票时按创建时间稳定排序 + 游标轮转。
func (p *Pool) Acquire(model string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	accounts := p.store.List()
	if len(accounts) == 0 {
		return nil, &AcquireError{Reason: "no_accounts", Msg: "账号池为空，请先在面板添加账号"}
	}
	now := time.Now()
	type cand struct {
		rec   store.AccountRecord
		rt    *acctRT
		quota *float64
	}
	var cands []cand
	for _, rec := range accounts {
		rt := p.rt(rec.ID)
		if rt.relogin {
			continue
		}
		if now.Before(rt.cooldownUntil) || now.Before(rec.QuotaHoldUntil) {
			continue
		}
		if d := rt.modelCooldown[model]; now.Before(d) {
			continue
		}
		cands = append(cands, cand{rec: rec, rt: rt, quota: rec.QuotaLeft})
	}
	if len(cands) == 0 {
		return nil, &AcquireError{Reason: "all_gates_full", Msg: "所有账号均处于冷却/满载/配额持有状态"}
	}

	// 余量最小优先；未知(nil) 垫底；稳定排序保证轮转公平。
	sort.SliceStable(cands, func(i, j int) bool {
		qi, qj := cands[i].quota, cands[j].quota
		switch {
		case qi != nil && qj != nil:
			return *qi < *qj
		case qi != nil:
			return true
		case qj != nil:
			return false
		}
		return false
	})
	n := len(cands)
	p.rrCur = (p.rrCur + 1) % n
	pick := cands[p.rrCur%n]

	// 同账号最小派发间隔。
	if p.cfg.MinSpacingMs > 0 {
		gap := time.Duration(p.cfg.MinSpacingMs) * time.Millisecond
		if wait := gap - now.Sub(pick.rt.lastDispatch); wait > 0 {
			return nil, &AcquireError{Reason: "spacing", Msg: "账号派发间隔未到", RetryAfter: wait}
		}
	}

	// 账号总闸 + 越闸下注。
	if pick.rt.inFlight >= p.cfg.MaxConcurrentPerAccount {
		return nil, &AcquireError{Reason: "all_gates_full", Msg: "账号并发闸已满"}
	}

	// 模型闸。
	if lim := p.modelLimitLocked(model); lim > 0 {
		if pick.rt.modelInFlight[model] >= lim {
			return nil, &AcquireError{Reason: "all_gates_full", Msg: "模型并发闸已满", RetryAfter: 2 * time.Second}
		}
		pick.rt.modelInFlight[model]++
	}

	pick.rt.inFlight++
	pick.rt.lastDispatch = now
	return &Lease{Account: pick.rec, Model: model, acquiredAt: now}, nil
}

// modelLimitLocked 返回模型并发上限（0=不限，持锁调用）。
func (p *Pool) modelLimitLocked(model string) int {
	if v, ok := p.cfg.MaxConcurrentPerModel[model]; ok {
		return v
	}
	return p.cfg.MaxConcurrentPerModel["default"]
}

// modelInFlight 统计某模型当前在飞请求数（持锁调用）。
func (p *Pool) modelInFlight(model string) int {
	total := 0
	accounts := p.store.List()
	for _, rec := range accounts {
		if rt, ok := p.runtime[rec.ID]; ok {
			total += rt.modelInFlight[model]
		}
	}
	return total
}

// Release 归还租约（流式请求持有到流排空后再调用）。
func (p *Pool) Release(l *Lease) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt, ok := p.runtime[l.Account.ID]; ok && rt.inFlight > 0 {
		rt.inFlight--
		if rt.modelInFlight != nil {
			rt.modelInFlight[l.Model]--
			if rt.modelInFlight[l.Model] <= 0 {
				delete(rt.modelInFlight, l.Model)
			}
		}
	}
}

// Outcome 上游结果分类。
type Outcome string

const (
	OutcomeSuccess  Outcome = "success"
	OutcomeAuth     Outcome = "auth"            // 401/403 → 粘性重登标记
	OutcomeQuota    Outcome = "quota"           // 402/1113 → 配额持有
	OutcomeAcctBusy Outcome = "acct_busy"       // 429/3008 → 账号冷却
	OutcomeModelBus Outcome = "model_busy"      // 3009 → 模型冷却
	OutcomeRisk     Outcome = "risk"            // 3012 → 模型级全局静默
	OutcomeError    Outcome = "error"           // 其他错误
)

// Report 上报结果并执行分级处置。
// 返回 (是否触发风险静默, 错误)。
func (p *Pool) Report(l *Lease, out Outcome, detail string) (risk bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	rt := p.rt(l.Account.ID)
	now := time.Now()
	switch out {
	case OutcomeSuccess:
		rt.lastErrorTag = ""
	case OutcomeAuth:
		rt.relogin = true // 粘性：只有更换凭证才清除
	case OutcomeQuota:
		hold := time.Duration(p.cfg.QuotaHoldMs) * time.Millisecond
		_ = p.store.Update(l.Account.ID, func(r *store.AccountRecord) {
			r.QuotaHoldUntil = now.Add(hold)
			q := -1.0
			r.QuotaLeft = &q
		})
	case OutcomeAcctBusy:
		rt.cooldownUntil = now.Add(time.Duration(p.cfg.CooldownMs) * time.Millisecond)
	case OutcomeModelBus:
		rt.modelCooldown[l.Model] = now.Add(time.Duration(p.cfg.ModelCooldownMs) * time.Millisecond)
	case OutcomeRisk:
		risk = true
	case OutcomeError:
		rt.lastErrorTag = detail
		rt.lastErrorUntil = now.Add(2 * time.Minute)
	}
	return
}

// ClearRelogin 更换凭证后清除重登标记。
func (p *Pool) ClearRelogin(accountID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rt, ok := p.runtime[accountID]; ok {
		rt.relogin = false
	}
}

// Stats 返回面板展示用的账号运行态。
type AcctStat struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	InFlight  int     `json:"inFlight"`
	Cooldown  float64 `json:"cooldownSec"`  // 账号冷却剩余秒
	ModelHold float64 `json:"modelHoldSec"` // 模型冷却剩余秒
	QuotaHold float64 `json:"quotaHoldSec"` // 配额持有剩余秒
	Relogin   bool    `json:"relogin"`
	QuotaLeft *float64 `json:"quotaLeft"`
	LastErr   string  `json:"lastError"`
}

// Snapshot 全池快照。
func (p *Pool) Snapshot() []AcctStat {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	var out []AcctStat
	for _, rec := range p.store.List() {
		rt := p.rt(rec.ID)
		st := AcctStat{ID: rec.ID, Name: rec.Name, Relogin: rt.relogin, QuotaLeft: rec.QuotaLeft, InFlight: rt.inFlight}
		if d := time.Until(rt.cooldownUntil); d > 0 {
			st.Cooldown = d.Seconds()
		}
		if d := time.Until(rec.QuotaHoldUntil); d > 0 {
			st.QuotaHold = d.Seconds()
		}
		if rt.lastErrorTag != "" && now.Before(rt.lastErrorUntil) {
			st.LastErr = rt.lastErrorTag
		}
		out = append(out, st)
	}
	return out
}

var ErrNoAccounts = errors.New("账号池为空")
