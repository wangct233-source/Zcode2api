// Zcode2api · 智云网关
//
// GLM 编程套餐多账号聚合网关（OpenAI 格式输出）。
// 完全开源免费 · 收费的都是骗子。
//
// 用法：
//
//	zcode2api serve [--config config.yaml]   启动网关与管理面板
//	zcode2api version                        打印版本
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wangct233-source/Zcode2api/internal/claim"
	"github.com/wangct233-source/Zcode2api/internal/config"
	"github.com/wangct233-source/Zcode2api/internal/pool"
	"github.com/wangct233-source/Zcode2api/internal/proxy"
	"github.com/wangct233-source/Zcode2api/internal/quota"
	"github.com/wangct233-source/Zcode2api/internal/riskhold"
	"github.com/wangct233-source/Zcode2api/internal/server"
	"github.com/wangct233-source/Zcode2api/internal/store"
	"github.com/wangct233-source/Zcode2api/internal/updater"
)

// version 由构建期 -ldflags 注入。
var version = "dev"

func main() {
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version":
			fmt.Println("Zcode2api", version)
			return
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		default:
			fmt.Fprintf(os.Stderr, "未知命令 %q\n可用命令：serve / version\n", os.Args[1])
			os.Exit(2)
		}
	}
	serve()
}

func serve() {
	configPath := flag.String("config", "config.yaml", "配置文件路径")
	flag.Parse()

	fmt.Println("==============================================")
	fmt.Println("  Zcode2api · 智云网关  version:", version)
	fmt.Println("  完全开源免费 · 收费的都是骗子")
	fmt.Println("  交流群 539858079 · https://github.com/wangct233-source")
	fmt.Println("==============================================")

	// 1. 配置文件（缺则写模板）。
	created, err := config.EnsureConfigFile(*configPath)
	if err != nil {
		fatal("初始化配置失败", err)
	}
	if created {
		fmt.Println("已生成默认配置:", *configPath)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal("加载配置失败", err)
	}

	// 2. 状态目录与账号库。
	dataDir := config.DataDir()
	acctStore, err := store.Open(dataDir)
	if err != nil {
		fatal("打开账号库失败", err)
	}

	// 3. 子系统。
	acctPool := pool.New(acctStore, cfg.Pool)
	hold, err := riskhold.Open(dataDir)
	if err != nil {
		fatal("打开风控静默表失败", err)
	}
	gateway := &proxy.Gateway{
		Cfg:  cfg,
		Pool: acctPool,
		Hold: hold,
		Client: &http.Client{
			// 对齐长周期生成场景：不设整体超时，
			// 连接/响应头阶段设置看门狗，正文阶段由客户端断连传播取消。
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 20,
				IdleConnTimeout:     90 * time.Second,
				ResponseHeaderTimeout: 600 * time.Second,
			},
		},
	}
	srv := &server.Server{
		Cfg: cfg, Store: acctStore, Pool: acctPool, Gateway: gateway,
		Version: version, ConfigPath: *configPath,
	}

	// 4. 启动 HTTP 服务。
	addr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	httpSrv := &http.Server{Addr: addr, Handler: srv.Handler()}
	go func() {
		fmt.Printf("监听 http://%s ｜ 代理 API: /v1/chat/completions ｜ 面板: /admin\n", addr)
		if cfg.Server.Host == "127.0.0.1" {
			fmt.Println("提示：当前仅监听本机；容器部署请设 ZG_HOST=0.0.0.0")
		}
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			fatal("HTTP 服务异常退出", err)
		}
	}()

	// 5. 后台任务。
	quota.New(cfg, acctStore).Start()
	if cfg.Claim.Enabled {
		cs := claim.New(cfg.Claim, acctStore)
		cs.Start()
		defer cs.Stop()
		fmt.Println("[claim] 自动领取已开启（实验性）")
	}
	if cfg.Updater.Enabled && cfg.Updater.IntervalHours > 0 {
		go updateLoop(cfg)
	}

	// 6. 优雅退出：首次信号停止接收；10 秒内二次信号立即退出。
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	fmt.Println("正在优雅关闭（10 秒内再按一次立即退出）...")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(ctx)
	fmt.Println("已退出")
}

// updateLoop 定期检查 GitHub Releases；发现新版本自动下载替换（热更新）。
func updateLoop(cfg *config.Config) {
	for {
		time.Sleep(time.Duration(cfg.Updater.IntervalHours) * time.Hour)
		rel, err := updater.CheckLatest(cfg.Updater.Repo)
		if err != nil {
			fmt.Println("[update] 检查更新失败:", err)
			continue
		}
		if rel == nil {
			continue
		}
		fmt.Println("[update] 发现新版本", rel.TagName, "，正在自动升级...")
		ver, err := updater.Apply(rel, nil)
		if err != nil {
			fmt.Println("[update] 自动升级失败（不影响当前服务）:", err)
			continue
		}
		fmt.Println("[update] 已升级至", ver, "，重启进程后生效")
	}
}

func fatal(msg string, err error) {
	fmt.Fprintln(os.Stderr, msg+":", err)
	os.Exit(1)
}

var _ = filepath.Join
