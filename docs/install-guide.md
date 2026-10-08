# Zcode2api · 完整安装部署指南

> 智云网关 —— GLM 编程套餐多账号聚合网关（OpenAI 格式输出）  
> 完全开源免费 · 收费的都是骗子  
> 仓库：<https://github.com/wangct233-source/Zcode2api>

---

## ~~方式一：Docker Compose 部署（推荐）~~

### ~~1. 准备目录~~

```bash
mkdir -p /opt/zcode2api/data /opt/zcode2api/config
cd /opt/zcode2api
```

### ~~2. 下载官方 compose 文~~件

```bash
wget https://raw.githubusercontent.com/wangct233-source/Zcode2api/main/deploy/docker-compose.yml -O docker-compose.yml
```

### 3. 生成配置文件 `config/config.yaml`

```yaml
server:
  host: 127.0.0.1
  port: 17800
provider:
  name: zai          # zai 或 bigmodel
defaultModel: glm-4.6
models:
  - glm-4.6
  - glm-4.5
  - glm-4.5-air
  - glm-4.5-flash
```

### 4. 修改 compose 中的两个必设环境变量

打开 `docker-compose.yml`：

| 变量                  | 说明                                       |
| ------------------- | ---------------------------------------- |
| `ZG_PROXY_API_KEY`  | 调用 `/v1/chat/completions` 时用的密钥，改成随机长字符串 |
| `ZG_PANEL_PASSWORD` | Web 面板密码（**没有兜底密码，不设面板锁定**）              |

可选变量：

| 变量                     | 说明                  |
| ---------------------- | ------------------- |
| `ZG_CREDENTIAL_SECRET` | 账号库加密种子，设固定值可跨机迁移账号 |
| `ZG_PROVIDER`          | `zai` / `bigmodel`  |
| `ZG_STORE_DIR`         | 状态目录，默认 `/app/data` |

### 5. 启动

```bash
docker compose up -d
docker logs -f zcode2api        # 看启动日志
```

### 6. 验证

```bash
curl http://127.0.0.1:17800/health
# {"inFlight":0,"status":"ok"}  即部署成功
```

---

## 方式二：拉取镜像的其他途径（网络慢时）

镜像：`ghcr.io/wangct233-source/zcode2api:latest`（amd64 / arm64 双架构）

```bash
# 直连
docker pull ghcr.io/wangct233-source/zcode2api:latest

# DaoCloud 加速
docker pull ghcr.m.daocloud.io/wangct233-source/zcode2api:latest
docker tag ghcr.m.daocloud.io/wangct233-source/zcode2api:latest ghcr.io/wangct233-source/zcode2api:latest

# 南京大学镜像
docker pull ghcr.nju.edu.cn/wangct233-source/zcode2api:latest
docker tag ghcr.nju.edu.cn/wangct233-source/zcode2api:latest ghcr.io/wangct233-source/zcode2api:latest
```

> 注意：镜像名必须全小写。compose 里 `image:` 写 `ghcr.io/wangct233-source/zcode2api:latest`，用 tag 命令对齐即可。

---

## 方式三：二进制部署（支持热更新）

从 <https://github.com/wangct233-source/Zcode2api/releases> 下载对应平台产物（附 sha256 校验文件）：

```bash
wget https://github.com/wangct233-source/Zcode2api/releases/latest/download/zcode2api-<版本>-linux-amd64 -O zcode2api
chmod +x zcode2api
ZG_PROXY_API_KEY=你的密钥 ZG_PANEL_PASSWORD=你的面板密码 ./zcode2api serve
```

**热更新**：默认每 24 小时自动检查本仓库 Releases，发现新版本自动下载 → sha256 校验 → 原子替换二进制，重启进程即完成升级。面板内也有「检查更新 / 一键升级」按钮。

---

## 初始化配置

### 登录面板

浏览器打开 `http://服务器IP:17800/admin`，输入 `ZG_PANEL_PASSWORD` 登录。

### 添加账号

面板 → 账号池 → 粘贴凭证 → 添加：

- **API Key**：Z.AI 控制台生成的密钥（推荐，最稳定）
- **JWT 凭证**：订阅账号的登录凭证

账号库使用 AES-GCM + scrypt 加密落盘在 `data/accounts.json`，容器重建不丢；跨机迁移时在两侧设置相同的 `ZG_CREDENTIAL_SECRET`。

### 调用 API（OpenAI 格式）

```bash
curl http://服务器IP:17800/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer 你的ZG_PROXY_API_KEY" \
  -d '{
    "model": "glm-4.6",
    "messages": [{"role": "user", "content": "你好"}],
    "stream": true
  }'
```

客户端（LobeChat / Cline / 沉浸式翻译等）配置：

| 项            | 值                       |
| ------------ | ----------------------- |
| API Base URL | `http://服务器IP:17800/v1` |
| API Key      | `ZG_PROXY_API_KEY` 的值   |
| 模型           | `glm-4.6` 等（面板可见白名单）    |

---

## 常用运维命令

```bash
docker logs -f zcode2api            # 实时日志
docker restart zcode2api            # 重启
docker compose pull && docker compose up -d   # 升级到最新镜像
docker compose down                 # 停止（data/ 卷保留，账号不丢）
tar czf zcode2api-backup.tar.gz data config   # 备份（账号库为加密文件，可放心打包）
```

## 常见问题

| 问题           | 处理                                                    |
| ------------ | ----------------------------------------------------- |
| 面板打不开提示锁定    | 未设置 `ZG_PANEL_PASSWORD`，设置后 `docker compose up -d` 重建 |
| 拉镜像超时        | 用方式二的镜像加速源                                            |
| 上游 401/账号需重登 | 面板删除该账号重新添加凭证                                         |
| 所有账号 503     | 账号池为空或全部处于冷却，看面板账号状态                                  |
| 端口冲突         | 改 compose 端口映射 `"18000:17800"`                        |
| 容器端口映射不通    | 镜像已内置 `ZG_HOST=0.0.0.0`（Dockerfile ENV，优先级高于 config.yaml）；裸跑二进制默认 127.0.0.1 是有意的安全默认，对外服务请设 `ZG_HOST=0.0.0.0` |

---

## 本服务器当前部署实况（43.226.44.194）

- 部署目录：`/opt/zcode2api`（compose + config + data）
- 镜像拉取：直连 ghcr 慢（~45KB/s），已走 `ghcr.m.daocloud.io` 加速
- 本次部署密钥（部署时随机生成）：
  - API Key：`zg-7f1a73a05b3c1d9dd6c3ccfe08736e15`
  - 面板密码：`zg-f734337a6d55f40d`
- 防火墙：17800/tcp 已放行

---

© 2026 ·智云-wangct233 · 交流群 1071892426 → <https://qm.qq.com/q/xHtxPNo5qM>
