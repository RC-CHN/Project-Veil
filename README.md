# Project Veil

Veil 是基于 TCP 与 TLS 1.3/REALITY 的实验代理。目前提供可复用 Go 核心、SOCKS5 CONNECT、固定目标 TCP 转发和 CLI。尚未提供 TUN、UDP 或平台界面，也尚未通过抗识别验收。

仓库按组件组织。当前实现集中在一个 Go module 中：

```text
veil-core/
  core/       公共 API：客户端、服务端、流、连接池
  inbound/    SOCKS5 和固定目标 TCP 入口
  internal/   TLS、帧编码、SOCKS 解析和 CLI 配置组装
  cmd/        Veil CLI 与测试程序的 main 包
  scripts/    构建、协议检查和性能测试
  patches/    固定版本的 TLS 补丁
  examples/   配置示例
```

`internal` 使用 Go 的导入限制，封装不对外承诺稳定的实现；可嵌入接口从 `veil/core` 和 `veil/inbound` 使用。核心不依赖界面，不操作系统路由、DNS、防火墙或服务管理。

后续 LuCI、Windows/Linux Tauri 桌面、Android 应用分别作为独立组件目录加入；OpenWrt/procd、Linux/systemd、OPNsense 和 Windows 的平台适配负责配置、权限、网络与服务生命周期。它们复用同一核心，本轮未创建这些平台组件。

## 构建与检查

需要 Go 1.26.3、Python 3；集成测试另需 OpenSSL。仓库根目录可以执行：

```sh
make build
make test
make race
make vet
veil-core/.build/veil -keygen
veil-core/.build/veil -config /path/to/client.json
```

生成文件位于 `veil-core/.build/`。脚本和具体配置用法见 [核心说明](veil-core/README.md)，固定目标入口示例见 [client.forward.json](veil-core/examples/client.forward.json)。直接 `go build` 不包含必需的 uTLS 补丁，请使用构建器；独立的可发布 SDK 与移动端绑定仍待后续完成。

源代码整理不会迁移或重启已有部署；部署目录与构建目录应各自管理。

本项目使用 [GPL-3.0-or-later](LICENSE)，来源见 [第三方说明](THIRD_PARTY.md)。
