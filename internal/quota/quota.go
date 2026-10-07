// Package quota 后台余额轮询：为账号池提供配额感知所需的余量数据。
package quota

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/wangct233-source/Zcode2api/internal/config"
	"github.com/wangct233-source/Zcode2api/internal/store"
)

// Poller 余额轮询器。
type Poller struct {
	cfg    *config.Config
	store  *store.Store
	client *http.Client
	stop   chan struct{}
	once   sync.Once
}

// New 创建轮询器（默认每 5 分钟一轮）。
func New(cfg *config.Config, s *store.Store) *Poller {
	return &Poller{
		cfg:    cfg,
		store:  s,
		client: &http.Client{Timeout: 20 * time.Second},
		stop:   make(chan struct{}),
	}
}

// Start 启动后台轮询。
func (p *Poller) Start() {
	go func() {
		t := time.NewTicker(5 * time.Minute)
		defer t.Stop()
		p.poll() // 启动后立即跑一轮
		for {
			select {
			case <-p.stop:
				return
			case <-t.C:
				p.poll()
			}
		}
	}()
}

// Stop 停止。
func (p *Poller) Stop() { p.once.Do(func() { close(p.stop) }) }

// poll 对每个 api_key 账号查询一次余额并写回账号库。
func (p *Poller) poll() {
	for _, rec := range p.store.List() {
		if rec.Kind != "api_key" {
			continue
		}
		q, err := p.balance(rec)
		if err != nil {
			log.Printf("[quota] 账号 %s 余额查询失败: %v", rec.ID, err)
			continue
		}
		_ = p.store.Update(rec.ID, func(r *store.AccountRecord) { r.QuotaLeft = q })
	}
}

// balance 调用上游计费接口查询余额（元）。
// 上游若调整接口结构，仅此函数需要跟进。
func (p *Poller) balance(rec store.AccountRecord) (*float64, error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.z.ai/api/coding/paas/v4/billing/usage", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+rec.Secret)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errHTTP(resp.StatusCode)
	}
	var body struct {
		Data struct {
			Balance float64 `json:"balance"`
		} `json:"data"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return nil, err
	}
	q := body.Data.Balance
	return &q, nil
}

type httpErr int

func (e httpErr) Error() string { return "上游返回 " + intString(int(e)) }

func errHTTP(code int) error { return httpErr(code) }

func intString(i int) string {
	return fmtInt(i)
}

func fmtInt(i int) string {
	b := [12]byte{}
	pos := len(b)
	neg := i < 0
	if neg {
		i = -i
	}
	if i == 0 {
		return "0"
	}
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
