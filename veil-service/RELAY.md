# 部署 Veil 中转与出口

一台中转可以用同一个 Veil 端口连接多个出口。出口由客户端配置决定，中转收到认证后的目标拨号请求，再连接相应的出口；增加东京、洛杉矶等连接不需要给中转增加监听端口。

```text
应用 → 本地 SOCKS5 / HTTP → Veil 客户端
                              │ 外层 Veil：客户端 ↔ 中转
                              ▼
                    中转 R:55423
                              │ 承载内层 Veil：客户端 ↔ 出口
                              ▼
                       出口 E:443 → 目标网站
```

中转和出口都运行普通的 Veil **server**。客户端持有两套服务器连接信息，先通过中转连接出口，再与出口握手。中转会知道出口地址；内层代理请求和数据在客户端与出口之间加密。中转端不需要保存出口认证密钥。下文以 REALITY 为例，两层也可分别使用标准 TLS。

## 1. 准备两套独立凭据

在有 Veil CLI 的管理机上执行。源码构建用 `make build`，将下列 `veil` 替换为 `veil-core/.build/veil`；Linux 服务发行包中的 `veild` / `veilctl` 不提供 `-keygen`。

```sh
umask 077
veil -keygen > relay-keys.json
veil -keygen > exit-keys.json
```

每个文件包含 `secret`、`reality_private_key`、`reality_public_key` 和 `short_id`。分别为中转 R 和出口 E 使用对应的一套值。私钥只放在对应服务端；客户端使用 `secret`、公钥和 `short_id`。

下文的 `relay.example.net`、`exit.example.net` 和 `cover.example` 都是占位符。中转与出口地址替换为实际 IP 或域名；回落站点应替换为该服务器能正常访问、支持所需 TLS 1.3 握手的站点。`server_name` 与该站点证书名称匹配，客户端使用相同名称。

## 2. 在中转和出口运行服务端

在两台服务器分别安装 [Linux 服务发行包](platform/systemd/README.md)。中转保存如下 `relay-server.json`：

```json
{
  "role": "server",
  "listen": "0.0.0.0:55423",
  "secret": "RELAY_SECRET",
  "tls": {
    "mode": "reality",
    "server_name": "cover.example",
    "cover_address": "cover.example:443",
    "reality_private_key": "RELAY_PRIVATE_KEY",
    "short_id": "RELAY_SHORT_ID",
    "record_padding": true
  }
}
```

出口的 `exit-server.json` 使用相同结构，把 `listen` 改为 `0.0.0.0:443`，换成出口自己的密钥和回落站点。这里的地址是 IPv4；IPv6 监听可用 `[::]:443` 并核对主机的双栈设置。

在中转执行：

```sh
sudo veil-system relay enable
sudo veil-system relay validate /path/to/relay-server.json
sudo veil-system relay import /path/to/relay-server.json
sudo veil-system relay start
sudo veil-system relay status
```

在出口执行：

```sh
sudo veil-system exit enable
sudo veil-system exit validate /path/to/exit-server.json
sudo veil-system exit import /path/to/exit-server.json
sudo veil-system exit start
sudo veil-system exit status
```

中转的云安全组及主机防火墙放行 **TCP 55423**；出口放行 **TCP 443**。中转需能连接出口和自身回落站点，出口需能连接目标网站及自身回落站点。Veil 不自动修改这些规则。已有实例更新配置用 `import` 后 `restart`；`start` 不会应用运行中的新配置。

## 3A. 桌面 / LuCI / OPNsense：导入完整连接包

在 **服务 → Veil** 中，也可以分两步填写：先“添加中转”，填写 R 的地址、公钥、密钥和 SNI；再“添加连接”，填写 E 的信息，在“连接路径”选择这个中转，添加本地 mixed / SOCKS5 / HTTP 入口并启用。这里“添加中转”保存的是**客户端连接信息**，不会替你在远程服务器安装服务。

跨设备迁移更方便的方式是导入下面的自包含 JSON。所有占位符都需替换，整个包包含认证凭据。

```json
{
  "version": 1,
  "profile": {
    "id": "tokyo",
    "name": "东京，经中转",
    "kind": "connection",
    "enabled": true,
    "relay_id": "relay-main",
    "config": {
      "role": "client",
      "server": "exit.example.net:443",
      "secret": "EXIT_SECRET",
      "tls": {
        "mode": "reality",
        "server_name": "exit-cover.example",
        "reality_public_key": "EXIT_PUBLIC_KEY",
        "short_id": "EXIT_SHORT_ID",
        "fingerprint": "chrome149",
        "record_padding": true
      }
    },
    "inlets": [{"protocol": "mixed", "listen": "127.0.0.1:1080"}]
  },
  "relay": {
    "id": "relay-main",
    "name": "公共中转",
    "kind": "relay",
    "enabled": false,
    "config": {
      "role": "client",
      "server": "relay.example.net:55423",
      "secret": "RELAY_SECRET",
      "tls": {
        "mode": "reality",
        "server_name": "cover.example",
        "reality_public_key": "RELAY_PUBLIC_KEY",
        "short_id": "RELAY_SHORT_ID",
        "fingerprint": "chrome149",
        "record_padding": true
      }
    },
    "inlets": []
  }
}
```

各界面导入后会展示出口与中转，重新生成 ID，并默认停用新连接；确认地址、入口、勾选启用后“保存并应用”。中转本身没有启用开关或本地入口，随引用它的连接使用；`relay.enabled` 应为 `false`。第二个出口连接选择相同中转，另用本地端口，例如 1081。供 LAN 使用时，将监听 IP 改为路由器的 LAN 地址；本地代理没有用户认证，不要暴露到公网。

