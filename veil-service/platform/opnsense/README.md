# OPNsense

`os-veil` 提供原生 **服务 → Veil** 页面，通过 configd 管理 Go 控制服务。界面跟随 OPNsense 的英文或简体中文设置，与桌面和 LuCI 共用连接编辑器，支持多连接、中转、完整连接包导入/导出、多个代理入口、Google HTTPS 测试和分段诊断。

客户端可选择 SOCKS5、HTTP（含 HTTPS CONNECT）或同端口混合入口。“高级：独立服务端或固定目标转发”保留服务端及固定转发实例的配置、校验和启停功能。

## 安装与使用

在对应版本的 OPNsense 上安装原生包：

```sh
pkg add /path/to/os-veil-0.3.3.pkg
# 已安装时，用 -f 替换为指定的本地包：
pkg add -f /path/to/os-veil-0.3.3.pkg
```

安装会启动管理服务。打开 **服务 → Veil**，导入连接 JSON、确认入口和监听地址、保存，然后启动代理。应用已保存配置会提示确认并断开旧连接；仅保存不会影响正在运行的实例。升级会停止旧管理进程，保留保存配置，升级后在页面启动代理。

入口示例见 [client.mixed.json](../../../veil-core/examples/client.mixed.json)。替换服务器和凭据后导入；自定义 CA、服务端证书和私钥使用防火墙上持久目录内的绝对路径。SOCKS/HTTP 入口无本地用户认证，仅供本机使用时监听回环地址；向 LAN 提供服务时绑定相应 LAN 地址，由其他组件管理访问规则与流量路由。

连接卡片的启动/停止会同步保存该连接的启用状态；开机按此状态恢复。旧的单客户端配置可迁移到连接列表。独立服务端区域的“系统启动时自动启动代理”单独控制其启动钩子，手动停止不会取消这一勾选项。卸载 `pkg delete os-veil` 会停止管理进程并清理派生运行文件，`config.xml` 内的配置保留，重装后可重新启动。

## 配置与权限

`config.xml` 中的 `OPNsense/veil` 是配置来源，随 OPNsense 系统备份保存。`/var/run/veil/state` 仅保存供 daemon 使用的派生配置；启动或应用时从平台配置同步。普通 CLI 的 `save` 不会更新平台配置，OPNsense 上应通过页面或平台 API 保存。

两个原生权限项分别为：

- **Services: Veil: Configuration**：查看含凭据的配置、保存和控制运行状态。
- **Services: Veil: Status**：仅显示状态；隐藏配置界面，拒绝配置读取和运行修改。

页面使用系统认证、CSRF 保护和 ACL。配置保存、启动、重启携带 `expected_revision`，旧页面无法覆盖后续保存或应用未经确认的新配置。读取状态不会返回连接凭据。

前端经 configd 调用固定动作；配置正文通过 root 私有的短期请求文件传递，命令参数仅含随机请求 ID。请求处理或调用结束时删除文件。PHP 只处理配置和状态，代理流量由 Go 核心转发。

```sh
configctl veil status
configctl veil stop
# 按 config.xml 中的开机策略恢复状态：
configctl veil configure
```

原生 API（沿用 OPNsense 会话或 API key 认证）：

| 端点 | 请求与结果 |
| --- | --- |
| `GET /api/veil/service/connections` | 多连接脱敏状态、路径、入口与诊断 |
| `POST /api/veil/settings/connection` | `request` 为控制 v1 单连接操作的 JSON 字符串；配置保存先写入 config.xml |
| `GET /api/veil/settings/get` | 返回 JSON 配置字符串 `profile`、布尔 `enabled`、平台 `revision` |
| `POST /api/veil/settings/validate` | `profile` 和字符串 `enabled`（`"0"`/`"1"`）；执行核心校验 |
| `POST /api/veil/settings/save` | 同上，增加 `expected_revision`；只保存平台配置 |
| `GET /api/veil/service/status` | 状态、平台 revision、开机策略及待应用状态 |
| `POST /api/veil/service/start` | `expected_revision`；同步保存配置并启动 |
| `POST /api/veil/service/restart` | `expected_revision`；同步保存配置并明确重启 |
| `POST /api/veil/service/stop` | 空对象；停止代理，保留管理入口 |

## 构建原生包

使用 Go 1.26.3、Python 3 和对应分支的[官方 plugins 源码](https://github.com/opnsense/plugins)。在仓库根目录构建静态 FreeBSD amd64 程序并打包构建材料：

```sh
python3 veil-service/scripts/opnsense_bundle.py \
  --plugins /path/to/opnsense-plugins --version 0.3.3
```

将 `veil-service/.build/opnsense/veil-opnsense-build-0.3.3.tar.gz` 放入一次性 OPNsense 构建机，解压后运行 `./build.sh`。脚本使用官方 `make package` 和本机 `pkg`，从 `opnsense-version` 取得 ABI；产物位于 `net/veil/work/pkg/os-veil-0.3.3.pkg`。框架副本和 Go 二进制只进入构建产物，不提交到源码目录。

原生构建目标为 OPNsense 26.7 / FreeBSD 15.1 amd64。包需在目标 OPNsense 版本的原生环境生成，其他版本应重复安装和运行验收。

## 验收

常规 CI 检查 PHP/JS/shell 语法、XML、中文文案覆盖、状态权限边界和 FreeBSD Go 编译；浏览器运行完整连接流程，PHP 事务测试连接真实 Go daemon，并注入配置存储与监听失败。原生包安装、升级、卸载重装、启动策略及 GUI 交互在独立 OPNsense VM 验证，不在每次 CI 下载或启动系统镜像。

`scripts/opnsense_smoke.cjs` 使用 Playwright 操作实际页面，覆盖保存与应用分离、取消重启、旧 revision 保存/应用拒绝、错误提示及桌面/手机布局。录屏从登录完成后开始，不展开配置凭据：

```sh
export VEIL_OPNSENSE_TEST=1
export VEIL_OPNSENSE_URL=https://127.0.0.1:18443/ui/veil/
export VEIL_OPNSENSE_PASSWORD='disposable-test-password'
export VEIL_OPNSENSE_CONFIG=/path/to/test-client-mixed.json
export VEIL_OPNSENSE_LANGUAGE=zh  # 系统语言也设为简体中文
export VEIL_OPNSENSE_VIDEO=1
export VEIL_OPNSENSE_OUTPUT=/path/to/ignored-artifacts
node veil-service/scripts/opnsense_smoke.cjs
```

需安装 Playwright/Chromium，或通过 `PLAYWRIGHT_MODULE`、`CHROMIUM_PATH` 指定现有工具。`VEIL_OPNSENSE_USER` 和 `VEIL_OPNSENSE_READONLY=1` 用于只读账户验收；脚本同时验证真实状态读取、配置 API 拒绝以及未授权停止不改变运行状态。

## 中转连接

完整的服务器安装、同端口多出口、连接包和双层 Veil 配置见[中转部署与连接](../../RELAY.md)。
