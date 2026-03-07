# Gitea 自定义开发指南

本文档记录了如何在 paraflow-hq/gitea fork 上进行开发、测试、构建镜像和部署。

## 项目概览

- **仓库**: https://github.com/paraflow-hq/gitea
- **基础版本**: Gitea v1.24.7（分支 `base/v1.24.7`）
- **镜像地址**: `ghcr.io/paraflow-hq/gitea:1.24.7-custom`
- **部署平台**: Render（Docker image 模式）
- **自定义内容**: code search API (`GET /repos/{owner}/{repo}/code_search`)

## 开发环境准备

### 前置依赖

```bash
# Go (需要和 go.mod 中的版本匹配，当前为 1.24)
# 建议用 goenv 或 asdf 管理版本
go version  # 确认版本

# Node.js (前端资源构建需要)
node --version  # 需要 v22+

# Make
make --version
```

### 克隆和切换分支

```bash
git clone git@github.com:paraflow-hq/gitea.git
cd gitea
git checkout base/v1.24.7
```

## 本地开发和测试

### 1. 构建二进制

```bash
# 完整构建（包含前端资源和 SQLite 支持）
TAGS="bindata sqlite sqlite_unlock_notify" make build

# 仅构建后端（前端不变时更快）
TAGS="bindata sqlite sqlite_unlock_notify" make backend
```

> **注意**: 必须加 `bindata` tag，否则模板和翻译文件不会嵌入二进制，运行时会报 `template not found` 错误。

### 2. 本地运行

```bash
# 创建工作目录
mkdir -p /tmp/gitea-test

# 启动（首次会显示安装页面）
GITEA_WORK_DIR=/tmp/gitea-test ./gitea web --port 3333 --install-port 3333
```

浏览器打开 `http://localhost:3333` 完成安装。选择 SQLite3 作为数据库即可。

### 3. 常见问题

**must_change_password 导致 API 无法使用**:
```bash
# 方法1: 命令行
GITEA_WORK_DIR=/tmp/gitea-test ./gitea admin user change-password --username admin --password NewPass123!
GITEA_WORK_DIR=/tmp/gitea-test ./gitea admin user must-change-password --unset admin

# 方法2: 直接改 SQLite（更可靠）
sqlite3 /tmp/gitea-test/gitea.db "UPDATE user SET must_change_password = 0 WHERE name = 'admin';"
```

**创建 API Token**:
```bash
curl -s http://localhost:3333/api/v1/users/admin/tokens \
  -u "admin:你的密码" \
  -H "Content-Type: application/json" \
  -d '{"name": "dev-token", "scopes": ["all"]}'
```

### 4. 运行集成测试

```bash
# 运行指定测试
TAGS="bindata sqlite sqlite_unlock_notify" make test-sqlite#TestAPIRepoCodeSearch

# 运行所有集成测试（耗时较长）
TAGS="bindata sqlite sqlite_unlock_notify" make test-sqlite
```

## 代码结构（关键目录）

```
gitea/
├── routers/api/v1/          # API 路由和处理函数
│   ├── api.go               # 路由注册（在这里添加新路由）
│   └── repo/                # 仓库相关 API
│       └── code_search.go   # 我们添加的代码搜索 API
├── modules/structs/         # API 请求/响应结构体定义
│   └── repo_code_search.go  # 代码搜索的结构体
├── modules/indexer/code/     # 代码搜索引擎
│   └── gitgrep/             # git grep 实现（无需 Indexer）
├── tests/integration/       # 集成测试
├── templates/swagger/       # Swagger 文档（自动生成）
└── Dockerfile               # Docker 构建文件
```

## 添加新 API 的流程

1. **定义结构体** — 在 `modules/structs/` 下创建请求/响应结构体
2. **实现处理函数** — 在 `routers/api/v1/repo/` 下创建处理函数，包含 swagger 注释
3. **注册路由** — 在 `routers/api/v1/api.go` 的 `Routes()` 函数中添加路由
4. **写测试** — 在 `tests/integration/` 下编写集成测试
5. **生成 Swagger** — 构建时自动完成（Dockerfile 中配置了 `make generate-swagger`）

## 提交和构建镜像

### 1. 提交代码

```bash
git add .
git commit -m "feat: 你的改动描述"
git push origin base/v1.24.7
```

推送到 `base/v1.24.7` 分支后，GitHub Actions 会自动构建 Docker 镜像并推送到 GHCR。

### 2. 查看构建状态

```bash
# 查看最近的构建
gh run list --limit 5

# 查看构建日志
gh run view <run-id> --log

# 查看失败日志
gh run view <run-id> --log-failed
```

### 3. 构建产出

- 镜像: `ghcr.io/paraflow-hq/gitea:1.24.7-custom`
- 同时打 `latest` tag
- 构建配置: `.github/workflows/build-image.yml`
- 构建时间: 约 8-12 分钟

### 4. 手动触发构建

如果需要在不推送代码的情况下重新构建：

```bash
gh workflow run build-image.yml --ref base/v1.24.7
```

## 部署到 Render

### 更新现有服务（推荐）

1. Render Dashboard → 找到 Gitea 服务
2. Settings → Image → 改为 `ghcr.io/paraflow-hq/gitea:1.24.7-custom`
3. 选择 `ghcr` Registry Credential
4. Save → 自动重新部署

数据不会丢失（PostgreSQL 和 `/data` 持久化磁盘与容器无关）。

### 创建新的测试实例

```bash
# 需要在 scripts/gitea-recover/.env 中配置:
# RENDER_API_KEY, RENDER_OWNER_ID, RENDER_GHCR_CREDENTIAL_ID, DATADOG_API_KEY

bash scripts/gitea-recover/gitea-create-render.sh --test
```

### Render 相关凭证

| 变量 | 说明 | 获取方式 |
|------|------|----------|
| `RENDER_API_KEY` | Render API token | Render Dashboard → Account Settings → API Keys |
| `RENDER_OWNER_ID` | Workspace ID (`tea-xxx`) | Render Dashboard → Workspace Settings |
| `RENDER_GHCR_CREDENTIAL_ID` | GHCR 凭证 ID (`rgc-xxx`) | `curl -s https://api.render.com/v1/registrycredentials -H "Authorization: Bearer $RENDER_API_KEY"` |

## 验证部署

```bash
GITEA_URL="https://你的gitea域名"
TOKEN="你的token"

# 检查版本
curl -s "${GITEA_URL}/api/v1/version" -H "Authorization: token ${TOKEN}"

# 测试代码搜索
curl -s "${GITEA_URL}/api/v1/repos/{owner}/{repo}/code_search?q=关键词" \
  -H "Authorization: token ${TOKEN}" | python3 -m json.tool

# 查看 Swagger 文档
# 浏览器打开: ${GITEA_URL}/api/swagger
```

## 注意事项

- **不要直接改 `main` 分支** — `main` 跟踪上游 go-gitea/gitea，我们的改动都在 `base/v1.24.7`
- **Go 版本** — Dockerfile 中使用 Go 1.24（与 go.mod 一致），本地可能版本不同，以 CI 构建为准
- **Swagger 生成** — 已集成到 Dockerfile 构建流程中，无需手动运行
- **安全升级** — 如果需要升级到新版本（如 v1.24.8），创建新的 `base/v1.24.8` 分支，cherry-pick 自定义 commit
