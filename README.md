# Project Veil

Veil 是基于 HTTPS 会话行为模型的加密代理传输项目。正式工程已迁到本仓库根目录；当前工程候选为 **0.5.0-engineering1**，线路保持 Config5 / FlightModel4 / streammux-flight-v3。

## 目录

```text
Project-Veil/
├── core/                 可嵌入 Go 核心库：直接 TCP 流和 UDP 关联
├── node/                 共用后台：配置、SOCKS、生命周期、平台适配
├── apps/                 后续 Windows/Linux 界面程序的模块入口
├── api/                  后续本地管理协议的合同入口
├── integrations/         第三方 TUN 桥接与来源索引
├── packaging/            Linux、Windows、容器交付边界
├── examples/             新 node 配置示例
├── tools/                独立构建、检查、迁移保全工具
├── docs/architecture/    抽象设计与模块边界
├── docs/migration/       来源清单、实现范围和验证记录
├── third_party/          第三方版本和许可证
├── go.work               core/node 联合开发工作区
└── references/           原型、参考实现、实验及冻结证据
```

当前主要有两个 Go 模块：`veil.local/core` 和 `veil.local/node`。node 通过公开 API 引用 core；文件和系统相关代码集中在 node。core/internal/session 是本轮迁入的运行库，继续保留历史回归；进一步拆分承载与会话代码留给后续小步迁移。

## 构建和检查

需要 Go 1.26.7 和 Python 3。可用 `VEIL_GO` 指定 Go 可执行文件；项目脚本没有绑定本机缓存路径。

```sh
python3 tools/build.py
python3 tools/check.py --race --windows
python3 tools/verify_migration.py
```

构建默认生成 Linux/amd64、Windows/amd64 的 veild 和 veilctl，写入独立的 `out/builds/` 任务目录，附带源码摘要和二进制清单。检查脚本分别测试两个模块；Windows 在 Linux 上执行的是交叉构建检查。真实 Windows 运行、ACL 和系统集成仍需原生验收。

本机已有的离线工具链可这样接入：

```sh
export VEIL_GO=/tmp/veil-modcache/golang.org/toolchain@v0.0.1-go1.26.7.linux-amd64/bin/go
export GOTOOLCHAIN=local GOMODCACHE=/tmp/veil-modcache GOCACHE=/tmp/veil-engineering-go-cache
export GOPROXY=off GOSUMDB=off
```

## 当前入口

```sh
veild version
veilctl capabilities
veilctl modelgen --lanes 4
veilctl configcheck --config /path/to/node.json
veild --config /path/to/node.json
```

`modelgen` 向标准输出写入包含私有种子的模型。客户端和服务端使用同一个模型文件，存放时设置私有访问权限。配置、模型和私钥由平台文件读取器检查；Linux 私有文件应为 0600。Windows 读取器检查已打开文件的所有者和 ACL，原生验收待完成。

配置格式见 [客户端](examples/client.json) 和 [服务端](examples/server.json)。示例仅含占位地址和路径，需要自己的证书、私钥、模型和客户端证书指纹。节点配置使用独立的 `version: 1`；旧 session18 安装包和部署脚本不能直接用于新命令。

目前 veild 以前台进程运行，启动和退出输出状态 JSON；`end_to_end` 尚未做探测时保持 `not_checked`。SOCKS 入口限定环回地址。系统服务、TUN、控制 IPC、自动证书恢复和全天耐久属于后续模块。

## 开发入口

- [core API](core/README.md)
- [node 模块](node/README.md)
- [模块边界与并行开发](docs/architecture/modules.zh.md)
- [跨平台抽象原语](docs/architecture/cross-platform-primitives.zh.md)
- [本次迁移范围](docs/migration/phase1.zh.md)
- [原始交接](handoff.md)

`references/veil` 和冻结二进制保持原位。新工程能构建及转发流量，不代表完整 Veil 0.5 或抗识别评估已经交付；性能优化继续暂停。许可证见 [LICENSE](LICENSE)。
