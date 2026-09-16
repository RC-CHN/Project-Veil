# Project Veil 工程化移交记录

> 当前交接更新于 2026-09-16。正式开发入口是根目录 [README.md](README.md)、[core/](core/README.md) 和 [node/](node/README.md)，版本为 `0.5.0-engineering2`。Linux 实战链路仍是交付目标；最新讨论将性能、稳定性与抗识别基线列为下一阶段建议。完整产品工程化尚未完成。本文先记录最新进度，再保留 engineering1 和 session18 的历史交接快照。

## 当前工程交接（engineering2，2026-09-16）

### 用户确定的优先级

用户要求先跑通 Linux 实战全栈。机器主要为 x86/systemd，首轮验收目标为 Linux/amd64；将来可能使用 OpenWrt，不能把运行逻辑绑定到 systemd。第一批 goal（正式成品双端、私有材料准备、TCP/UDP 诊断及预算边界验收）已经完成。完整五批范围见 [Linux 交付计划](docs/delivery/linux-first.zh.md)。

用户随后提出优先考虑性能、稳定性和匿踪性，**此前性能暂停的安排不再适用**。建议先推进：**正式版本联合基线与有界观测 → 按证据做局部优化 → 扩大耐久和抗识别评估**，然后衔接 Linux 安装与服务、TUN/DNS/路由与防直连、证书和更新恢复、真实双机验收。详细批次、指标和待办见 [质量推进计划](docs/delivery/quality-plan.zh.md)。这覆盖下面旧快照中的优先级；不要求先完成全部核心拆包。

用户进一步明确：**混淆要考虑伪装成正常过境业务流量**。对照必须包含匹配跨境链路与托管条件的真实正常业务，兼顾服务部署/TLS、业务交互、完整会话和跨连接行为。文件同步/对象传输只是初始候选，尚无当前模型伪装成立的证据；不能以任意 HTTPS、随机填充或仅改握手指纹替代。当前 mTLS 入口是否符合选定业务也要单独验证；域名可选、公网 IP 证书优先的既有部署偏好仍保留。

本次只核对既有代码/报告并更新计划，没有修改运行代码、启动性能或耐久实验，也没有创建新 goal。当前版本尚无新的性能和抗识别成绩；下一项有界目标建议为正式进程测量入口、有界事件/资源诊断、基线报告及 30–60 分钟稳定性先导。

### 本轮实现

- `veilctl prepare`：Linux 初始双端材料准备。普通模式验证给定服务端证书、私钥、链、名称及有效期余量；生成独立客户端 CA/身份、同一私有模型和配对配置。client/server/admin 分离，私有目录 0700、文件 0600，拒绝覆盖已有输出；`--local-test` 明确限制环回地址并生成私有测试服务端身份。
- `veilctl configcheck`：输出实际本地字节预算、模型文件摘要、证书指纹/到期等报告；不建立连接，端到端状态仍为未检查。
- `veilctl probe`：通过正在运行的本地 SOCKS 向明确指定的自有 TCP/UDP 回显目标发随机载荷，验证 TCP 半关闭/完整响应及 UDP 边界；失败返回非零退出。探测不直连目标、不在本地解析目标域名，结果仅代表指定目标。
- `veild --status-interval`：带运行实例 ID/PID 的启动、周期和退出 JSON 状态，增加拒绝、认证及承载失败计数。stdout 仍须被外部持续消费，尚无完整事件归档；快照的 `EndToEnd` 不会被本地就绪伪装成通过。
- node 配置增加有界 `limits.stream_bytes` / `limits.carrier_bytes`。缺省保持 8 MiB / 64 MiB，对端取较小值；窗口、队列、并发、累计 OPEN 和确认语义未放宽。较大值必须明确配置，不提供透明跨承载续传。
- `tools/accept_linux.py`：核对构建来源与二进制后启动正式 veild 双端及自有回环 TCP/UDP/DNS 夹具，覆盖真实文件加载、权限、域名、并发半关闭、错误身份/种子、预算耗尽、服务端被杀后的新连接恢复及持续连接；不依赖 root、Docker 或 systemd，不修改宿主网络。

操作与具体限制见 [Linux 成品双端操作](docs/operations/linux-process.zh.md)。Windows 保持交叉构建，prepare 写入后端暂不支持 Windows；OpenWrt 尚未做原生实现或验收。

### 本轮验证

本轮第一批 goal 已完成。最终成品进程验收 **24/24 通过**：同一 TCP 连接和 UDP 关联持续 260.009 秒，各 258 次操作；12 MiB 下载长度和 SHA-256 一致；默认流预算、对端较小预算及承载预算均被执行；错误身份/种子被拒绝，服务端被杀后客户端不重启即可恢复新业务。13 个节点进程已退出，临时私有材料已删除。

core/node 完整 Linux 竞态检查、验收监督器初始化失败清理测试及 Windows 交叉构建通过。129 个导入源文件和三个冻结二进制保全检查通过。构建、检查、最终进程验收绑定源码摘要 `2fc0afd051fe2f7bdf72edca79575029d8ad1a34aead8aa560d096eaf24d6ca7`。

详细边界、字节预算结果和任务目录见 [本轮交付报告](docs/delivery/linux-process-report.zh.md)；[小型验收证据](docs/delivery/linux-process-evidence/acceptance.json) 随文档提交，原始状态日志和二进制保留在忽略的 out 中。260 秒回环进程验证不代表公网、TUN 或全天耐久通过。

本轮分批本地提交：`eb8f252` 为准备/诊断/状态与预算实现，`6945881` 为成品进程验收及监督清理测试；操作说明、证据和本交接随后以 `docs: record Linux process delivery and next milestones` 独立提交。未推送远端。

### 明确停点与下一批

下一批建议是 **当前版本的性能、稳定性与抗识别基线**：接出内部等待和批次统计，补有界事件归档/资源采样，使用正式双端做同语义性能比较、隔离弱网先导和当前 TLS/流量特征审计。先定位外层发送名额和信用调度等瓶颈，再决定具体优化；旧 session16 数据不能当作 engineering2 结论。详见质量推进计划中的待办。

**Linux 安装和服务管理仍待完成**：运行用户/私有目录、版本化布局、systemd 单元、启动校验、就绪与业务探测、停止/卸载及机器重启验收。veild 继续以前台进程和稳定配置/退出接口运行，systemd 仅是首个服务适配；保留 procd、netifd、firewall4 及 LAN 转发的独立适配边界。

TUN/DNS 桥接尚未接入新 node；公网 IP ACME、普通换证、失效身份退役、升级回退、通用私有持久事务、事件分段归档、全天耐久和当前版本检测评估仍未完成。没有受控 VPS 实际签发和公网双机验收；本轮回环结果不能替代这些证据。