连接卡片的“更多 → 导出”会把完整连接和中转一起导出。客户端靠连接包中的信息展示路径，不需要预先知道云厂商或地域。

## 3B. 普通 Linux CLI：使用多连接管理

在网关机器安装 `veild` 和 `veilctl`，用独立状态目录启动多连接 daemon。以下以前台运行示例说明，目录归当前用户所有：

```sh
mkdir -p ~/.local/state/veil-chain ~/.local/run/veil-chain
chmod 700 ~/.local/state/veil-chain ~/.local/run/veil-chain
veild -connections -autostart \
  -state-dir "$HOME/.local/state/veil-chain" \
  -socket "$HOME/.local/run/veil-chain/control.sock"
```

另一个终端导入 3A 的完整文件 `tokyo.json`，并应用：

```sh
veilctl -socket "$HOME/.local/run/veil-chain/control.sock" \
  -config tokyo.json -if-revision '' -apply connection_save
veilctl -socket "$HOME/.local/run/veil-chain/control.sock" connections
veilctl -socket "$HOME/.local/run/veil-chain/control.sock" -id tokyo connection_test
```

CLI 保留文件中的 ID；空 revision 仅用于新建。修改已有配置时用 `connections` 返回的对应 revision 替换空字符串。`enabled: true` 和 `-apply` 使入口立即启动；重启 daemon 时 `-autostart` 启动这些已启用连接。

需要 systemd 常驻时，在服务管理器中使用上述 `-connections -autostart` 命令及专用私有目录。发行包现有 `veil@.service` 模板是**单配置模式**，不能直接用它导入连接包；单配置实例可以采用下面的固定转发方式。OpenWrt 的 procd 已配置多连接模式。

## 3C. 单配置实例：两个本地实例串联

需要手动管理两个独立进程时，可以运行一个外层固定转发实例，再运行内层代理。普通 Linux 上可使用两个 `veil-system` 实例；图形界面直接使用 3A 的完整连接包。

外层 `outer.json`：连接 R，并将本地临时端口固定转发到 E。

```json
{
  "role": "client",
  "listen": "127.0.0.1:12001",
  "server": "relay.example.net:55423",
  "target": "exit.example.net:443",
  "secret": "RELAY_SECRET",
  "tls": {
    "mode": "reality",
    "server_name": "cover.example",
    "reality_public_key": "RELAY_PUBLIC_KEY",
    "short_id": "RELAY_SHORT_ID",
    "fingerprint": "chrome149",
    "record_padding": true
  }
}
```

运行 `veil -config outer.json`；或在 Linux 上用 `veil-system outer enable/import/start` 管理它。固定转发配置不设置 `inbound`。

内层 `inner.json`：使用 **E 的认证和 SNI**，但拨号地址设为本地外层端口。

```json
{
  "role": "client",
  "listen": "127.0.0.1:1080",
  "inbound": "mixed",
  "server": "127.0.0.1:12001",
  "secret": "EXIT_SECRET",
  "tls": {
    "mode": "reality",
    "server_name": "exit-cover.example",
    "reality_public_key": "EXIT_PUBLIC_KEY",
    "short_id": "EXIT_SHORT_ID",
    "fingerprint": "chrome149",
    "record_padding": true
  }
}
```

先启动外层，再运行 `veil -config inner.json`。Windows 使用 `veil.exe`。不要让两个进程共用状态目录或监听端口。

这里 12001 是本机固定转发口，不是 SOCKS/HTTP 口，也不需要在中转云安全组开放。每增加一个出口，使用另一组本地外层/内层端口；所有外层仍连接 R:55423。单配置界面只知道本地转发地址，完整路径以这两份配置为准；需要自动展示路径可使用 3A/3B 的连接包方式。

## 4. 验证与排查

先从客户端测试出口直连（若网络允许），再测试完整中转路径。直连时客户端 `server` 用 E 的公网地址；经过中转时使用 3A 的 `relay_id` 或 3C 的本地转发口。不能只凭“代理运行中”判断远端握手已成功。

```sh
curl --fail --show-error --max-time 15 --noproxy '' \
  --proxy socks5h://127.0.0.1:1080 https://www.google.com/generate_204
curl --fail --show-error --max-time 15 --noproxy '' \
  --proxy http://127.0.0.1:1080 https://www.google.com/generate_204
```

成功的 204 响应没有正文。各界面在**完整连接**上点击“测试 Google”，测试包含两层握手与 HTTPS 请求；不能单测一份未指定出口的中转配置。

| 现象 | 检查 |
| --- | --- |
| 本地连接被拒绝 | 本地实例是否运行、监听 IP/端口是否正确、外层固定转发是否先启动 |
| 中转地址超时 | R:55423 安全组、防火墙、监听地址、客户端到 R 的可达性 |
| 经过中转失败、出口直连成功 | R 到 E:443 的可达性；外层使用 R 的凭据，内层使用 E 的凭据 |
| REALITY 握手失败 | 公私钥、secret、short_id、SNI 对应关系；服务器到回落站点的连接 |
| 保存后仍走旧路径 | 点击“保存并应用”，或对已运行的单配置实例执行 `restart` |
| 待应用配置与测速结果不同 | 多连接测试使用已保存配置；先应用，再确认正在使用的入口路径 |

服务器日志用 `sudo veil-system relay logs --follow`、`sudo veil-system exit logs --follow`；本地多连接用 `veilctl ... connections` 查看各连接错误。迁移客户端时带走完整连接包或外层/内层两份配置，并重新选择本地入口地址即可，服务器端口不变。
