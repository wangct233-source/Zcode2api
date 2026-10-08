# ⚡ Zcode2api · 智云网关

> **GLM 编程套餐多账号聚合网关（Go 实现）—— 一个本地端点，后面站着一整个账号池。**
>
> **完全开源免费 · 收费的都是骗子**

[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/wangct233-source/Zcode2api)](https://github.com/wangct233-source/Zcode2api/releases)
[![Docker](https://img.shields.io/badge/docker-ghcr.io-2496ED)](https://github.com/wangct233-source/Zcode2api/pkgs/container/zcode2api)

> ⚠️ **[免责声明（24 小时学习警告）](DISCLAIMER.md)** —— 本项目仅供学习研究，获取后请在 24 小时内自行评估删除；一切使用后果自负。

---

## 这是什么

把 GLM 编程套餐（Z.AI / 智谱 Bigmodel）从「一机一号」的限制中解出来：

- 🌉 **OpenAI 格式 API 输出** —— `/v1/chat/completions`、`/v1/models`，任何支持 OpenAI 协议的客户端（LobeChat、Cline、沉浸式翻译…）即插即用
- 👥 **多账号池** —— 自动轮询、每账号独立设备指纹、三重并发闸门（账号级 + 模型级 + 派发间隔）
- 🌡️ **分级风控处置** —— 429/3008→账号冷却、3009→模型冷却、402/1113→配额持有、401/403→重登标记、3012→模型级 30 分钟静默（**持久化**，重启不清）
- 🖥️ **Web 管理面板** —— 账号管理、运行状态、一键升级，单文件内嵌零 CDN
- 🔥 **热更新** —— 自动从 GitHub Releases 拉取新版本，校验 sha256 后替换二进制，无需重新构建
- 🐳 **Docker 一键部署** —— 预构建多架构镜像（amd64/arm64），`docker compose up -d` 即用

> 架构设计思想受 [ZcodeKnight](https://github.com/YU123-ZZZ/zcodeknight-YU) (MIT) 启发，全部代码为 Go 独立实现，并修复了多项已知缺陷（见下方「相比原型的改进」）。

## 快速开始（Docker）

```bash
mkdir -p data config
# 把 deploy/docker-compose.yml 下载到当前目录后：
docker compose up -d
```

编辑 `deploy/docker-compose.yml` 中的两个**必设**环境变量：

| 变量 | 说明 |
|---|---|
| `ZG_PROXY_API_KEY` | 你调用 `/v1/chat/completions` 时用的密钥 |
| `ZG_PANEL_PASSWORD` | 面板密码（**没有兜底密码，不设面板锁定**） |

然后：

- 代理 API：`http://127.0.0.1:17800/v1/chat/completions`（Authorization: Bearer $ZG_PROXY_API_KEY）
- 管理面板：`http://127.0.0.1:17800/admin`
- 健康检查：`/health`

### 在面板添加账号

登录面板 → 账号池 → 粘贴 Z.AI API Key（或 JWT 凭证）→ 完成。账号库使用 AES-GCM + scrypt(KDF) 加密落盘，密钥可用 `ZG_CREDENTIAL_SECRET` 固定以支持跨机迁移。

## 二进制部署

从 [Releases](https://github.com/wangct233-source/Zcode2api/releases) 下载对应平台产物：

```bash
./zcode2api serve --config config.yaml   # 首次启动自动生成配置模板
```

**热更新**：二进制版内置 updater（默认每 24h 检查），发现新版本自动下载 → sha256 校验 → 原子替换 → 重启进程即完成升级；面板也有「检查更新 / 一键升级」按钮。

## 配置

优先级：`环境变量 > config.yaml > 默认值`。完整默认值见 `internal/config/config.go`，常用项：

| 配置 | 默认 | 说明 |
|---|---|---|
| `server.host/port` | 127.0.0.1:17800 | 容器内请设 `ZG_HOST=0.0.0.0` |
| `auth.proxyApiKey` | 无 | 客户端密钥，未设不校验 |
| `auth.panelPassword` | 无 | 面板密码，未设面板锁定 |
| `provider.name` | zai | `zai` / `bigmodel`，或直接改 `openaiBase` |
| `pool.maxConcurrentPerAccount` | 2 | 单账号并发闸（上游实测上限 3） |
| `pool.minSpacingMs` | 1500 | 同账号最小派发间隔 |
| `claim.enabled` | false | 自动领取（实验性，Rod 无头浏览器） |
| `updater.intervalHours` | 24 | 热更新检查周期，0=仅手动 |

## 相比原型的改进

本项目在吸收 ZcodeKnight 设计思想的基础上，修复了对比分析中发现的已知缺陷：

| 改进 | 说明 |
|---|---|
| 🔒 面板无兜底键 | 未设置密码时面板完全锁定（原型存在永续可用的 `admin` 兜底键） |
| 🔑 scrypt KDF | 账号库加密用 scrypt+随机盐（原型为无盐 SHA-256），支持跨机迁移 |
| ⏱️ 看门狗 | 上游响应头 600s 超时（原型同步路径完全无超时） |
| 💾 风控静默持久化 | 3012 静默期落盘，重启不重置（原型重启即清） |
| 🏷️ 结构化错误分类 | 状态码优先，不再依赖响应体正则 |
| 🚫 登录限速前置 | 失败计数先于密码比较（防绕过） |
| 🧹 零死代码 | 全部模块真实接线，无休眠功能 |

## Roadmap

- [ ] 会话→账号粘性（保护上游 prompt cache）
- [ ] 出口 IP 多代理轮换与账号绑定
- [ ] claim 真实页面接入（当前为实验性骨架）
- [ ] 请求级用量统计与成本追踪
- [ ] 凭证失效率动态权重

## 交流

- 💬 QQ 交流群：**1071892426** → [一键加群](https://qm.qq.com/q/XcS6Sh8NYA)
- 👤 作者主页：<https://github.com/wangct233-source>

---

© 2026 ·智云-wangct233 · 当前版本见 [Releases](https://github.com/wangct233-source/Zcode2api/releases) · **完全开源免费 · 收费的都是骗子**