prepare 只创建新材料，不负责在线换证、安装或恢复；生成的客户端身份 30 天有效，需要后续身份轮换。准备过程失败可能留下私有暂存目录；尚未验证宿主断电持久性。预算耗尽会中断受影响流，EOF 不代表文件传输完整，业务需验证长度/摘要。

## 第一阶段工程交接快照（2026-09-16）

以下为 engineering1 迁移完成后的历史记录，源码及四批提交保持可追溯；当前状态与优先级以上面的 engineering2 交接为准。

### 当前结论与开发约束

- **主线已迁到根目录。** `core`、`node` 是独立 Go 模块，正式构建不依赖 `references/`；协议仍为 Config5 / FlightModel4 / `streammux-flight-v3`，node 配置单独使用 `version: 1`。
- **当前可用范围是核心库和前台 SOCKS 双端。** 准备模型、证书、私钥与配置后，可以运行客户端/服务端；系统安装、开机启动、TUN、自动运维和 GUI 尚未交付。
- **本轮按用户要求分批提交现有工程，并更新交接。** 没有增加运行功能、启动部署或重跑耐久实验。提交范围为正式源码、小型验证证据和文档；`references/`、`out/`、`local/` 继续忽略。
- **性能优化继续暂停。** 全天耐久、当前版本抗识别评估及完整 Veil 0.5 验收均未完成，不能用迁移测试替代。
- session18 私有发布包、旧 activate/renew 脚本及冻结镜像不能直接用于新 node；后续部署必须建立新版本绑定。保留旧二进制、报告和失败记录。

### 已实现范围

| 模块 | 当前实现 | 尚未完成的边界 |
|---|---|---|
| `core/` | 可嵌入的 Client/Server、规范目的地、内存模型与身份、直接 TCP 流及 UDP 关联；接入已有 TLS/h2、模型、多流、预算和执行代次运行库 | `internal/session` 仍保留原运行库的聚合实现；设计中的承载依赖接口及内部职责拆分尚未全部落地 |
| 流与生命周期 | TCP 半关闭、动态 I/O 期限、建立取消与运行取消、UDP 报文边界、任务退出后归还槽位 | 后续变化继续以合同回归约束，不能据短时用例宣称长期稳定 |
| `node/` | 配置与私有材料加载、SOCKS5 TCP/UDP、组件启停和退出等待、客户端/服务端装配 | 尚无持久状态提交、DNS 映射、系统网络事务、部署及证书恢复状态机 |
| `veild` / `veilctl` | 前台运行、版本/能力查询、模型生成、配置检查；启动及退出输出状态 JSON | 尚无常驻管理控制接口；未探测的端到端状态保持 `not_checked` |
| 平台适配 | Linux/Windows 私有文件读取器；Windows 实现所有者和 ACL 检查 | Windows 只做过交叉构建，原生运行、权限及系统集成未验收 |
| 观测 | 公共状态、事件接口、有界缓冲及丢失计数 | 缺运行实例标识的完整接线、分段归档、持续采集、端到端探测及告警 |
| 工程工具 | 根工作区、独立模块构建/检查、源码摘要、产物清单、迁移保全检查 | 尚无完整原生平台 CI 和发布流水线 |
| 预留模块 | `apps/`、`api/control/`、`packaging/`、`integrations/` 已记录职责和边界 | GUI、控制 IPC、安装器、系统服务、新容器镜像及新 TUN 桥接均待实现，目录存在不代表功能可用 |

架构与实际迁移范围见 [模块边界](docs/architecture/modules.zh.md)、[抽象设计草案](docs/architecture/cross-platform-primitives.zh.md)、[第一阶段迁移报告](docs/migration/phase1.zh.md)。设计草案中的接口和目录不能全部视为已实现。

### 验证与证据

第一阶段已有的通过记录：

- core/node 完整 Linux 测试及竞态检查，包括真实双端 TLS/h2、直接 TCP/UDP、半关闭、动态期限、SOCKS 全链路、启动失败和组件退出。
- core/node Windows/amd64 交叉构建；生成 Linux/amd64 和 Windows/amd64 的 `veild`、`veilctl` 共四个二进制。Windows 成品未在原生系统执行。
- 测试和构建绑定源码摘要 `7af8659db826c74bebcfd9f6e5fd924a8d046482e4fcd081305b473e71112e3b`。该摘要覆盖工具定义的构建源码范围，不包含本交接文档及全部说明文档。

本轮提交前重新核对：当前构建源码摘要与上述测试、构建记录一致；四个既有产物均存在且 SHA-256 一致；再次运行 `python3 -B tools/verify_migration.py`，129 个导入源文件和三个冻结二进制保全检查通过。本轮没有重复执行 Go 测试或重新构建。

小型证据已纳入正式源码提交：[测试记录](docs/migration/phase1-evidence/checks.json)、[构建清单](docs/migration/phase1-evidence/build.json)、[源码清单](docs/migration/phase1-evidence/source-manifest.json)、[保全记录](docs/migration/phase1-evidence/preservation.json)。本地二进制位于 `out/builds/20260915T161134Z-f6aeb129/`；`out` 未纳入 Git，迁移到其他机器后应重新构建。

### 分批提交

前三批已提交到本地 `main`，起点为 `817b5f6`：

| 提交 | 内容 |
|---|---|
| `2492c75` | 核心库、迁入运行库、合同回归与线路测试向量，共 146 个文件 |
| `18e0075` | node、命令行、SOCKS、平台文件读取、配置示例及第三方许可，共 22 个文件 |
| `1f2d8f1` | 工作区、构建/检查工具、架构、迁移证据和后续模块说明，共 33 个文件 |

本文件作为第四批，以 `docs: record engineering progress and remaining work` 独立提交；其提交标识通过 `git log --oneline` 查询。正式源码已纳入 Git，旧正文中“根仓库只跟踪 LICENSE”的描述仅适用于历史快照。本轮仅创建本地提交，未推送远端。

### 待办与建议推进顺序

以下均为未完成事项；实施时应在各阶段保留独立版本和验证证据。

