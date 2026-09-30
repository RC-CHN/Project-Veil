# Veil service

`veild` 将一份 Veil 配置和运行实例组合为本地常驻服务，`veilctl` 提供结构化控制入口。独立 Go module，通过 `veil/service` 复用核心；服务管理器、界面和数据转发互不依赖。每个 daemon 管理一份配置，多实例通过不同的私有状态目录和 socket 区分。

```text
LuCI / 桌面 / CLI → 平台通信适配 → control.Manager → service.Runtime → core / inbound
                     Unix socket       配置与状态         生命周期       数据转发
systemd / procd / rc.d ───────────────→ 启动、停止 veild 进程
```

控制请求不会承载代理数据。Linux、OpenWrt、FreeBSD 共用 Unix socket；Windows 使用按用户隔离的本机命名管道。公共 `service.Runtime` 没有 systemd、Unix socket、持久化路径或 HTTP 依赖。需要平台 socket protect 时可注入 `core.ClientConfig.DialContext`。

## 构建与试运行

systemd Linux 的发行包构建、安装、多实例管理和卸载见 [Linux 安装与管理](platform/systemd/README.md)。`veil-system` 提供实例管理、配置导入、状态和日志入口；公共 `veilctl` 保持独立于 systemd。OpenWrt 安装、双语 LuCI 和 SOCKS5/HTTP 入口见 [OpenWrt / LuCI](platform/openwrt/README.md)。

Go 1.26.3、Python 3；沿用核心构建器的固定 TLS 补丁。以下在 `veil-service/` 执行：

```sh
make build                  # 默认 batch 后端，无 cgo
make BACKEND=openssl build  # Linux 服务端；需要 OpenSSL 开发库
make race
python3 scripts/cross_check.py
```

输出 `.build/veild` 和 `.build/veilctl`。切换后端会覆盖这些构建输出，部署时应复制到独立路径。不要直接 `go build`，不要把运行部署放在 `.build` 清理目录。

在两个终端中，用空闲监听端口和测试配置试运行：

```sh
# 终端 1；state/run 必须为当前用户独占的 0700 目录。
.build/veild -state-dir /tmp/veil-example/state -socket /tmp/veil-example/run/control.sock

# 终端 2；所有 flags 放在动作之前。
.build/veilctl -socket /tmp/veil-example/run/control.sock -config /path/to/client.json validate
.build/veilctl -socket /tmp/veil-example/run/control.sock -config /path/to/client.json save
.build/veilctl -socket /tmp/veil-example/run/control.sock start
.build/veilctl -socket /tmp/veil-example/run/control.sock status
.build/veilctl -socket /tmp/veil-example/run/control.sock stop
```

`validate` 校验 JSON、地址格式、TLS 参数和本地证书文件，不拨号，不保证远端可达。配置沿用核心 JSON 示例，证书路径应使用 daemon 可读取的绝对路径。原 CLI 参数和有效配置保持兼容。

## 保存与运行边界

| 操作 | 行为 |
| --- | --- |
| `validate` | 只校验，返回规范化配置的 SHA-256 revision |
| `save` | 校验后以 0600 临时文件、fsync、rename 原子保存；不改变运行实例 |
| `start` | 未运行时启动；已经运行时幂等，不应用待生效配置 |
| `stop` | 关闭监听、取消现有连接并等待退出；保留已保存配置 |
| `restart` | 明确中断旧连接，以保存配置启动；先校验，再停旧实例 |
| `config` | 显式返回已保存配置和同一时刻的状态/revision；包含凭据，需配置管理权限 |
| `status` | 返回运行状态、监听地址、计数和保存/运行 revision；不返回配置或密钥 |

运行中保存不同配置后，`restart_required` 为 true。`restart` 后若新端口被占用，实例保持 stopped 并报告错误；不宣称端口切换无缝或自动回滚。`-autostart` 仅在 daemon 启动时启动已有配置；没有配置则等待导入，启动失败也保留控制入口以便修复。`stop` 只停止本次运行，重启带 `-autostart` 的 daemon 会再次启动保存配置。

`status.last_connection_error` 可选，保留本次运行最近一条转发或拨号操作错误，含失败方向/操作，可能包含底层网络地址；不包含业务载荷或配置密钥。它不表示整个实例停止，实例重启后清空。`veilctl` 与 `veild` 应配套升级。

仅通过 `save` 更新配置。运行期间手工改 `config.json` 不会热加载。配置文件或目录损坏会在 daemon 启动时明确报错。若 rename 后目录 fsync 失败，返回 `durability_uncertain` 和已提交的 revision，表示磁盘掉电持久性未确认，内存状态已与文件保持一致。

## 本地控制协议 v1

