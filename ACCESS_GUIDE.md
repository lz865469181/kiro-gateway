# Kiro Gateway — 环境初始化搭建指南

> 本文档面向 **人类用户** 和 **AI 助手**（Claude、Codex 等）。AI 助手可以按本文档的诊断流程自动发现凭据、排查启动故障。

## 目录

1. [快速开始（3 步）](#快速开始3-步)
2. [凭据获取详解](#凭据获取详解)
3. [启动故障诊断（AI 自排查流程）](#启动故障诊断ai-自排查流程)
4. [错误速查表](#错误速查表)
5. [工具接入配置](#工具接入配置)
6. [完整配置参考](#完整配置参考)
7. [后台运行](#后台运行windows)

---

## 概述

Kiro Gateway 在本机启动一个 HTTP 代理服务，将 OpenAI/Anthropic 兼容的 API 请求转发到 Kiro（Amazon Q Developer）后端。

**核心价值**：让 Claude Code、Cursor、Cline、Continue 等 AI 工具使用 Kiro 上的 Claude 系列模型。

---

## 快速开始（3 步）

### 第一步：获取凭据

选择以下任一方式（详见 [凭据获取详解](#凭据获取详解)）：

| 方案 | 适用场景 | 复杂度 |
|------|----------|--------|
| A: Refresh Token | 有 Kiro IDE，手动抓 token | ⭐ |
| B: 凭据文件 | 有 Kiro IDE 且登录中 | ⭐⭐ 自动发现 |
| C: kiro-cli SQLite | 安装了 kiro-cli / q CLI | ⭐ 自动发现 |
| D: credentials.json | 多账号、多区域 | ⭐⭐⭐ |
| E: 环境变量 | 快速测试 | ⭐ |

### 第二步：配置并启动

在 `kiro-gateway.exe` 同级目录创建 `.env` 文件：

```env
# 必填：设置网关密码（自己编一个，不要用示例值）
PROXY_API_KEY="your-strong-password-here"

# 方案 A: 直接填 refresh token
REFRESH_TOKEN="你的_refresh_token"

# 方案 B: 凭据文件路径
# KIRO_CREDS_FILE="/home/user/.aws/sso/cache/kiro-auth-token.json"

# 方案 C: 无需配置，自动发现
```

启动：

```powershell
# Windows
.\kiro-gateway.exe

# Linux/macOS
./kiro-gateway
```

启动成功标志：

```text
server listening addr=0.0.0.0:8000 version=2.4.dev.13-go
```

### 第三步：验证

```bash
curl http://localhost:8000/health
# → {"status":"healthy","timestamp":"...","version":"2.4.dev.13-go"}

curl -H "************** ****** your-password" http://localhost:8000/v1/models
# → {"data":[...], "object":"list"}
```

---

## 凭据获取详解

### 网关的凭据发现机制

网关按以下优先级自动查找凭据：

```
1. ACCOUNTS_CONFIG_FILE (默认 credentials.json)
   └─ JSON 数组 [{"type": "json", "path": "...", ...}]
   └─ 如果文件存在且 ACCOUNT_SYSTEM=true → 直接使用

2. 如果 credentials.json 缺失或 ACCOUNT_SYSTEM=false → 自动迁移:
   a) KIRO_CLI_DB_FILE → 手动指定或自动扫描 → type: "sqlite"
   b) KIRO_CREDS_FILE  → 手动指定 → type: "json"
   c) REFRESH_TOKEN     → 手动指定 → type: "refresh_token"
   └─ 写入 credentials.json
```

### 方案 A：Refresh Token（手动抓取）

从 Kiro IDE 开发者工具提取 token：

1. 打开 Kiro IDE，确保已登录
2. `F12` → Application → Local Storage
3. 搜索 `refreshToken`，复制值
4. 填入 `.env`：`REFRESH_TOKEN="粘贴的值"`

### 方案 B：凭据文件（Kiro IDE 自动发现）

Kiro IDE 登录后，凭据文件路径：

| 系统 | 路径 |
|------|------|
| Windows | `%USERPROFILE%\.aws\sso\cache\kiro-auth-token.json` |
| macOS/Linux | `~/.aws/sso/cache/kiro-auth-token.json` |

**设置方式**：

```env
KIRO_CREDS_FILE="/home/user/.aws/sso/cache/kiro-auth-token.json"
JSON_READONLY=true    # 不让网关修改 IDE 的凭据文件
```

Windows 路径用正斜杠或双反斜杠均可：

```env
KIRO_CREDS_FILE="C:/Users/用户名/.aws/sso/cache/kiro-auth-token.json"
```

**自动发现链**：网关读取 `kiro-auth-token.json` 后，会通过 `clientIdHash` 字段自动追踪到同目录下的 SSO cache 文件（包含 `clientId` 和 `clientSecret`），无需手动指定。

### 重要：SSO 区域 ≠ API 区域

这个文件里有两个"区域"，经常**不同**：

```
kiro-auth-token.json:
  "region": "ap-southeast-1"     ← SSO 区域（你的 IAM Identity Center 在哪）
  "clientIdHash": "0a29bc..."
```

```
profile.json (在 Kiro IDE 数据目录):
  "arn": "arn:aws:codewhisperer:us-east-1:...:profile/XXXXX"
                                    ↑ 第4段是 API 区域
```

如果两者不同，**必须**在 `.env` 中显式指定：

```env
KIRO_CREDS_FILE="/home/user/.aws/sso/cache/kiro-auth-token.json"
JSON_READONLY=true
KIRO_API_REGION=us-east-1    # ← 必须指定！跟 SSO 区域不同
PROFILE_ARN="arn:aws:codewhisperer:us-east-1:...:profile/XXXXX"
PROXY_API_KEY="your-password"
```

**不指定 `KIRO_API_REGION` 的后果**：Kiro API 返回 `profileArn is required`。

API 区域解析优先级：

| 优先级 | 来源 | 示例 |
|--------|------|------|
| 1 | `api_region`（credentials.json 每条账号） | `"api_region": "us-east-1"` |
| 2 | `KIRO_API_REGION` 环境变量 | `KIRO_API_REGION=us-east-1` |
| 3 | Profile ARN 第4段自动提取 | `arn:aws:codewhisperer:us-east-1:...` → `us-east-1` |
| 4 | 回退到 SSO 区域 | `ap-southeast-1` |
| 5 | 默认 `us-east-1` | |

### 方案 C：kiro-cli SQLite（全自动）

如果你用过 `kiro-cli` 或 `q`（Amazon Q CLI），网关会**自动扫描**以下路径：

| 系统 | 扫描路径 |
|------|----------|
| Linux/macOS | `~/.local/share/kiro-cli/data.sqlite3` |
| Linux/macOS | `~/.local/share/amazon-q/data.sqlite3` |
| Windows | `%LOCALAPPDATA%\kiro-cli\data.sqlite3` |
| Windows | `%LOCALAPPDATA%\amazon-q\data.sqlite3` |

找到后自动使用，无需任何配置。

**推荐设置**（防止网关修改 CLI 数据库）：

```env
SQLITE_READONLY=true    # 网关只读，kiro-cli 管理自己的 token
PROXY_API_KEY="your-password"
```

### 方案 D：credentials.json（多账号）

手动创建 `credentials.json`：

```json
[
  {
    "type": "json",
    "path": "C:/Users/用户名/.aws/sso/cache/kiro-auth-token.json",
    "profile_arn": "arn:aws:codewhisperer:us-east-1:...:profile/XXXXX",
    "region": "ap-southeast-1",
    "api_region": "us-east-1"
  },
  {
    "type": "refresh_token",
    "refresh_token": "第二个账号的token",
    "region": "us-east-1"
  }
]
```

启动时设置 `ACCOUNT_SYSTEM=true`：

```env
ACCOUNT_SYSTEM=true
PROXY_API_KEY="your-password"
```

### 方案 E：环境变量（快速测试）

不创建任何文件，直接设环境变量启动：

```powershell
# Windows PowerShell
$env:PROXY_API_KEY = "your-password"
$env:REFRESH_TOKEN = "你的_refresh_token"
.\kiro-gateway.exe
```

```bash
# Linux/macOS
export PROXY_API_KEY="your-password"
export REFRESH_TOKEN="你的_refresh_token"
./kiro-gateway
```

### 认证类型自动检测

网关加载凭据后自动判断认证方式：

- `clientId` + `clientSecret` 都存在 → **AWS SSO OIDC**（`oidc.{region}.amazonaws.com/token`）
- 只有 refresh token → **Kiro Desktop**（`prod.{region}.auth.desktop.kiro.dev/refreshToken`）

---

## 启动故障诊断（AI 自排查流程）

> 以下流程设计为 **AI 助手可自主执行**。如果你是 AI 助手，按 Phase 1→7 顺序排查。

### Phase 1：读取错误消息

```bash
./kiro-gateway.exe
```

所有启动错误都遵循格式：

```text
initialize gateway account: failed to initialize first account <账号ID>: <根本原因>
```

在 [错误速查表](#错误速查表) 中匹配 `<根本原因>`。90% 是 `HTTP 401: Bad credentials`。

### Phase 2：扫描系统凭据

**Windows（PowerShell）：**

```powershell
# Kiro IDE 凭据文件
Get-ChildItem "$env:USERPROFILE\.aws\sso\cache\" -Filter "*.json" -ErrorAction SilentlyContinue |
    Select-Object Name, LastWriteTime, Length | Format-Table -AutoSize

# kiro-cli SQLite 数据库
Get-ChildItem "$env:LOCALAPPDATA" -Recurse -Depth 2 -Filter "*.sqlite3" -ErrorAction SilentlyContinue |
    Where-Object { $_.DirectoryName -match "kiro-cli|amazon-q|Amazon Q" } |
    Select-Object FullName, LastWriteTime, Length | Format-Table -AutoSize

# Kiro IDE Profile ARN
Get-ChildItem "$env:APPDATA\Kiro\User\globalStorage" -Recurse -Filter "profile.json" -ErrorAction SilentlyContinue |
    ForEach-Object { Write-Host "--- $($_.FullName) ---"; Get-Content $_.FullName }

# 检查 CLI 是否安装
Get-Command q, kiro-cli, amazon-q -ErrorAction SilentlyContinue
```

**Linux/macOS：**

```bash
# Kiro IDE 凭据文件
ls -la ~/.aws/sso/cache/*.json 2>/dev/null

# kiro-cli SQLite 数据库
find ~/.local/share/ -name "*.sqlite3" \( -path "*/kiro-cli/*" -o -path "*/amazon-q/*" \) 2>/dev/null

# Kiro IDE Profile ARN
find ~/.config/ ~/ -path "*/Kiro/User/globalStorage/*/profile.json" 2>/dev/null -exec cat {} \;

# 检查 CLI
which q kiro-cli 2>/dev/null
```

### Phase 3：读取凭据文件内容

**如果找到 `kiro-auth-token.json`：**

```bash
cat ~/.aws/sso/cache/kiro-auth-token.json
```

提取关键字段：

| 字段 | 含义 | 示例 |
|------|------|------|
| `region` | SSO 区域 | `ap-southeast-1` |
| `clientIdHash` | 关联的 SSO cache 文件 | `0a29bc4c...` |
| `expiresAt` | Token 过期时间 | `2026-08-05T03:28:14.116Z` ← 过去=已过期 |
| `authMethod` | 认证方法 | `IdC` |
| `provider` | 提供商 | `Enterprise` |

**再读 hash 对应的 SSO cache 文件：**

```bash
cat ~/.aws/sso/cache/{clientIdHash的值}.json
```

确认 `clientId` 和 `clientSecret` 字段存在 → 说明是 OIDC 认证。

**如果找到 SQLite 数据库：**

```bash
sqlite3 /path/to/data.sqlite3 "SELECT key, substr(value,1,100) FROM auth_kv LIMIT 5;"
```

### Phase 4：找到 Profile ARN

当 SSO 区域 ≠ API 区域时，必须提供 Profile ARN。

```powershell
# Windows
Get-ChildItem "$env:APPDATA" -Recurse -Depth 4 -Filter "profile.json" -ErrorAction SilentlyContinue |
    ForEach-Object { Get-Content $_.FullName }
```

```bash
# Linux/macOS
find ~/.config/ -path "*/Kiro/User/globalStorage/*/profile.json" 2>/dev/null -exec cat {} \;
```

期望内容：`{"arn": "arn:aws:codewhisperer:us-east-1:...:profile/XXXXX", "name": "..."}`

第4段（`us-east-1`）就是 API 区域。

### Phase 5：验证 Token 是否过期

如果 `expiresAt` 已过去：
- 网关会用 refresh token 尝试刷新
- 如果 OIDC 刷新也失败 → 账号 session 已过期
- **解决**：用户重新登录 Kiro IDE，或运行 `q login`

### Phase 6：生成并写入 `.env`

根据 Phase 2-4 的发现，生成正确的 `.env`：

**最常见情况：Kiro IDE + SSO（Enterprise）**

```env
KIRO_CREDS_FILE=凭据文件完整路径（Phase 2 找到的 kiro-auth-token.json）
JSON_READONLY=true
KIRO_API_REGION=API区域（Phase 4 从 profile ARN 提取的，通常跟 SSO 区域不同）
PROFILE_ARN=Phase 4 找到的完整 ARN
PROXY_API_KEY=用户自定义密码
```

**Kiro CLI 用户：**

```env
SQLITE_READONLY=true
PROXY_API_KEY=用户自定义密码
# 无需指定 KIRO_CLI_DB_FILE，自动发现
```

**直接 Refresh Token：**

```env
REFRESH_TOKEN=有效的 token
PROXY_API_KEY=用户自定义密码
```

### Phase 7：验证修复

```bash
# 前台启动查看日志
./kiro-gateway.exe

# 期望输出：
# time=... level=INFO msg="server listening" addr=0.0.0.0:8000 version=2.4.dev.13-go
```

```bash
# 另一个终端验证
curl http://127.0.0.1:8000/health
# → {"status":"healthy",...}

curl -H "************** ****** your-password" http://127.0.0.1:8000/v1/models
# → {"data":[...]}

curl -s -X POST http://127.0.0.1:8000/v1/chat/completions \
  -H "************** ****** your-password" \
  -H "Content-Type: application/json" \
  -d '{"model":"claude-haiku-4.5","messages":[{"role":"user","content":"Say OK"}],"max_tokens":10}'
# → {"choices":[{"message":{"content":"OK"}}],...}
```

---

## 错误速查表

### 启动错误

| 启动错误消息 | 根因 | Phase | 修复 |
|---|---|---|---|
| `no accounts configured` | 没找到任何凭据 | Phase 2 | 配置一个凭据方案 |
| `HTTP 401: Bad credentials` | Token 过期/无效/占位 | Phase 3 | 重新登录 Kiro IDE 或 kiro-cli |
| `HTTP 400: ...` | OIDC 刷新失败 | Phase 3 | 检查 OIDC 端点可达性、clientId/clientSecret |
| `profileArn is required` | API 区域错误或缺失 | Phase 4 | 设置 `KIRO_API_REGION` + `PROFILE_ARN` |
| `refresh token is not set` | 凭据文件中没有 token | Phase 3 | 检查文件内容非空 |
| `client ID is not set` | SSO cache 文件缺失 | Phase 3 | 检查 `clientIdHash` 链完整 |
| `client secret is not set` | SSO cache 文件缺 `clientSecret` | Phase 3 | 检查 `.aws/sso/cache/{hash}.json` |
| `invalid region "..."` | 区域格式不匹配 `xx-xxxx-N` | - | 修正为 `us-east-1` 格式 |
| `PROXY_API_KEY is required` | 未设置或用了示例值 | - | 设置自定义 `PROXY_API_KEY` |
| `failed to initialize any account` | 所有账号都初始化失败 | Phase 1 | 检查每个账号的独立错误 |

### 账号ID前缀含义

| 前缀 | 凭据类型 | 来源 |
|---|---|---|
| `refresh_token_` | 直接 Refresh Token | `REFRESH_TOKEN` 或 credentials.json |
| 完整文件路径 | JSON 凭据文件 | `KIRO_CREDS_FILE` 或 credentials.json |
| SQLite 路径 | SQLite 数据库 | `KIRO_CLI_DB_FILE` 或自动发现 |

### 运行时错误

| HTTP 状态 | 错误消息 | 含义 | 修复 |
|---|---|---|---|
| 401 | `Invalid or missing API Key` | 请求没带 `PROXY_API_KEY` | 加上 `Authorization: Bearer <key>` |
| 400 | `profileArn is required` | 同上"API 区域错误" | Phase 4 |
| 400 | `INVALID_MODEL_ID` | 模型不在订阅中 | 查看 `/v1/models` 可用列表 |
| 502 | DNS/连接失败 | 网络问题 | 检查网络/VPN，可能需配 `VPN_PROXY_URL` |
| 504 | 超时 | Kiro API 响应慢 | 重试或换模型 |

### 常见配置陷阱

- [ ] **PROXY_API_KEY 是示例值**：`my-super-secret-password-123` 等会被拒绝，必须自定义
- [ ] **REFRESH_TOKEN 是占位符**：`dummy`、`your_*` 等空值必 401
- [ ] **SSO 区域 ≠ API 区域**：最常见 `profileArn is required` 根因
- [ ] **Token 过期**：SQLite 模式下可能静默过期，重新登录 CLI 即可
- [ ] **JSON_READONLY 未设**：网关可能修改 IDE 凭据文件，建议始终设为 `true`
- [ ] **SQLITE_READONLY 未设**：网关可能修改 CLI 数据库，建议设为 `true`
- [ ] **VPN/代理阻断**：`oidc.{region}.amazonaws.com` 在受限网络不可达，需配 `VPN_PROXY_URL`

---

## 工具接入配置

网关地址：`http://localhost:8000`，密码：你设的 `PROXY_API_KEY`

### Claude Code

```bash
export OPENAI_BASE_URL="http://localhost:8000/v1"
export OPENAI_API_KEY="your-password"
claude --model claude-sonnet-4.5
```

### Cursor

Settings → Models → 关闭内置模型 → 添加：

- Model Name: `claude-sonnet-4.5`
- OpenAI Base URL: `http://localhost:8000/v1`
- API Key: 你的 `PROXY_API_KEY`

### Cline / Roo Code

- API Provider: **OpenAI Compatible**
- Base URL: `http://localhost:8000/v1`
- API Key: 你的密码
- Model: `claude-sonnet-4.5`

### 支持 Anthropic 协议的工具

| 配置项 | 值 |
|--------|-----|
| Base URL | `http://localhost:8000/v1` |
| API Key (`x-api-key`) | 你的密码 |
| Anthropic Version | `2023-06-01` |
| Model | `claude-sonnet-4-5` 或 `claude-sonnet-4.5` |

### 可用模型

以下模型是否可用取决于你的 Kiro 订阅等级，网关不做限制：

| 模型 | 特点 |
|------|------|
| `claude-haiku-4.5` | 极速、便宜 |
| `claude-sonnet-4` | 稳定可靠 |
| `claude-sonnet-4.5` | 均衡推荐 |
| `claude-sonnet-4.6` | 最新平衡型 |
| `claude-opus-4.5` | 最强推理 |
| `claude-opus-4.6` | 最新旗舰 |
| `claude-opus-4.7` | 最新旗舰（需付费订阅） |
| `deepseek-3.2` | 开源 MoE 685B |
| `glm-5` | 开源 MoE 744B |
| `minimax-m2.1` / `m2.5` | MiniMax 模型 |
| `qwen3-coder-next` | 通义千问 Coder |

查看完整列表：

```bash
curl -H "************** ****** your-password" http://localhost:8000/v1/models
```

---

## 完整配置参考

```env
# ========== 必填 ==========
PROXY_API_KEY="your-strong-password-here"

# ========== 凭据（选一种） ==========

# A: Refresh Token
REFRESH_TOKEN="eyJ..."

# B: 凭据文件（Kiro IDE）
KIRO_CREDS_FILE="C:/Users/用户名/.aws/sso/cache/kiro-auth-token.json"
JSON_READONLY=true
# 如果 SSO 区域 ≠ API 区域，还需要：
KIRO_API_REGION=us-east-1
PROFILE_ARN="arn:aws:codewhisperer:us-east-1:...:profile/XXXXX"

# C: kiro-cli SQLite（自动发现，无需配置路径）
SQLITE_READONLY=true

# 手动指定 SQLite 路径（自动发现失败时）
# KIRO_CLI_DB_FILE="~/.local/share/kiro-cli/data.sqlite3"

# ========== 可选 ==========

# 服务器
# SERVER_HOST=127.0.0.1    # 仅本机（更安全）
# SERVER_PORT=9000          # 自定义端口

# 代理（受限网络环境）
# VPN_PROXY_URL=http://127.0.0.1:7890
# VPN_PROXY_URL=socks5://127.0.0.1:1080

# 多账号
# ACCOUNT_SYSTEM=true

# 推理
# FAKE_REASONING_ENABLED=true    # 默认开启

# 调试
# DEBUG_MODE=errors              # 出错时保存日志
# DEBUG_MODE=all                 # 始终保存日志
```

---

## 后台运行（Windows）

```powershell
# PowerShell 后台启动
Start-Process -WindowStyle Hidden .\kiro-gateway.exe
```

---

## 许可

AGPL-3.0-or-later。基于 [Jwadow/kiro-gateway](https://github.com/jwadow/kiro-gateway)。
