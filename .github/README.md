# CI

GitHub Actions 在 `main`、`ci/**` 分支 push、所有 PR 和手动运行时执行。全部检查完成后的固定入口是 **CI**；分支保护应选这个汇总检查，避免依赖条件跳过或动态矩阵名称。工作流不修改分支保护，也不发布或部署软件。

| 并行任务 | 验证内容 |
| --- | --- |
| Core × 3 | native、batch、OpenSSL 各自运行协议/回环网络测试和 race；batch 额外验证标准库 TLS overlay |
| Control service | 公共控制层、持久保存、生命周期、race/vet、真实 veild/veilctl 进程、Linux 安装包、启动脚本语法及 LuCI 与 OPNsense 翻译/ACL/JS 检查、OPNsense PHP 语法 |
| Rust interoperability | fmt/clippy、规范固定向量、Rust↔Go 双向 TLS/REALITY 互通，覆盖 batch 和 OpenSSL |
| Compile × 8 | Linux ARM64/ARMv7/MIPS/MIPSLE、FreeBSD amd64、Windows amd64/ARM64、Android ARM64；核心及支持的控制组件 |

Linux amd64 已由运行测试覆盖。Windows 检查 CLI 和公共控制包，Android 仅检查公共核心/控制包；交叉编译不代表对应设备运行、安装包或 UI 已完成。服务进程 smoke 在私有网络命名空间中运行，无需外部代理节点。

效率设计：

- Go 后端和每个跨平台目标分开并行，没有九个平台的串行编译队列；任务限时 10～12 分钟。正常代码变更以 5～8 分钟墙钟时间为目标，冷缓存、GitHub 排队及 runner 性能会影响实际耗时。
- 每个后端/目标分别缓存实际使用的 `veil-core/.build/mod` 和 `.build/cache`，避免并行任务抢写同一个缓存键；Rust 缓存依赖和 target。命中旧编译缓存后仍执行测试，Go/race 使用 `-count=1`。
- 不缓存生成的 TLS 源码副本和 overlay，每次从固定依赖重建并校验源文件哈希。Go 版本固定在根目录 `.go-version`，Rust 固定在 `interop/rust/rust-toolchain.toml`。
- 普通 Markdown 修改只跑工作流自检与汇总；`PROTOCOL.md` 修改还会验证 Rust 互通及向量。仅控制层改动不重跑核心/Rust，Rust 改动不重跑全部 Go/交叉编译。核心、CI、工具链和未知组件改动运行完整检查。浅克隆无法确定差异时安全退回全量。
- 新提交自动取消同一 PR/分支的旧运行；只使用普通 `pull_request`、只读 token 和不持久化凭据的 checkout。
- 有意暂停 30 秒的下载回归通过手动运行的 `extended` 开关开启，只在 batch 跑一次。性能基准、WAN 测速、浏览器抓包不放在普通 PR 中。OpenWrt SDK/OPNsense 原生打包与真实 VM 界面验收按平台改动本地执行；常规 CI 仅增加轻量平台语法、翻译和 ACL 检查，Linux 交叉编译包含 rpcd 适配器。

选择固定 `ubuntu-26.04` runner，因为现有混合密钥交换测试需要 OpenSSL 3.5 的 X25519MLKEM768，避免每次从源码编译 OpenSSL，也不静默跳过这项测试。该镜像目前属于 GitHub 官方公开预览，工具清单见 [runner image](https://github.com/actions/runner-images/blob/main/images/ubuntu/Ubuntu2604-Readme.md)。actionlint 的标签表暂未更新，因此单独声明了这个已核实的 runner 标签。Actions 按完整提交固定；actionlint 发布包按 SHA-256 校验。

本地可直接复用同样的组件命令：

```sh
python3 -m unittest discover -s .github/scripts -p 'test_*.py'
bash .github/scripts/lint-workflow.sh
python3 veil-core/scripts/check.py batch --race --stdlib
python3 veil-service/scripts/build.py batch --check --race
python3 veil-service/scripts/openwrt_check.py
python3 veil-service/scripts/opnsense_check.py
python3 veil-core/scripts/cross_check.py --target linux/arm64
python3 veil-service/scripts/cross_check.py --target linux/arm64
```

跨平台脚本不加 `--target` 时保留原先的本地全量检查行为。不要在同一个工作目录同时调用同一后端的构建器；CI 矩阵有各自独立的 runner 和工作目录。
