# Kiro Gateway — AI 工具接入指南

## 概述

Kiro Gateway 在本机启动一个 HTTP 代理服务，将 OpenAI/Anthropic 兼容的 API 请求转发到 Kiro（Amazon Q Developer）后端，让 Claude Code、Cursor、Cline、Continue 等 AI 工具**免费或低成本使用 Claude 系列模型**。

**一句话**：启动网关 → 配置工具指向 `http://localhost:8000` → 使用 Kiro 上的 Claude。

---

## 快速开始（3 步）

### 第一步：获取凭据

选择以下任一方式：

#### 方案 A：Refresh Token（推荐）

从 Kiro IDE 或浏览器开发者工具获取 Refresh Token。在 Kiro IDE 登录后，打开开发者工具 → Application → Local Storage，搜索 `refreshToken`。

#### 方案 B：凭据文件

```bash
# Kiro IDE 登录后，凭据文件默认在：
Windows: %USERPROFILE%\.aws\sso\cache\kiro-auth-token.json
macOS/Linux: ~/.aws/sso/cache/kiro-auth-token.json
```

#### 方案 C：kiro-cli SQLite 数据库（自动发现）

如果你用过 `kiro-cli` 或 `amazon-q-developer-cli`，网关会**自动发现** SQLite 数据库：

- `~/.local/share/kiro-cli/data.sqlite3`
- `~/.local/share/amazon-q/data.sqlite3`
- Windows: `%LOCALAPPDATA%\kiro-cli\data.sqlite3`

**直接启动即可，无需配置凭据路径。**

### 第二步：配置并启动网关

在 `kiro-gateway.exe` 同级目录创建 `.env` 文件：

```env
# 必填：设置你的网关密码（自己编一个安全的密码，不要用示例值）
PROXY_API_KEY="your-strong-password-here"

# 如果你的 Refresh Token 是 ...，就填这行（方案 A）
REFRESH_TOKEN="你的_refresh_token"

# 如果你有凭据文件（方案 B），用这个（注意 Windows 路径反斜杠要双写）
# KIRO_CREDS_FILE="C:\\Users\\你的用户名\\.aws\\sso\\cache\\kiro-auth-token.json"

# 方案 C 不需要额外配置，启动即可自动发现
```

启动网关：

```powershell
# Windows 直接双击 kiro-gateway.exe，或在终端运行：
.\kiro-gateway.exe
```

看到这行就成功了：

```text
server listening addr=0.0.0.0:8000 version=2.4.dev.13-go
```

验证：

```powershell
curl http://localhost:8000/health
# 返回 {"status":"healthy",...}
```

### 第三步：连接工具

网关地址：`http://localhost:8000`
网关密码：你在 `.env` 中设置的 `PROXY_API_KEY`

---

## 工具接入配置

### Claude Code

```bash
# 用 OpenAI 兼容方式接入
claude --model claude-sonnet-4.5 \
  --openai-base-url http://localhost:8000/v1 \
  --openai-api-key "your-strong-password-here"
```

或用环境变量：

```bash
export OPENAI_BASE_URL="http://localhost:8000/v1"
export OPENAI_API_KEY="your-strong-password-here"
claude --model claude-sonnet-4.5
```

**支持的模型**（用 `--model` 指定）：

| 模型 | 特点 |
|------|------|
| `claude-haiku-4.5` | 极速、便宜 |
| `claude-sonnet-4` | 稳定、可靠 |
| `claude-sonnet-4.5` | 均衡推荐 |
| `claude-sonnet-4.6` | 最新平衡型 |
| `claude-opus-4.5` | 最强推理 |
| `claude-opus-4.7` | 最新旗舰（需付费订阅） |
| `deepseek-3.2` | 开源 MoE 685B |
| `glm-5` | 开源 MoE 744B |

模型是否可用取决于你的 Kiro 订阅等级，网关不做限制。

### Cursor

1. 打开 Cursor → Settings → Models
2. 关闭所有内置模型
3. 添加自定义模型：
   - **Model Name**: `claude-sonnet-4.5`
   - **OpenAI Base URL**: `http://localhost:8000/v1`
   - **API Key**: 你的 `PROXY_API_KEY`