1. **收敛核心接口和规格。** 小步拆分 `core/internal/session`，完善解析/拨号等依赖边界；统一当前线路、认证、关闭、预算和威胁规格，继续机制差异审查。保留直接调用合同及历史回归。
2. **补齐观测与监督。** 实现运行实例标识、事件分段归档、磁盘预算和丢失记录，接入持续存活监督、业务探测及告警。先解决全天验收对完整事件记录的要求，再开展新长跑。
3. **实现私有持久存储。** 落地权限、独占锁、版本冲突、提交结果不确定和中断恢复合同，分别验证 Linux/Windows；为 DNS 状态、身份退役和安装事务提供基础。
4. **接入 TUN 与系统网络。** 迁移持久 DNS 映射，确定第三方桥接边界，完成服务端例外路由、IPv4/IPv6 防直连、启动/退出和网络变化恢复；只操作本项目归属的系统资源。
5. **完成证书与身份运维。** 新工程接入普通换证、过期/撤销身份退役、失败保持停止和持久恢复；保持公网 IP 证书优先策略。真实 ACME 签发需要具体受控 VPS 环境，再完成定时续期、实际生效检查和告警。
6. **完成安装与升级。** 为 Linux/Windows 实现系统服务、权限配置、安装器和升级回退；为新 node 构建独立镜像与发布格式，不能沿用 session18 标签假定已验证。
7. **实现本地控制和界面。** 先固定版本化协议、系统对端身份与授权、操作结果查询和事件订阅，再由 Windows/Linux GUI 对接共用 node。
8. **补齐当前版本验收与 CI。** Windows 原生运行/ACL、当前版本客户端身份/模型种子/CA 轮换、错误组合拒绝、弱网、网络切换、负载下升级回退及完整全天耐久；增加与实际支持范围一致的原生平台检查。
9. **完成当前版本语料和检测。** 建设正常 HTTPS 对照与独立留出数据，冻结检测流程，分别评估启动、完整会话、跨连接及无凭据主动探测，保留误报和失败样本。
10. **性能待用户重新安排。** 保留与 AnyTLS 同语义比较、额外流量和主动等待目标及历史未达标记录，不在本轮恢复优化。

当前构建及检查入口以根 [README.md](README.md) 为准：`python3 tools/build.py`、`python3 tools/check.py --race --windows`。`tools/verify_migration.py` 还需要本机保留的 `references/` 来源与冻结二进制，普通源码克隆本身不包含这些大产物。

## 历史交接快照（2026-09-15）

以下正文原样保留，用于追溯 session18 原型、实验和冻结证据。其中“当前候选”“默认工作目录”“尚未纳入 Git”和 Docker 运行状态均指 2026-09-15，不能覆盖上面的正式工程状态。本轮未重新核验历史 Docker 状态。

更新：2026-09-15；运行状态与冻结材料核验于北京时间 21:43–21:44。对象：接手正式工程化的开发者或 agent。

本文件位于 Project-Veil 根目录。下文相对链接均以该根目录为基准。本次交接以实际源码、冻结清单、实验记录及 Docker 状态为依据，覆盖设计目标、已做工作、文件用途、准确停点、未完成事项和接手步骤。

## Veil 是什么，为什么要做

**Veil 是一个面向抗流量识别的加密代理传输项目。** 用户在自己的设备和 VPS 上分别运行客户端、服务端，应用通过本地 SOCKS 或 TUN 接入，由 Veil 将 TCP 字节流和 UDP 数据报经加密连接送到 VPS，再由服务端连接业务目标。它的研发内容包含承载协议、双端运行时和部署工具；当前研发主线是 Veil 0.5，已有的 0.4 留作冻结基线。

**设计目的，是在保持代理可用性的同时，降低观察者凭流量特征识别、进而封锁代理连接的能力。** 项目关注跨境网络中的流量分析与主动探测场景，包括 GFW 这类观察者可能利用的特征：握手、包长和方向、请求响应节奏、空闲与结束行为，以及多个连接长期聚合后的规律。业务内容经过 TLS 加密之后，这些外部特征仍是要单独处理的问题。

为此，Veil 0.5 选择的核心方向是：**先由双端共同执行一套受约束的 HTTPS 会话行为规则，再把应用数据放入这些规则允许的传输机会。** 规则描述下一步允许什么请求、双方如何响应、容量与等待范围、如何确认和推进状态，以及怎样结束会话。内层负责保持应用数据的正确顺序、半关闭和数据报边界，外层负责模型规定的交互。这样可以减少应用的一次写入与外层一次可观察交换之间的直接对应关系。

这里的“模型”指版本化的协议行为规则和状态机，不是在线调用的大语言模型，也不表示运行时已经具备自动学习能力。双端共享模型与种子来协调行为，同时使用每条连接独立的材料；上下行可以采用配对但不对称的规则。当前实现只支持少数经过约束检查和实验验证的模型，后续需要通过真实 HTTPS 样本和预算分析来证明模型选择是否合理。

设计上研究的参考业务包括 HTTPS 对象上传、下载和同步交互。以一次下载为例，应用侧先经过 SOCKS/TUN 形成内层业务流，外层按模型完成多步对象交换、回执和状态推进，再由另一端还原业务字节；一个对象可承载多个内层流的数据，应用写入边界与 HTTP 请求边界分开处理。这个例子说明的是控制和承载方式，**当前 session18 尚未证明其线上统计行为与某类真实云同步或下载业务足够接近。**

项目的成功标准需要同时覆盖三件事：

- **能可靠使用：** TCP/UDP 语义正确，认证、目的地限制和资源预算可执行，客户端与 VPS 可安装、监控、换证、回退和恢复。
- **能用证据说明抗识别效果：** 在真实正常 HTTPS 对照、独立留出数据和当前版本检测器下，衡量启动、完整会话、跨连接及主动探测结果，保留误报、识别率和失败样本。
- **代价可接受：** 保留与 AnyTLS 同语义比较的速度目标，以及流量和主动等待预算。用户现已要求先做非性能工程事项，性能问题单独处理。

选择双端行为模型还有一个明确要求：核心机制应当相对所审查的 sing-box 协议实现有可说明的区别。是否构成这种区别需要持续审查，不能仅凭改字段、请求路径、填充参数或增加并行度下结论。可编程流量行为这一研究方向已有 Marionette 等先例，项目不主张全球首创；相关分析见 [机制差异与设计取舍](references/veil/docs/next-design.zh.md)。

当前可交接的是**已有真实业务和多项正确性、部署实验支撑的研究原型**。抗识别效果、完整长期稳定性和正式工程化仍有待完成。接手者应继续围绕“业务正确性、可验证的抗识别目标、有限成本、可维护部署”推进，不能把“已经能够转发流量”当作全部设计目的已经达成。

## 1. 先读这几条