本地通信采用私有 Unix socket 或 Windows 命名管道。Windows 的 ACL、CLI 使用及测试入口见 [Windows CLI](platform/windows/README.md)。Unix 目录 0700、socket 0600，只允许 daemon 用户和 root 访问。状态目录进程锁阻止多 daemon 同时管理同一配置；socket 锁阻止覆盖正在运行的监听。仅在确认 connection refused 后清理崩溃遗留 socket，不覆盖普通文件。

每条本地控制连接发送一行 JSON，以换行结束；服务端返回一行 JSON 后关闭连接。请求示例：

```json
{"version":1,"action":"status"}
```

`save`/`validate` 用 `config` 携带原配置对象，最大 1 MiB。`expected_revision` 可作比较后更新保护，防止多个界面覆盖彼此的配置；`veilctl -if-revision HASH ...` 对应此字段。不支持的版本、请求中的未知字段、同一行的多余对象会被拒绝；每条连接只处理第一行。响应总含 `version`，成功含 `status`，失败含 `error.code` 与 `error.message`；生命周期和保存错误尽可能附带当前状态。本版客户端允许响应增加可选字段，同时保留已知字段类型、协议版本和 4 MiB 响应上限校验；旧客户端升级时应一起更新，以支持后续诊断扩展。收到超时后应先查 `status`，不要盲目重复 `restart`。此版本没有热重载、事件推送或跨重启请求去重。

最多同时处理 16 个控制连接，每个连接有 15 秒截止时间；不在后台轮询核心。权限检查依靠私有目录和 socket。浏览器页面不能直接访问该 socket；LuCI 通过 rpcd 接入，OPNsense 通过 configd 桥接，其他界面也应使用本地命令或平台 RPC；不要转发成无鉴权 TCP 接口。

## 平台适配

| 平台 | 本轮内容 | 尚待完成 |
| --- | --- | --- |
| systemd Linux | 静态发行包、安装/卸载、`veil-system` 实例管理；Ubuntu 22.04 systemd 容器验收 | 其他发行版及 ARM 设备实测 |
| OpenWrt | procd、rpcd/ubus、双语 LuCI、SOCKS5/HTTP 软件包；OpenWrt 24.10.8 x86_64 虚拟机实际验收，含 Argon | ARM/MIPS 实机验收 |
| OPNsense / FreeBSD | 原生插件、config.xml/configd、双语 GUI、权限分离、FreeBSD rc.d；26.7 amd64 虚拟机验收 | 其他 OPNsense 版本验收 |
| Windows | 私有命名管道、用户 ACL、原生 CLI 生命周期测试、amd64/ARM64 便携包；Wails 桌面与系统代理控制 | 稳定 Windows 环境中的完整桌面托盘交互验收 |
| Android | 公共 service/control 包编译检查 | VpnService、socket protect 绑定、TUN/DNS、应用 |

启动脚本只管理进程，不创建路由、DNS、防火墙或系统代理规则。OpenWrt 使用 procd，OPNsense 使用 configd 与 rc.d；Windows 使用用户私有 ACL、本机命名管道及原子替换后端，复用同一公共控制层。

systemd 模板预期 `/usr/local/bin/veild`，使用专用 `veil` 用户；`veil.conf` 是供 `systemd-sysusers` 使用的声明。模板提供实例私有的 `/var/lib/veil-NAME` 和 `/run/veil-NAME`，管理员通过 `sudo veilctl -socket /run/veil-NAME/control.sock ...` 管理，配置可用 `-config -` 从 stdin 导入。模板允许绑定低端口，不授予路由管理权限。源码构建和检查不会安装或启动这些模板。

OpenWrt 模板预期 `/usr/sbin/veild`，保存配置于 `/etc/veil`，socket 在 `/var/run/veil/control.sock`，由 procd 负责异常重启。没有配置变更自动重启钩子，保持保存与应用分离。

