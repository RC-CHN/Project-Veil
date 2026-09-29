# Project Veil

[![CI](https://github.com/RC-CHN/Project-Veil/actions/workflows/ci.yml/badge.svg)](https://github.com/RC-CHN/Project-Veil/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/RC-CHN/Project-Veil)](https://github.com/RC-CHN/Project-Veil/releases/latest)
[![License: GPL-3.0-or-later](https://img.shields.io/badge/license-GPL--3.0--or--later-blue.svg)](LICENSE)

Veil 是基于 TCP 与 TLS 1.3 的代理协议与工具集。它在加密连接中复用独立业务流，通过范围式随机调度和 TLS 记录填充组织流量，并提供可嵌入核心、命令行入口与公共控制服务。

## 协议思路

- **连接绑定认证**：使用 TLS exporter 将业务认证绑定到当前加密会话。传输支持标准 TLS 和 REALITY 式握手；后者将服务端认证证明放入证书扩展，使用客户端声明的签名算法。
- **并发多路复用**：每条 TLS 连接承载多个业务流，每流独立处理目标拨号确认、接收额度、取消、双向半关闭和完成确认。
- **范围式随机化**：外层生成和分发参数范围，核心在连接建立及发送时取样。上下行分别配置调度份额、启动分片与额度返还策略。
- **记录层填充**：在 TLS 记录层执行有预算的随机填充，配合真实业务流交织；发送策略与业务帧的解析规则分离。
- **握手外观**：创建连接时选择浏览器 ClientHello 模板，服务端为普通未认证连接提供网站回落。

## 与常见协议的思路对比

| 协议 / 组合 | 认证与加密 | 连接组织 | 流量外观与随机化思路 |
| --- | --- | --- | --- |
| **Veil** | TLS 1.3；业务认证绑定 TLS exporter；REALITY 式握手使用证书扩展证明 | 原生并发多流，每流独立额度、半关闭与完成确认 | 浏览器握手模板、网站回落；双向范围采样、真实流交织、TLS 记录层预算填充 |
| [AnyTLS](https://github.com/anytls/anytls-go/blob/main/docs/protocol.md) | TLS 内使用密码摘要认证 | TLS 上的会话层和连接复用 | 按 PaddingScheme 拆分与填充首段写入，通过填充帧补足尺寸；服务器可下发更新策略 |
| [NaiveProxy](https://github.com/klzgrad/naiveproxy#padding-protocol-an-informal-specification) | HTTPS 代理认证，使用 Chromium 的 TLS/QUIC 网络栈 | HTTP/2 或 HTTP/3 CONNECT 流复用 | 沿用浏览器网络栈，对 CONNECT 头、流起始数据和特定控制帧做填充 |
| [Trojan](https://trojan-gfw.github.io/trojan/protocol.html) | TLS 证书认证，隧道内校验密码摘要 | 目标 TCP 连接与 TLS 隧道对应 | 使用 TLS 站点外观，首个请求携带业务数据；认证不匹配时回落到网站服务 |
| [VLESS + REALITY](https://github.com/XTLS/REALITY#readme) | VLESS 用户身份配合 REALITY 握手认证 | VLESS 请求层与传输层组合，复用方式由具体组合决定 | 借用目标站点的握手外观并提供回落；可结合 [Vision](https://xtls.github.io/en/config/inbounds/vless.html) 的首段填充及内层 TLS 处理 |
| [VMess AEAD](https://www.v2fly.org/developer/protocols/vmess.html) | 用户 ID 派生认证与会话密钥，协议头和数据分块受保护 | 分块数据流，可结合 [Mux.Cool](https://www.v2fly.org/developer/protocols/muxcool.html) 复用 | 长度掩码与随机填充，外层可组合 TLS、WebSocket 等传输 |
| [Shadowsocks 2022](https://shadowsocks.org/doc/sip022.html) | 预共享密钥派生会话子密钥，使用分块 AEAD | 目标 TCP 连接与代理连接一一对应 | 原生密文流，请求首段包含随机长度填充 |

Veil 将握手外观、会话认证、流生命周期和发送策略分别处理：连接创建时确定握手模板，认证后由复用层管理业务流，发送端按本地参数范围组织数据和填充。参数由外层配置生成器提供，核心负责校验与执行。

## 组件

| 目录 | 职责 |
| --- | --- |
| [`veil-core/`](veil-core/README.md) | 公共 Go API、TLS 传输、并发复用、SOCKS5、HTTP/HTTPS CONNECT、混合入口、固定目标 TCP 转发与 Veil CLI |
| [`veil-service/`](veil-service/README.md) | `veild` / `veilctl`、配置保存与 revision、运行生命周期、私有 Unix socket / Windows 命名管道控制接口 |
| [`veil-service/platform/`](veil-service/platform/) | Linux 安装包与 systemd 管理、OpenWrt 双语 LuCI、OPNsense 原生插件与 FreeBSD rc.d 适配 |
| [`veil-desktop/`](veil-desktop/README.md) | Windows / Linux 桌面端，配置导入、双语界面、启停与诊断、托盘和系统代理管理 |
| [`veil-rust/`](veil-rust/README.md) | 实验性独立 Rust 核心（Veil v0.3 / T 档位），用于性能对照与替代实现探索 |
| [`interop/rust/`](interop/rust/README.md) | 独立 Rust 互通验证端，覆盖客户端和服务端角色 |

核心和控制服务分别为独立 Go module。应用通过 `veil/core`、`veil/inbound` 和 `veil/service` 使用核心；平台组件通过公共控制层管理配置与运行状态，代理数据由核心直接转发。

## 构建与使用

预编译安装包见 [GitHub Releases](https://github.com/RC-CHN/Project-Veil/releases/latest)，按平台选择 CLI、桌面或路由器组件；每个发行包附 SHA-256 校验和。

使用 Go 1.26.3 和 Python 3；集成测试使用 OpenSSL 3.5 与 curl。所有 Go 构建必须经过仓库构建器，它负责生成固定版本的 TLS 适配补丁；直接 `go build` 无法编译。

```sh
make build           # 核心与 CLI
make service         # veild、veilctl
make test            # 核心测试
make race            # 核心竞态检查
make service-race    # 服务竞态检查
```

核心构建输出位于 `veil-core/.build/`，控制服务输出位于 `veil-service/.build/`。配置示例见 [`veil-core/examples/`](veil-core/examples/)，固定目标入口见 [client.forward.json](veil-core/examples/client.forward.json)。

```sh
veil-core/.build/veil -keygen        # 生成 REALITY 密钥对
veil-core/.build/veil -profilegen    # 生成随机化流量策略
veil-core/.build/veil -config /path/to/client.json
```

实验性 Rust 核心需要 Rust 1.96.0：

```sh
cargo build --manifest-path veil-rust/Cargo.toml --release --locked
```

## 文档

- [线协议规范](PROTOCOL.md)：字节格式、认证计算、流状态机和固定向量。
- [核心使用说明](veil-core/README.md)：配置、嵌入 API 和传输策略。
- [控制服务说明](veil-service/README.md)：配置保存、启停控制和平台适配。
- [Linux 安装与管理](veil-service/platform/systemd/README.md)：发行包、安装卸载、实例管理与日志。
- [OpenWrt / LuCI](veil-service/platform/openwrt/README.md)：SOCKS5 与 HTTP 入口、软件包、配置导入与双语界面。
- [OPNsense 插件](veil-service/platform/opnsense/README.md)：config.xml 配置、configd 控制、原生双语界面与安装包。
- [桌面安装与使用](veil-desktop/README.md)：Windows / Linux 发行包、配置导入、托盘和系统代理管理。
- [Windows CLI](veil-service/platform/windows/README.md)：amd64 / ARM64 便携包、私有命名管道与实例管理。
- [Rust 实验核心](veil-rust/README.md)：独立 Rust 实现的功能范围、构建与[实测数据](veil-rust/BENCHMARK.md)。
- [Rust 互通验证](interop/rust/README.md)：独立实现的构建与双向互通检查。
- [CI 说明](.github/CI.md)：并行任务、构建缓存和跨平台检查。

## 许可

本项目使用 [GPL-3.0-or-later](LICENSE)，代码来源见 [第三方说明](THIRD_PARTY.md)。
