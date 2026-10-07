// Package riskhold 模型级风控静默（3012 unusual activity）。
//
// 改进点：与原型的纯内存实现不同，静默期落盘持久化，
// 进程重启后不会立即恢复向被标记模型发请求。
package riskhold

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Hold 持久化风控静默表。
type Hold struct {
	mu     sync.Mutex
	path   string
	untils map[string]time.Time // model → 静默截止时间
}

// Open 打开（或初始化）静默表。
func Open(dir string) (*Hold, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	h := &Hold{path: filepath.Join(dir, "risk-hold.json"), untils: map[string]time.Time{}}
	if data, err := os.ReadFile(h.path); err == nil {
		_ = json.Unmarshal(data, &h.untils)
		// 清理已过期条目
		now := time.Now()
		for k, v := range h.untils {
			if v.Before(now) {
				delete(h.untils, k)
			}
		}
	}
	return h, nil
}

// Remaining 返回模型剩余静默时长（0 表示未静默）。
func (h *Hold) Remaining(model string) time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	until, ok := h.untils[model]
	if !ok {
		return 0
	}
	d := time.Until(until)
	if d <= 0 {
		delete(h.untils, model)
		return 0
	}
	return d
}

// Mark 标记模型静默 duration 时长并落盘。
func (h *Hold) Mark(model string, d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.untils[model] = time.Now().Add(d)
	data, err := json.Marshal(h.untils)
	if err == nil {
		tmp := h.path + ".tmp"
		if os.WriteFile(tmp, data, 0o600) == nil {
			_ = os.Rename(tmp, h.path)
		}
	}
}