1. **当前可运行候选是 `0.5.0-session18`，完整 Veil 0.5 尚未交付完成。** 已有真实 TCP/UDP、模型执行、安装/回退和多项隔离验收；宿主部署、证书自动运维、全天耐久、当前版本完整轮换、检测评估仍有缺口。
2. 用户已明确要求：**先处理非性能事项，性能优化暂停，后续单独处理。** 80% AnyTLS 速度、20–40% 总额外流量、约 30 ms 主动等待目标仍保留，不能因暂停而写成已达标。
3. 用户已明确选择：**公网 VPS 优先用 Let's Encrypt 公网 IP 证书及自动续期，域名可选。** 短期证书安装策略和换证钩子已做；真实公网签发、定时续期和过期紧急恢复尚未全部接通。
4. **根仓库目前没有跟踪主要工作成果。** 根 `.gitignore` 内容是 `references/`；根 Git 的 HEAD 是 `817b5f6`，`git ls-files` 只有 `LICENSE`。源码、报告、二进制和参考实现都在被忽略的 `references/` 中，不能只克隆根 Git 仓库就认为完成交接。
5. **当前没有需要接管的后台实验。** 2026-09-15 21:44 的 Docker 只读查询没有发现 `veil` 命名的容器或网络；相关宿主实验/运行进程查询也为空。session18 镜像仍保留。全天耐久首轮已经中断并清理，未重跑。
6. **最近完成的是服务端独立换证阶段。** 之后仅开始阅读过期恢复相关代码；用户要求移交时，没有提交新的紧急恢复实现，也没有启动新的失效证书实验。

建议阅读顺序：本文件 → [当前非性能清单](references/veil/docs/v0.5-nonperformance-delivery-plan.zh.md) → [session18 操作入口](references/veil/deploy/session18/README.zh.md) → [运行时验收](references/veil/docs/v0.5-generation-runtime-report.zh.md) → [最新换证报告](references/veil/docs/v0.5-server-certificate-rotation-report.zh.md)。

## 2. 设计目标与成功标准

原始完整目标是实现并交付初步可部署的 Veil 0.5，同时保留 Veil 0.4 冻结基线。工程化应继续围绕以下要求，而不是把当前能通过的实验集合重新定义为全部目标。

| 方向 | 目标与约束 |
|---|---|
| 承载与机制差异 | 在标准 HTTPS 上实现相对参考 sing-box 已有协议有明确核心机制差异的新承载；版本化、受约束的可编程双端会话行为模型是核心候选。只换字段、URL、填充表或给旧 POST 隧道加调度器不满足要求 |
| 双端模型 | 双端预共享模型配置与种子；每条连接独立执行随机性；配对设计上下行不对称格式；内层流与外层交互解耦；先支持少量经过验证的模型，再扩展 |
| 规格与组合审查 | 冻结可独立实现的线路规格和威胁模型，审查与既有机制的重合；复用成熟 TLS/密码学组件，模式种子不能替代认证和 TLS 随机性 |
| 数据与控制语义 | 有界编解码、队列、认证、重放防护、状态协调、TCP 顺序与半关闭、UDP 数据报、SOCKS/TUN、DNS、防直连、目的地/资源限制、断链与配置升级语义 |
| 性能目标，当前暂停 | 与 AnyTLS 同语义、同链路比较，速度目标至少 80%；声明场景总额外流量目标 20–40%；主动额外调度等待约 30 ms 上限；网络等待和队列等待分别报告，不能靠关闭防护或混用确认语义达标 |
| 特征与检测 | 握手和首次应用交换纳入评估。建设多种真实 HTTPS 业务、客户端与服务实现的隔离样本库，保留失败及独立留出；针对当前 0.5 重新训练并冻结检测器，测试启动、全会话、跨连接和无凭据主动探测 |
| 评估诚实性 | 报告识别率、误报、置信区间和泛化；实验分类器结果不能等同真实 GFW，也不能声称未经证明的不可识别性 |
| 部署交付 | Linux/amd64 可回滚镜像，配置/模型生成与导出，真实证书维护，凭据/模型轮换，健康及预算监控，操作文档，适当的正确性、安全、弱网与耐久验证 |
| 实验与追溯 | 使用已授权容器/eBPF/自有端点；不修改无关宿主网络或外部服务，不发送外部消息；保留源码、版本、原始测量、失败记录，清理自有资源及临时密钥 |

完整目标仍未满足。本文件是工程移交材料，不是最终验收报告。

## 3. 当前设计实际上是什么

业务路径可简化为：

```text
应用
  ├─ SOCKS5 TCP / UDP
  └─ TUN 适配器 + 持久 DNS 映射 → SOCKS5
             ↓
内层 OPEN / 多流 / DATA / 消费信用 / 半关闭 / UDP 数据报
             ↓
有界 flight 分配与顺序交付，四条 lane
             ↓
每条 lane 的配对行为模型、条件对象交互、回执和执行代次交接
             ↓
同一条 TLS 1.3 + HTTP/2 连接上的标准 HTTP 交换
             ↓
服务端身份准入、目的地 DNS/地址策略 → 真实自有/允许目标
```

关键语义：

- 当前机制通过双端受约束状态机执行有依赖的对象交互、条件响应、收据和状态提交；模型结构、种子和每连接材料各有职责。现有实现围绕少数经过检查的 profile，不应宣传为任意模型已经安全可用。
- 当前独立运行库使用 TLS 1.3 / h2、双向证书认证和客户端叶证书指纹授权，TLS exporter 与完整模型标识绑定连接材料，再隔离 lane 和执行代次。共享种子不替代认证。
- 模型 ID 标识结构，不足以证明两端种子相同；发布时还检查整个模型文件的 SHA-256。
- 内层消费信用与外层对象回执不同：外层对象已收到，不等于应用已消费字节。不能混合这两个确认边界做性能优化。
- 多 lane 共享内层有界流与 flight 窗口；重排交付、全局 EOF 和关闭屏障有独立语义。不能靠无限队列隐藏顺序问题。
- session18 为长连接引入执行代次：在完整批次边界到达 2048 批次或 240 秒时交接；单代保留 4096 批次/600 秒硬限额。交接不重置应用字节、OPEN、窗口、准入和空闲预算。
- 建立期限为 8 秒，从开始建承载/接受连接计时；不能通过不断交接逃避内层建立期限。交接响应丢失或校验失败关闭承载，不盲目重发可能已经执行的交接。
- 断链会中断应用流；没有跨 TLS 透明续传、无损迁移或自动应用字节重放。当前证书更换也采用明确停机切换。
- 当前服务端要求客户端证书。无凭据主动探测、TLS/h2 指纹、启动和跨连接特征仍须评估；**不能直接继承旧 0.4 README 中的公开静态站点、证书热加载或抗探测描述。**

详细合同入口：[多流](references/veil/docs/v0.5-streammux-contract.zh.md)、[flight HTTP 组合](references/veil/docs/v0.5-flight-http-contract.zh.md)、[early OPEN](references/veil/docs/v0.5-early-open-contract.zh.md)、[执行代次](references/veil/docs/v0.5-flight-generation-contract.zh.md)、[DNS/TUN](references/veil/docs/v0.5-tun-dns-contract.zh.md)。这些合同分阶段编写，部分标题保留“候选/尚未交付”；实际通过范围要结合后续报告判断。**统一的当前线路/威胁规格及完整核心机制重合审查仍待完成。**

