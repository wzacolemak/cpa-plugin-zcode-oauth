# cpa-plugin-zcode-oauth

CLIProxyAPI (CPA) 原生插件：把 GLM Coding Plan / ZCode 的账号直接接成一个 CPA provider，并提供
用量面板与**体验套餐（relay）自动调度**。

- 免中转、无第三方账号：用你自己的 bigmodel / zcode 账号授权，插件自己发上游请求
- 三种上游，可在管理面板「🔀 套餐」里切换：`relay` / `ultra` / `bigmodel_direct`
- `auto` 模式：检测到体验套餐就优先用它，并发默认 1，超出/不可用自动回落正常渠道
- 管理面板内嵌用量看板（5 小时 / 每周 / MCP 月度、重置卡、账户信息）

## 功能

| 模式 | 上游 | 说明 |
| --- | --- | --- |
| `relay` | `zcode.z.ai/api/v1/zcode-plan/anthropic` | OAuth JWT 走 zcode 中转（start plan / 体验套餐） |
| `ultra` | `zcode.z.ai/api/v1/ultra/anthropic` | 长期 CodingPlan API key，官方 1.5x 额度口径 |
| `bigmodel_direct` | `open.bigmodel.cn/api/anthropic` | Coding Max 订阅的 API key 直连 |
| `auto` | 先 `relay`，回落 `ultra` / `bigmodel_direct` | 见下 |

### auto 模式（体验套餐优先）

1. 用 OAuth JWT 查 `zcode-plan/billing/current`，取生效期内的套餐；结果缓存 `plan_detect_ttl_seconds`
   （默认 300s），并发请求只发一次检测
2. 只把套餐覆盖的模型路由到 relay；套餐未下发模型清单时用 `trial_models`（默认 `glm-5.3`、`glm-5.3-flash`）
3. relay 并发上限 `relay_max_concurrency`（默认 1，体验套餐本身就这么窄）。**超出的请求不排队**，
   立刻走正常渠道
4. relay 报额度/鉴权/5xx 时，本次回落正常渠道，并把体验套餐标记 `plan_detect_ttl_seconds` 不可用，
   避免每个请求都先失败一次；请求本身写错（4xx）不回落，防止重复计费

弹窗里能看到体验套餐可用性、覆盖模型、实时并发占用、回落目标，以及累计计数
（`relay_hits` / `fallback_hits` / `busy_overflow` / `relay_failover`）。

## 安装

### 方式一：CPA 插件商店

本仓库同时提供一个**自建商店源**（`registry.json`）。在 CPA 配置里追加：

```yaml
plugins:
  enabled: true
  dir: "plugins"
  store-sources:
    - https://raw.githubusercontent.com/wzacolemak/cpa-plugin-zcode-oauth/main/registry.json
```

保存后到管理面板「插件商店」刷新，选中 `zcode` 安装。

### 方式二：手动放二进制

从 [Releases](https://github.com/wzacolemak/cpa-plugin-zcode-oauth/releases) 下载
`zcode_<version>_<os>_<arch>.zip`，解出的 `zcode.so` 放到：

```
<plugins.dir>/<goos>/<goarch>/zcode.so      # 例如 plugins/linux/amd64/zcode.so
```

重启 CPA。

### 方式三：自己编译

插件依赖 CPA 的 SDK（`go.mod` 里 `replace github.com/router-for-me/CLIProxyAPI/v8 => /cpa-src`），
所以构建时要挂载 CPA 源码：

```bash
# 本机 Go 版本 >= go.mod 要求
make build CPA_SRC=/path/to/CLIProxyAPI

# 或走容器（推荐，避免本地 Go 版本不符）
make docker-build CPA_SRC=/path/to/CLIProxyAPI GOPROXY=https://goproxy.cn,direct
```

## 配置

```yaml
plugins:
  configs:
    zcode:
      enabled: true
      priority: 2
      # 本实例对外地址，用于面板里的自引用链接（登录/刷新/重置）
      public_base: "http://127.0.0.1:3003"
      upstream_mode: "auto"            # relay | ultra | bigmodel_direct | auto
      auto_fallback_mode: "ultra"      # auto 的正常渠道
      relay_max_concurrency: 1         # 体验套餐并发
      trial_models:                    # 套餐没下发模型清单时的默认覆盖
        - glm-5.3
        - glm-5.3-flash
      plan_detect_ttl_seconds: 300
      auto_fallback_on_error: true
      timeout_seconds: 300
      app_version: "3.14.4"
      data_dir: "plugins/zcode-oauth-data"
      bigmodel_api_key: ""             # ultra / bigmodel_direct 用的长期 key
```

首次使用：在管理面板 auth-files 里给 `zcode` 添加凭据，或直接访问
`<public_base>/v0/resource/plugins/zcode/login` 完成 bigmodel 授权。

## 安全说明

- 本仓库**不含任何凭据**。运行期凭据在 `data_dir`（默认 `plugins/zcode-oauth-data/credential.json`），
  已被 `.gitignore` 排除
- 构建产物（`*.so` / `*.h`）、本机部署脚本、`auths/`、`logs/` 同样不入库
- 插件只是用你自己的账号发请求，不上报任何数据

## 已知限制

- `relay` 通道能否用取决于上游风控：实测某些套餐/出口会被 zcode 风控直接拒绝
  （HTTP 405 `{"code":3012,"msg":"request has been blocked due to unusual activity."}`）。
  `auto` 模式遇到这种情况会自动回落并冷却，不影响服务
- 并发计数是进程级的：多副本 CPA 各自单独计数，上游可能看到大于 `relay_max_concurrency` 的并发

## 开发

```bash
make docker-test CPA_SRC=/path/to/CLIProxyAPI   # go vet + go test -race
```

测试覆盖：并发闸门、模型覆盖匹配、检测缓存 TTL/失效/single-flight、回落错误分类，
以及用本地 stub 上游跑的 `executeAuto` 端到端（覆盖模型走 relay / 未覆盖走正常渠道 /
relay 占满时溢出 / relay 报错回落）。

## 致谢与许可

面板注入思路与 ZCode 客户端字段参考了 [shizukugo/cpa-plugin-zcode](https://github.com/shizukugo/cpa-plugin-zcode)（MIT）。
本项目以 MIT 许可发布，见 [LICENSE](LICENSE) 与 [NOTICE](NOTICE)。
