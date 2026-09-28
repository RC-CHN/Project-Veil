# Project Veil

Veil 是基于 TCP 与 TLS 1.3/REALITY 的实验代理。目前提供可复用 Go 核心、SOCKS5 CONNECT、固定目标 TCP 转发和 CLI。尚未提供 TUN、UDP 或平台界面，也尚未通过抗识别验收。

当前线协议为 v0.3，两端需同时升级。语言无关的字节格式、密码计算、状态机和固定向量见 [协议规范](PROTOCOL.md)；独立 Rust 实现及双向互通用法见 [互通验证端](interop/rust/README.md)。

仓库按组件组织，核心与控制服务分别为独立 Go module：

```text
veil-core/
  core/       公共 API：客户端、服务端、流、连接池
  inbound/    SOCKS5 和固定目标 TCP 入口
  service/    配置校验、应用组装和可嵌入运行生命周期
  internal/   TLS、并发复用、鉴权和 SOCKS 解析
  cmd/        Veil CLI 与测试程序的 main 包
  scripts/    构建、协议检查和性能测试
  patches/    固定版本的 TLS 补丁
  examples/   配置示例
veil-service/
  control/    配置保存、revision 和运行控制
  local/      私有 Unix socket 控制协议
  cmd/        veild 常驻服务、veilctl 控制命令
  platform/   systemd、OpenWrt/procd、FreeBSD/rc.d 模板
```

`internal` 使用 Go 的导入限制，封装不对外承诺稳定的实现；可嵌入接口从 `veil/core`、`veil/inbound` 和 `veil/service` 使用。核心不依赖界面，不操作系统路由、DNS、防火墙或服务管理。

后续 LuCI、Windows/Linux Tauri 桌面、Android 应用分别作为独立组件目录加入；OpenWrt/procd、Linux/systemd、OPNsense 和 Windows 的平台适配负责配置、权限、网络与服务生命周期。它们复用同一核心。公共控制服务和三个启动适配见 [服务组件说明](veil-service/README.md)；平台 UI、网络集成和安装包尚待完成。

## 构建与检查

需要 Go 1.26.3、Python 3；集成测试另需 OpenSSL。仓库根目录可以执行：

```sh
make build
make test
make race
make vet
make service       # 构建 veild、veilctl
make service-race  # 控制层测试、race 和 vet
veil-core/.build/veil -keygen
veil-core/.build/veil -profilegen
veil-core/.build/veil -config /path/to/client.json
```

生成文件位于 `veil-core/.build/`。脚本和具体配置用法见 [核心说明](veil-core/README.md)，固定目标入口示例见 [client.forward.json](veil-core/examples/client.forward.json)。直接 `go build` 不包含必需的 uTLS 补丁，请使用构建器；独立的可发布 SDK 与移动端绑定仍待后续完成。

源代码整理不会迁移或重启已有部署；部署目录与构建目录应各自管理。

本项目使用 [GPL-3.0-or-later](LICENSE)，来源见 [第三方说明](THIRD_PARTY.md)。