## 4. 当前应固定的版本与产物

| 项目 | 当前身份 |
|---|---|
| 当前独立 CLI | `references/veil/bin/veil-generation-runtime`，`0.5.0-session18` |
| CLI SHA-256 | `8b0a1e0d971b3524cab52a1a50d60a1d87c234998c63553edf2316e6b0e941ec` |
| Linux/amd64 镜像 | `veil:0.5.0-session18` |
| 本地 Docker Image ID | `sha256:fed6f07ce8d6ceef1f841d5dc93e9efa5c2c294880153751e7514b4090f926f7` |
| 线路/配置 | Config/Bundle 5，FlightModel 4，`streammux-flight-v3`，发布清单 v3 |
| 四 lane 模型结构 ID | `22f9ec6fb93a8c06110d35ab76f92db9712eed66b19cce054e531976204e6bf2` |
| 当前 DNS/TUN 适配器 | `references/veil/bin/sing-box-veil-tun-v7-dns2` |
| 适配器 SHA-256 | `7b1d38e0fac222b335cd964fe05f23951895df449eed7ae2a7c118f1bff3a44b` |
| 0.4 冻结二进制 | `references/veil/bin/veil-v4` |
| 0.4 二进制 SHA-256 | `efdbcbbf8d6259977e7c363e54f2b1880a859ed5cf46e7ceb59943f73137b5c7` |
| sing-box 参考提交 | `93fff5954390367dd456cad3cbd79be54f8b941f`，位于 `references/sing-box` |

上述镜像哈希是本机 Docker Image ID，不代表已经发布到可供别人拉取的 registry。跨主机移交需要实际保留/导出镜像或按冻结来源重建验证，不能凭一个本地标签假定远端可用。

当前部署预算包括：每端 384 MiB / 2 CPU / 128 PID、UID1000、只读根文件系统；默认四 lane、每流窗口 65536、每流字节上限 8 MiB、每承载累计 128 OPEN / 64 MiB 等。以实际配置及合同为准，不要把实验放宽值混入新基线。

**工作树与已测二进制必须区分：** 工作树在 session18 核心冻结后还包含共用目的地/DNS策略与部署脚本更新。当前 `cmd/veil-session` 的版本字符串虽仍是 session18，直接用现在的源码重新编译不保证等于上面的固定二进制。工程化新构建应使用新产物路径和明确版本/绑定，不能覆盖旧二进制或复用旧镜像标签。

精确运行时来源见 [运行时冻结源码](references/veil/docs/results/v0.5-generation-runtime/source.tar.gz)、[源码清单](references/veil/docs/results/v0.5-generation-runtime/source-manifest.json)、[重建与重放记录](references/veil/docs/results/v0.5-generation-runtime/source-replay-verification.json)。

## 5. 已经做了什么，以及证据的边界

| 工作 | 已完成内容与证据 | 尚不能据此推断 |
|---|---|---|
| 0.4 基线 | 保留独立二进制、历史源码/镜像及性能和对抗评估，见 [phase4](references/veil/docs/phase4-report.zh.md)、[phase5](references/veil/docs/phase5-report.zh.md) | 旧性能/检测结果不能用于证明新 0.5 |
| 0.5 核心与数据语义 | 多阶段实现有界行为模型、对象事务、TCP/UDP、OPEN/准入、消费信用、多流、flight、early OPEN、目的地策略；代码及阶段报告在 internal 和 docs 中 | 分阶段单测/小用例通过不等于所有新版本组合已交付 |
| session18 长连接 | 原 740 秒用例 370/370 操作成功，四 lane 各交接 10 次；同一长 TCP/UDP 持续到结束；实际建立期限与 18→16→18 回退通过，见 [报告](references/veil/docs/v0.5-generation-runtime-report.zh.md) | 不能当作 24 小时或公网长期稳定性证明 |
| 时间触发交接 | 实际安装镜像、200 ms 单向延迟、300 秒/150 次操作成功；四 lane 在批次阈值之前按时间交接，见 [报告](references/veil/docs/v0.5-generation-age-report.zh.md) | 不涵盖任意网络变化 |
| 交接故障 | 四类错误回执/丢失故障，九阶段、18 条载荷、新连接恢复及 23 类证据对照，见 [报告](references/veil/docs/v0.5-generation-fault-report.zh.md) | 使用带错误注入的认证测试客户端，不能声称全部是原版双端自然网络故障 |
| 双向弱网 | 固定 session18 双端在丢包/抖动/乱序下完成 300 秒、150 次业务及代次交接；43 类证据对照，失败先导保留，见 [报告](references/veil/docs/v0.5-weak-network-report.zh.md) | 长期断网、网络切换、带业务升级和全天耐久尚需独立验证 |
| DNS/TUN | 修复混合 DNS 回答策略信息丢失，增加持久映射、缺失状态拒绝与地址不复用；固定 session18 + DNS2 实际 TUN 主流程 122 项、边界 25 项、29 类证据对照，见 [报告](references/veil/docs/v0.5-tun-dns-runtime-report.zh.md) | 隔离容器中的实际 TUN 不等于完整宿主开机/断电/网络恢复；小容量 60 秒边界不等于默认容量及 900 秒实测 |
| 安装与回退 | 私有双端包、生成/导出、固定镜像绑定、单角色 stage/activate、持久事务与探测失败恢复；当前版本有额外绑定和跨镜像验证，见 [操作说明](references/veil/deploy/session18/README.zh.md) | 历史 session17 的身份/种子/CA 轮换不能替代当前 session18 完整轮换；单角色原子提交不是跨主机原子升级 |
| 监控 | 当前状态/预算/清理失败/事件丢失/证书剩余时间采集，原子 Prometheus 文本输出，见 [监控](references/veil/deploy/session17/MONITOR.zh.md) | `Ready` 或 `healthy` 不等于远端可用；`end_to_end=not_checked` 必须保留实际含义 |
| 最新服务端换证 | IP 服务端默认 48h 安装余量，其他身份默认 336h；独立服务端包和续期钩子；正式 7 轮业务、35 条载荷，15 项拒绝检查、21 类证据对照；客户端未重启，身份和模型不变，见 [报告](references/veil/docs/v0.5-server-certificate-rotation-report.zh.md) | 用自有 CA 精确模拟 160h IP 证书；尚未完成真实 Let's Encrypt 下单、定时续期或过期紧急恢复 |

最新换证验证还检查了同一证书重复调用不重启、准备期间活动代次变化时拒绝提交、远端实际证书检查失败时恢复旧服务端。业务流失败和清理失败为零；非零承载/后端计数及其解释保留在原始报告中，没有被抹掉。