FreeBSD 模板预期 `/usr/local/bin/veild`，默认以 root 运行（基础 rc.d 适配），启用项为 `veil_enable="YES"`，默认目录 `/var/db/veil`、`/var/run/veil`。它使用 [FreeBSD daemon(8)](https://man.freebsd.org/cgi/man.cgi?query=daemon&sektion=8) 的子进程 PID 文件配合 rc.d 停止，没有启用 daemon 的自动重启选项，避免停止子进程后被立即拉起。目录项请使用无空格的绝对路径。[OPNsense 插件](platform/opnsense/README.md) 由 config.xml 持久保存配置，通过 configd 同步到独立的 daemon 运行目录。设置 `veil_autostart=NO` 后，rc.d 只启动管理服务，代理开机策略由平台启动钩子执行。

## 回归与性能

```sh
make race
python3 scripts/cross_check.py
# 隔离网络中运行实际进程检查：
VEIL_ISOLATED_NETNS=1 python3 scripts/smoke.py
```

核心测试覆盖真实转发、半关闭、池复用、运行生命周期、curl 提前 EOF/RST、下载停顿及双向背压；组件测试覆盖保存不生效、revision 冲突、并发控制、关闭后禁止启动、持久保存、请求边界、socket 权限/占用/遗留恢复。systemd/procd/rc.d 模板检查不能替代相应操作系统安装与运行验收。

`scripts/regression.py` 复用核心性能夹具，对同一 TLS 后端的原 CLI 与 veild 做随机相邻 A/B，并检查两个方向互通。需要先构建核心 `veil-native`、`veil-batch`、`benchpeer` 和 batch 版 veild；必须在独立网络命名空间设 `VEIL_ISOLATED_NETNS=1`，例如调用 `python3 scripts/regression.py --out .build/perf-run`。原始样本、汇总和二进制摘要都写入指定目录。结果仅代表本机回环和测试工作负载，不代表 WAN、ARM 或抗识别验收。

`veilctl profilegen` 不需要控制 socket，在本机生成范围式 `traffic` 配置，供外层随连接信息分发；客户端与服务端各自设置本地发送策略。字段、预算及生效时机见[核心说明](../veil-core/README.md)。保存包含新策略的配置后仍需显式 restart，现有连接不会在保存配置时改变策略。

## 多连接控制

`veild -connections -autostart` 使用同一私有控制 socket 管理多个具名连接，自动迁移已有单客户端配置。公共控制层不依赖 procd 或 systemd。连接集合保存在状态目录的 `connections.json`，格式为 `{"version":1,"profiles":[...]}`。

每份 profile 包含 `id`、`name`、`kind`（`connection` 或 `relay`）、`enabled`、`config`（核心客户端配置）、`inlets`（`protocol` 和 `listen` 列表），以及可选的 `relay_id`。relay 是公共配置，不自行开放入口，随引用它的连接使用。最多 64 份配置、每连接 8 个入口。

| 动作 | 参数及行为 |
| --- | --- |
| `connections` | 返回不含认证信息的连接列表、对端、中转、入口、状态和测试结果 |
| `connection_get` | `id`；返回包含凭据的 `profile` |
| `connection_save` | `profile`、`expected_revision`、`apply`；创建时 revision 为空字符串；只保存或立即应用 |
| `connection_start` / `connection_stop` | `id`、`expected_revision`；持久化启用状态并应用 |
| `connection_delete` | `id`、`expected_revision`；删除配置并停止入口，拒绝删除被引用的中转 |
| `connection_test` | `id`；对固定 Google HTTPS 地址进行最多 8 秒的完整链路请求，返回 `probe` |

所有写操作要求 revision。名称修改保留运行实例；同一对端下入口变动复用池并保留未移除入口的流。改变对端或中转会重建受影响的连接。多连接响应使用 `connections` / `profile` / `probe`，原单实例 API 保持不变。

运行中配置尚待应用时，测试使用保存的配置临时建立链路，随后关闭；配置已生效时复用现有连接池。停止的连接也可测试，测试不会启用其 LAN 监听端口。Google 耗时是 HTTPS 请求时间，包含需要新建的隧道和 TLS 握手。

CLI 用 `-id` 指定连接，`-config FILE` 提供 profile，`-apply` 要求立即应用；`-if-revision HASH` 提供并发修改保护。配置文件与导出文件含凭据，应保存在私有目录。

### 自包含连接包

`connection_export`（`id`）返回 `{ "version": 1, "profile": { ... }, "relay": { ... } }`，直连省略 `relay`。导出将 CA 文件转换为 `tls.ca_pem`，只携带证书 PEM 块。`connection_save` 可额外接受 `relay`：它的 ID 必须等于 `profile.relay_id`，两者一起验证和持久保存；已有同 ID 中转必须与导入配置相同，禁止静默覆盖。`expected_revision` 仍约束主连接。LuCI 导入为两者生成新 ID，CLI 导入保持文件中的 ID。

```sh
veilctl -socket /var/run/veil/control.sock -id office connection_export > office.json
veilctl -socket /var/run/veil/control.sock -config office.json -if-revision '' connection_save
```

连接包包含认证信息；只向有配置写入权限的调用方开放导出。`connection_test` 仅接受完整连接，不接受独立中转配置。

## 中转连接

完整的服务器安装、同端口多出口、连接包和双层 Veil 配置见[中转部署与连接](RELAY.md)。

连接故障定位、分层诊断和日志读取见[连接诊断](DIAGNOSTICS.md)。
