# CI 与发布

GitHub Actions 在 `main`、`ci/**` 分支 push、所有 PR 和手动运行时执行。全部检查完成后的固定入口是 **CI**；分支保护应选这个汇总检查，避免依赖条件跳过或动态矩阵名称。

## 版本发布

推送 `vMAJOR.MINOR.PATCH` tag 触发 `Release`：复用完整 CI，同时并行打包五种 CLI、Windows/Linux amd64 桌面、OpenWrt 24.10 x86_64 / LuCI 和 OPNsense amd64 构建材料。Linux 桌面使用 Ubuntu 24.04 构建，减少对更新 glibc 的依赖。Go 和平台打包工具均固定版本或提交。

发布前校验完整文件清单、SHA-256，以及 CLI/桌面 manifest 中的版本、提交和干净工作树；全部检查及构建成功才将文件上传到 GitHub Release。只有发布任务具有 `contents: write` 权限；重跑不会覆盖已发布版本。

准备 `.github/releases/v版本号.md`，提交后创建并推送对应 tag 即可。每个包附校验文件，另有汇总 `SHA256SUMS`。OPNsense 构建材料需在对应系统内运行 `build.sh` 生成匹配 ABI 的原生包。

## 自动检查

| 并行任务 | 验证内容 |
| --- | --- |
| Core × 3 | native、batch、OpenSSL 各自运行协议/回环网络测试和 race；batch 额外验证标准库 TLS overlay |
| Control service | 公共控制层、持久保存、生命周期、race/vet、真实 veild/veilctl 进程、Linux 安装包、启动脚本语法及 LuCI 与 OPNsense 翻译/ACL/JS 检查、OPNsense PHP 语法 |
| Windows control | Windows 原生命名管道、ACL、持久化、套接字错误与双向转发回归；便携 ZIP 的摘要、架构、版本及包内 CLI 生命周期 |
| Desktop × 2 | Windows / Linux 原生构建、实例互斥、关闭释放与导入限制；Windows 发行包的真实 WebView2 操作、关窗保活、系统代理切换与恢复、HTTP/SOCKS5 实际转发及界面截图 |
| Rust interoperability | fmt/clippy、规范固定向量、Rust↔Go 双向 TLS/REALITY 互通，覆盖 batch 和 OpenSSL |
| Compile × 8 | Linux ARM64/ARMv7/MIPS/MIPSLE、FreeBSD amd64、Windows amd64/ARM64、Android ARM64；核心及支持的控制组件 |

Linux amd64 已由运行测试覆盖。Windows 包含原生控制测试与桌面构建，Android 仅检查公共核心/控制包；交叉编译不代表对应设备运行、安装包或 UI 已完成。服务进程 smoke 在私有网络命名空间中运行，无需外部代理节点。

Windows 桌面任务直接运行刚生成的发行包，通过 Playwright CDP 操作其真实 WebView2。测试使用临时配置和回环 TLS 服务，核验 SOCKS5 / HTTP 访问 HTTP / HTTPS、系统代理自动设置 / 保持 / 清除、断开与明确退出恢复，以及中英、深浅色和窄窗口。原生 `WM_CLOSE` 后核验窗口隐藏且转发继续；通知区图标和菜单的实际鼠标交互仍单独验收。截图和诊断保存为 `windows-desktop-gui` artifact，保留 7 天；临时凭据和状态目录不上传。调试端口只在测试启动参数中通过 WebView2 的 `--edge-webview-switches` 开启，结束后关闭测试进程并恢复系统代理。

这项检查复用 Windows 桌面构建与 Go 缓存，只安装锁定的 `playwright-core`，不下载 Playwright 浏览器；已有 WebView2 时不重复安装。需要本地复现时，在隔离 Windows 用户中设置 `VEIL_TEST_SYSTEM_PROXY=1`，执行工作流中的 GUI 步骤。

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