## 6. 文件与目录地图

### 6.1 项目层级

```text
Project-Veil/
├── handoff.md                     本交接文档
├── LICENSE                        根仓库 AGPL-3.0 许可证文本
├── .gitignore                     当前忽略整个 references/
└── references/
    ├── sing-box/                  参考实现的独立 Git 工作树
    ├── veil/                      当前 Veil Go 模块及研发资料主体
    ├── experiments/phase1/        初期调研/实验材料
    ├── tcp-obfuscation-review.zh.md
    ├── veil-v1-draft.zh.md
    ├── veil-v1-cover-and-evaluation.zh.md
    └── veil-v1-active-probing-and-certificates.zh.md
```

根目录的 `test.mp3` 和 `3.5HDMI_E_DTBO.zip` 保持原状，不是当前 session18 的运行入口。

### 6.2 Veil 模块内的职责

| 路径，均相对 `references/veil/` | 用途与接手注意点 |
|---|---|
| `go.mod` | 模块 `veil.local/veil`，Go 1.26.0，toolchain 1.26.7 |
| `cmd/veil-session/main.go`、`preflight.go` | 当前独立客户端/服务端、模型生成、配置/证书预检、状态和事件输出入口 |
| `internal/objecttransport/` | 当前运行库主体；client/server/config、TLS、SOCKS/UDP、flight 驱动、模型、代次交接和观测 |
| `internal/behavior/` | 有界行为模型、批次、预算与执行状态 |
| `internal/objectstore/` | 有界本地条件对象存储 |
| `internal/streamopen/` | 目的地 OPEN 编解码和握手 |
| `internal/streamlink/` | 单流、消费信用和相关诊断 |
| `internal/streammux/` | 多流帧、运行时、early OPEN 和关闭语义 |
| `internal/datagram/` | UDP 数据报封装与有界队列 |
| `internal/flightwindow/` | 全局租约/在途窗口及顺序接收 |
| `internal/destination/` | 目的地连接、地址与准入限制 |
| `destinationpolicy/` | 运行库和 DNS/TUN 共用的目的地策略 |
| `tundns/` | Linux 私有持久 DNS 映射、锁、原子更新与地址签发状态 |
| 根 `*.go`、`v5_*.go`、`cmd/veil/` | 0.4/早期 0.5 研发线路仍共存，不能误认为当前 session18 主入口 |
| `cmd/veilbench/` | 较早基准工具；不等于当前完整同语义比较入口 |
| `deploy/session18/` | 当前固定镜像 Dockerfile、启动脚本和操作说明 |
| `deploy/session17/` | 目录名历史遗留，但 release/install/activate/monitor/renew 是当前仍复用的公共部署工具 |
| `deploy/session16-rollback/` | 固定历史回退候选 |
| `deploy/tundns/` | DNS2 适配器覆盖文件及使用说明；构建依赖固定的参考源码 |
| `deploy/*.patch`、`SING*-LICENSE` | sing-box/sing 的半关闭、UDP 等本地补丁和许可证材料，移交时保留 |
| `experiments/sessiondeploy/` | 镜像、发布、安装、回退、证书换证等实验与独立分析器 |
| `experiments/generationruntime/` | 时间触发交接、建立期限、错误注入与回退验证 |
| `experiments/sessiontun/`、`tundnsruntime/` | TUN/DNS 构建、联调及分析 |
| `experiments/weaknetwork/` | 双向弱网拓扑、工作负载、抓包分析和证据对照 |
| `experiments/sessionendurance/` | 先导/全天运行器、分析器、负例和中断收集脚本 |
| `experiments/flightwindow/`、`muxbench/`、`muxcost/` 等 | 历史机制和性能实验；按报告的源码/二进制绑定使用 |
| `experiments/exchange/corpus/`、`detection/` | 早期样本/检测工具，可供复用，但不能代表当前 session18 已做完检测 |
| `docs/*.md` | 逐阶段合同、问题诊断、实验报告和剩余计划；不是一个已经统一整理的产品文档集 |
| `docs/results/` | 冻结源码、构建/运行记录、原始日志/抓包、分析与负例、清理证据；不要覆盖 |
| `testdata/` | 测试向量及回归材料 |
| `bin/` | 多个历史/候选/竞态/测试探针二进制；名字和哈希必须与相应报告匹配 |
| `test-runs/` | 历史构建工作目录和第三方源码副本，不应直接成为工程化主源码 |
| `local/` | 私有运行材料目录；移交/纳入 Git 前检查凭据和配置，不应整目录公开提交 |
| `scripts/`、根 Dockerfile/Makefile、旧 Compose/Kubernetes 模板 | 大量是早期工具，可能指向旧线路；重新整理入口后再用于正式部署 |

关键部署文件：

- [release.py](references/veil/deploy/session17/release.py)：完整配对包、独立角色校验/导出、`renew-server`；`create` 省略 `--model` 会生成新种子，普通换证不要误用它。
- [install.py](references/veil/deploy/session17/install.py)：私有归档限制、摘要、角色与运行时绑定，安装到新的 generation，安装锁。
- [activate.py](references/veil/deploy/session17/activate.py)：固定 Docker 放置、归属核验、启动/停止、探测、持久事务与恢复；新增准备代次一致性检查。
- [renew.py](references/veil/deploy/session17/renew.py)：消费 ACME lineage、生成服务端新包、安装和带探测激活；依赖旧证书仍有效。
- [monitor.py](references/veil/deploy/session17/monitor.py)：私有启动状态采集与原子文本指标；默认叶证书告警仍为 336h，短期证书可显式设 24h，完整自动策略尚未接通。
- [证书维护操作文档](references/veil/deploy/session17/SERVER-CERTIFICATES.zh.md)：已实现命令、权限、停机语义与当前边界。

大致磁盘占用，按交接时 `du -sh`：`bin` 1.5G，`docs/results` 448M，`test-runs` 698M，参考 sing-box 16M。应先制定源码/运行产物/证据分层保留方案，再整理仓库，避免把实验资料误删或把所有大文件直接塞入普通源码提交。

## 7. 当前停点和已知问题

### 7.1 证书紧急恢复：只开始审阅，未实现

上一阶段已冻结普通换证。被用户中断的下一轮只读了 release/install/activate/renew 的当前代码，尚未修改。

明确的现状：

- `renew.apply()` 先对旧 active generation 做 `0s` 有效期检查；`release.renew_server()` 也要求旧身份仍有效。因此旧证书已过期时，普通续期钩子会拒绝。
- `activate.activate()` 在停止旧实例前校验旧 generation；`recover_locked()` 会校验并尝试恢复旧实例。失效旧身份不能沿用普通回退分支。
- 目前已有手动“明确 stop 后激活新包”的入口，但没有完整、可审计、抗进程中断的紧急退役/替换自动流程。
- `certcheck` 验证链、时间、名称、用途和私钥匹配；代码注明不进行 AIA/OCSP 等联网获取。不能把“证书链校验通过”写成“已检查公开 CA 撤销状态”。