### Cline / Roo Code（VS Code 插件）

1. API Provider: **OpenAI Compatible**
2. Base URL: `http://localhost:8000/v1`
3. API Key: 你的 `PROXY_API_KEY`
4. Model: `claude-sonnet-4.5`

### OpenCode / Continue / 其他任意工具

只要工具支持自定义 OpenAI 兼容端点，配置三要素：

| 配置项 | 值 |
|--------|-----|
| Base URL | `http://localhost:8000/v1` |
| API Key | 你的 `PROXY_API_KEY` |
| Model | 如 `claude-sonnet-4.5` |

### 支持 Anthropic 协议的工具

如果工具支持 Anthropic Messages API，用这个端点：

| 配置项 | 值 |
|--------|-----|
| Base URL | `http://localhost:8000/v1` |
| API Key | 你的 `PROXY_API_KEY` |
| Model | 如 `claude-sonnet-4-5`（用横线或点号均可） |

---

## 完整配置参考

```env
# ========== 必填 ==========
PROXY_API_KEY="your-strong-password-here"

# ========== 凭据（选一种） ==========

# A: Refresh Token
REFRESH_TOKEN="eyJ..."

# B: 凭据文件
# KIRO_CREDS_FILE="C:\\Users\\xxx\\.aws\\sso\\cache\\kiro-auth-token.json"

# C: kiro-cli SQLite（不需要填，自动发现）
# 或手动指定：KIRO_CLI_DB_FILE="~/.local/share/kiro-cli/data.sqlite3"

# ========== 可选 ==========

# 服务器监听地址和端口
# SERVER_HOST=127.0.0.1    # 仅本机访问（更安全）
# SERVER_PORT=9000          # 自定义端口

# AWS 区域（默认 us-east-1）
# KIRO_REGION=us-east-1
# KIRO_API_REGION=us-east-1

# 代理（中国大陆等受限网络环境需要）
# VPN_PROXY_URL=http://127.0.0.1:7890
# VPN_PROXY_URL=socks5://127.0.0.1:1080

# 多账号（高级功能，使用 credentials.json）
# ACCOUNT_SYSTEM=true

# 推理模式开关
# FAKE_REASONING_ENABLED=true    # 默认开启

# 调试
# DEBUG_MODE=errors              # 出错时保存调试日志
# DEBUG_MODE=all                 # 始终保存调试日志
```

---

## 查看可用模型

```bash
curl -H "Authorization: Bearer your-strong-password-here" http://localhost:8000/v1/models
```

也可以在浏览器打开 `http://localhost:8000/docs` 查看 Swagger 文档交互式测试。

---

## 后台运行（Windows）

```powershell
# PowerShell 后台启动
Start-Process -WindowStyle Hidden .\kiro-gateway.exe

# 或创建快捷方式，目标设为 kiro-gateway.exe 的完整路径，起始位置设为 exe 所在目录
```

---

## 常见问题

**Q: 提示 "initialize gateway account: no accounts configured"**

没有找到任何凭据。检查：
- `.env` 里的 `REFRESH_TOKEN`、`KIRO_CREDS_FILE` 是否填写正确
- 方案 C 检查 kiro-cli 是否已安装登录（运行 `kiro-cli login`）

**Q: 提示 "PROXY_API_KEY is required"**

必须在 `.env` 中设置一个非示例值的 `PROXY_API_KEY`。

**Q: 提示 HTTP 401**

凭据已过期。刷新方式：
- 方案 A/B：重新抓取 refresh token 或重新登录 Kiro IDE
- 方案 C：运行 `kiro-cli login` 重新登录

**Q: 提示 HTTP 502 或连接失败**

可能是网络问题。如果在受限网络环境，需要配置代理：

```env
VPN_PROXY_URL=http://127.0.0.1:7890
```

**Q: 模型不可用（INVALID_MODEL_ID）**

说明该模型不在你的 Kiro 订阅中。查看可用模型列表确认。

---

## 许可

AGPL-3.0-or-later。基于 [Jwadow/kiro-gateway](https://github.com/jwadow/kiro-gateway)。