拟继续的方向是显式记录旧身份退役，完整验证新身份，失败时保持停止而不恢复过期/撤销身份，并测试进程在提交前后中断的恢复。**这是待设计和实现的方向，不存在一个已经可用的 `--retire-previous` 功能。**

### 7.2 全天耐久：真实中断，未重跑

- 原始任务：2026-09-15 20:01:55 启动，24 小时/14,400 次操作计划。
- 双端于 20:58:46 退出；约第 57 分钟后记录失败。共 573 次尝试、570 成功、3 失败；长 TCP/UDP 连接重置，短 TCP 连接被拒绝。
- 双端 ExitCode=0、无 OOM、无重启；这些信息不能证明任务完成，也不能确定是谁停止了容器。
- 原 `environment.json`、`quiescent.json` 未生成。`running.json` 是启动快照，**不是存活信号**。
- Docker 历史事件查询为空。不要据此猜测为产品故障、宿主重启或用户操作。
- 双端启动/事件、目标日志、原始工作负载及容器/网络信息已另行保存；三个退出容器、专属网络和临时密钥目录已清理，独立复核通过。

入口：[耐久进度](references/veil/docs/v0.5-session-endurance-progress.zh.md)、[原始 day-01](references/veil/docs/results/v0.5-session-endurance/day-01)、[完整中断收集](references/veil/docs/results/v0.5-session-endurance/interruption-02)、[独立清理](references/veil/docs/results/v0.5-session-endurance/interruption-02/independent-cleanup.json)。140 秒先导 161/161 和 17 类证据对照已通过，只能作为先导。

**交接时额外发现一处静态验收矛盾：** `cmd/veil-session/main.go` 的事件记录器每个启动最多写 1024 条事件；全天合同要求 1440 条独立短 TCP，再加长流和承载事件，而 `experiments/sessionendurance/analyze.py` 要求 `EventsDropped=0` 并读取完整流事件。正式重跑前应先解决有界事件保留/分段归档与验收契约的兼容，不能仅删除丢失检查。这个矛盾尚未通过新长跑验证，**也不是首次约 57 分钟中断原因的证明**。

### 7.3 文档/工程入口存在代际混杂

`references/veil/README.zh.md` 开头仍写 dev5/dev4，旧 Makefile 的 build 指向 `cmd/veil`，test/race 使用整个 `./...`；许多合同和阶段报告保留历史“尚未完成”段落。当前接手应以本文件、非性能清单和 session18 入口定位，再逐项阅读原始报告，而不能随便选一个带“最新/当前”的历史段落。

`deploy/session17` 的历史轮换脚本和报告大量固定旧镜像。当前新二进制能读取部分旧包，不代表旧脚本已验过 session18 所有场景。

## 8. 还差什么

| 项目 | 当前缺口与完成要求 |
|---|---|
| 正式源码/构建工程 | 明确主线，拆分旧原型、参考实现、实验与产品源码；建立可复现构建、版本/镜像/配置绑定与 CI；保留冻结资料而不覆盖。根仓库忽略全部工作树的问题需要正式处理 |
| 统一规格与机制审查 | 整理当前 Config5 / FlightModel4 / inner-v3 的完整线路与威胁规格，写清旧版本关系、模型边界、认证/回放/关闭语义，与参考既有机制逐项比较 |
| ACME 与运维 | 受控公网 IP 的 staging/production 实际签发；挑战端口、账号/私钥和 UID 权限；定时续期、实际证书生效检查、失败重试/告警、宿主重启；普通钩子不能替代这一整条链路 |
| 失效身份恢复 | 过期/撤销后旧身份退役、新身份验证、失败保持安全停止、持久事务恢复，禁止自动重新启用失效身份 |
| 当前版本完整轮换 | session18 下客户端身份、模型种子、CA 轮换及旧身份/错误种子/错误组合拒绝、模糊与必要竞态验证；不要只引用 session17 成功 |
| 宿主/TUN 部署 | 安装器与启动单元、持久 DNS 状态、服务端例外路由、IPv4/IPv6 防直连、先后启动/退出、机器重启、网络变化和故障恢复；不触碰无关网络 |
| 端到端监控 | 把本地状态、业务探测、预算、证书续期与告警调度接起来；明确采集身份与敏感状态权限，不把本地就绪当远端健康 |
| 长期验证 | 解决耐久观测与监督问题，保留失败后新建任务完整跑满；长期断网、网络切换、负载下升级/回退和资源泄漏观察仍需完成 |
| 语料与检测 | 当前候选、多业务/多客户端/多服务器正常 HTTPS 数据；独立负例/留出、冻结训练与特征，启动/完整/跨连接/无凭据主动探测，识别/误报/区间与泛化 |
| 性能，暂停 | 恢复时保持同链路、同资源、同确认语义；继续定位速度、开销和主动等待，不提高无界资源或改变口径掩盖问题 |
| 最终交付总结 | 上述完成后汇总产品入口、验收证据、已知限制、回退/恢复操作和仍未达标项；本移交文件不能冒充完成验收 |

性能现状的具体口径：最近完整诊断比较来自历史 session16 阶段，八组联合门槛 **0/8**；热连接 1 MiB 上传/下载约为 AnyTLS 的 **56.51% / 49.04%**，该场景额外流量约 **38.56% / 39.44%**；短流开销明显更高，主动等待最大值也未通过严格 30 ms。它不是新做的 session18 全套性能测试。依据：[信用等待诊断报告](references/veil/docs/v0.5-credit-flow-report.zh.md)。

## 9. 证书默认策略的依据与落地边界

Let's Encrypt 已正式开放 IPv4/IPv6 证书，`shortlived` profile 有效期 160 小时。IP 验证采用 HTTP-01 或 TLS-ALPN-01；默认计划优先 HTTP-01，需要公网 TCP 80 可达。公网 IPv4 本身不证明挑战端口可达，安装过程须验证实际控制权。DNS-01 不用于 IP 证书。

已核实的官方资料：[正式开放公告](https://letsencrypt.org/2026/01/15/6day-and-ip-general-availability)、[Certbot 支持说明](https://letsencrypt.org/2026/03/11/shorter-certs-certbot)、[挑战类型](https://letsencrypt.org/docs/challenge-types/)。具体策略见 [项目证书策略](references/veil/docs/v0.5-public-ip-certificate-policy.zh.md)。

服务端用公开证书，客户端继续用独立的私有身份认证。新服务端证书不能替换客户端身份，普通换证不能随机更换模型种子或客户端授权。

尚未提供并使用一个实际受控 VPS 公网 IP/执行环境进行本轮公网签发。接手应在需要真实外部执行时获取具体环境，不再把“缺少域名”作为必需前置条件。当前部署控制器要求 UID1000；Certbot 常见 root 私钥与 UID1000 读取权限之间的交接需要明确实现，不能凭默认路径假定成功。

## 10. 复查和开发入口

默认工作目录是 `/workspace/projects/Project-Veil/references/veil`。下列命令用于定位和校验，不会自动启动公网服务。

```sh
cd /workspace/projects/Project-Veil/references/veil
bin/veil-generation-runtime version
sha256sum bin/veil-v4 bin/veil-generation-runtime bin/sing-box-veil-tun-v7-dns2
python3 deploy/session17/release.py --binary bin/veil-generation-runtime --help
python3 deploy/session17/renew.py --help
```

最近的换证证据可以离线重放分析，输出写新位置：

```sh
python3 experiments/sessiondeploy/analyze_certificate_rotation.py \
  --run docs/results/v0.5-server-certificate-rotation/run-03 \
  --out /tmp/veil-handoff-certificate-analysis.json
python3 experiments/sessiondeploy/verify_certificate_analysis.py \
  --run docs/results/v0.5-server-certificate-rotation/run-03 \
  --out /tmp/veil-handoff-certificate-negatives.json
```

Python 证书夹具/分析器使用 `cryptography`，版本记录在该 run 的 environment 中；其他实验还需要 Docker、OpenSSL、抓包/网络工具及固定工具镜像。每个脚本的依赖、参数、固定二进制和镜像应以其源代码及对应报告为准。

本机已有离线 Go 工具链缓存；迁移机器时不保证这些 `/tmp` 路径存在：

```sh
VEIL_GO=/tmp/veil-modcache/golang.org/toolchain@v0.0.1-go1.26.7.linux-amd64/bin/go
export GOTOOLCHAIN=local GOMODCACHE=/tmp/veil-modcache GOCACHE=/tmp/veil-go-cache
export GOPROXY=off GOSUMDB=off
"$VEIL_GO" test ./internal/... ./destinationpolicy ./tundns ./cmd/veil-session
```

需要竞态验证时对实际改动范围运行相应 `-race`；不要把上述核心检查当作部署或完整验收。**不要直接运行旧 `make build` 覆盖 `bin/veil`；不要在这个混合研发树盲目执行 `go test ./...`。** `docs/results`、`test-runs` 中有历史源码和第三方副本，整个递归范围会混入不属于当前主线的包。新开发构建可使用独立新路径，但只有按冻结构建记录重建并比较哈希，才能宣称复现旧二进制。

## 11. 冻结材料与本次交接复核

2026-09-15 21:43–21:44 本次复核结果：

- 47 份 `docs/results/*/artifact-manifest.json`，合计 **8,348 个产物文件**，全部按字节数和 SHA-256 验证一致。部分清单为 `{scope, files}` 包装，读取时兼容 `files` 字段。
- 最近证书阶段冻结 **303 个文件**，清单 SHA-256：`f2a837a25d00ca4e585e8d87ce19e51ccaaa8aecfc5b8c01ce2088791cd2067a`。
- 最近整体源码快照为 **364 个文件**，归档 SHA-256：`820c8392d7e15e9b5b7b4600b6fe45007bd92a1c02051ea089daf3c358c83fc2`。
- 本次将当前工作树逐文件与最近 364 文件源码清单比较，没有发现差异。这证明本次中断后的紧急恢复分支尚未产生后续源码修改。
- 本次重新计算了 0.4、session18 和 DNS2 三个关键二进制哈希，与本文件列出的身份一致。
- Docker 中 session18 镜像仍在，linux/amd64；未发现 `veil` 命名的容器或网络，没有重新启动耐久任务。

最新阶段入口：[源码](references/veil/docs/results/v0.5-server-certificate-rotation/source.tar.gz)、[源码清单](references/veil/docs/results/v0.5-server-certificate-rotation/source-manifest.json)、[产物清单](references/veil/docs/results/v0.5-server-certificate-rotation/artifact-manifest.json)、[独立分析](references/veil/docs/results/v0.5-server-certificate-rotation/analysis-02.json)、[21 类对照](references/veil/docs/results/v0.5-server-certificate-rotation/negative-analysis-02.json)、[冻结重放](references/veil/docs/results/v0.5-server-certificate-rotation/replay-verification.json)、[历史保存复核](references/veil/docs/results/v0.5-server-certificate-rotation/preservation-after.json)、[清理复核](references/veil/docs/results/v0.5-server-certificate-rotation/independent-cleanup.json)。

全天耐久目录尚不是一个通过验收的完整阶段，不能因为其他 47 份规范清单通过而把它计为完成。其启动记录、失败和中断收集单独保留。

## 12. 给接手 agent 的建议顺序

1. **先保全和确定主线。** 单独保存被 Git 忽略的现有源码、冻结证据和关键二进制/镜像，审查 `local` 等私有材料，再设计正式 Git 跟踪/大产物保存方式。保留参考提交、补丁和许可证。不要为“清理目录”直接删除历史失败材料。
2. **整理可重复的工程入口。** 明确当前 runtime 与 TUN 适配器的独立构建、版本、打包、安装、配置校验和 CI 范围；处理旧 README、Makefile、部署模板与当前版本混杂。目录调整要同步修复相对路径、脚本 ROOT 推导和证据索引。
3. **收敛当前设计与可部署范围。** 在现有合同和实际源码基础上整理统一规格，先固定有限 profile、平台/网络语义与失败行为；新机制差异审查和检测评估仍保持原要求。
4. **完成运维恢复闭环。** 继续失效证书退役/恢复、ACME 真实接入、调度和告警，再与宿主/TUN 启动恢复结合。普通服务器换证已有可复用的实际测试。
5. **修正耐久观察和监督后再长跑。** 用真实存活句柄/容器状态监控；观察失败不能自动当任务死亡，已终止任务不能继续显示运行。先解决 1024 事件上限与全天要求的矛盾，保留新一次独立原始目录。
6. **补齐当前候选验收与检测。** 新工程版本重新验证角色绑定、轮换、错误输入、故障恢复、弱网/TUN/长期行为；建设当前 HTTPS 语料与冻结检测流程。
7. **性能仍暂停。** 用户重新安排后再处理，使用既有严格口径和失败结果，不把工程化迁移带来的测试范围变化当作性能达标。

交接完成后由接手者继续上述工程化和剩余验收。本次仅整理状态与设计资料，未继续实现紧急恢复，也未将完整 Veil 0.5 目标标记完成。
