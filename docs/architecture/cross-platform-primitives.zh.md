# Veil 工程化：跨平台抽象与基础原语草案

日期：2026-09-15。状态：设计草案；首批接口已开始实现，其余合同仍待收敛。当前实际范围及验收见 [第一阶段迁移报告](../migration/phase1.zh.md)。

目标：把 Veil 拆成可独立使用的 core、共用后台 node，以及后续 Windows/Linux 用户界面；让各模块能够依据接口合同并行开发。当前 session18、历史失败记录、源码归档及二进制继续作为迁移依据。性能工作仍暂停。

## 1. 设计结论

采用 **小接口、显式组合、共用状态机、平台适配器、契约测试**。

Go 中用接口表达“基类”的可替换能力，用具体结构体承载共享实现。接口围绕调用方需要的行为定义；共享状态机负责执行顺序、预算和恢复，平台实现负责系统操作。

优先确定以下原语：

| 层次 | 原语 | 核心合同 |
|---|---|---|
| 业务传输 | Endpoint、Stream、Association、Dialer | 地址身份、TCP 顺序与半关闭、UDP 边界、取消 |
| 承载依赖 | AddressResolver、TCPDialer、UDPDialer | 完整解析、固定数值端点、可中断 I/O |
| 运行管理 | Runner、Readiness、Lease、Snapshot | 启动成功点、退出等待、资源归还、状态 |
| TUN | PacketDevice | 一次一个 IP 包、缓冲区所有权、可中断读写 |
| 系统集成 | RouteBackend、DNSBackend、GuardBackend | 有归属的操作、结果核对、条件恢复 |
| 持久状态 | PrivateStore、Revision、CommitResult | 独占写入、持久提交、冲突和不确定结果 |
| 身份与控制 | IdentitySnapshot、LocalPeer、ControlTransport | 证书快照、操作系统对端身份、授权 |
| 观测 | EventSink、EventCursor、HealthSnapshot | 有界、非阻塞、可见的丢失、健康层次 |

标准库已提供的 context.Context、io.Reader/Writer、net.Conn、net.Listener、time.Time 等直接复用。仅补充项目需要而标准接口未表达的语义。TLS、密码随机数和 HTTP/2 继续使用成熟实现。

## 2. 对上一版目录草案的细化

仍以 core、node 两个主要 Go 模块开始。下面只列接口相关目录，其余 packaging、apps、tests、docs 等沿用目录草案。

~~~text
core/
├── go.mod
├── client.go                       # 构造和组合入口
├── server.go
├── options.go
├── endpoint/                       # 叶子包：规范地址及端口
├── transport/                      # 叶子包：Stream、Association、Dialer
├── underlay/                       # 原生解析和拨号依赖接口
├── model/                          # 模型公开值类型、解析与校验
├── policy/                         # 纯目的地判定
├── identity/                       # 经过验证的不可变身份快照
├── telemetry/                      # 公开事件、状态及稳定错误分类
├── internal/
│   ├── session/                    # 会话组合与生命周期
│   ├── carrier/                    # TLS/h2、对象事务、lane、执行代次
│   ├── behavior/                   # 模型执行
│   ├── admission/                  # 身份准入、配额租约
│   ├── destination/                # 完整答案检查、数值端点固定
│   └── ...                         # streammux、datagram、flightwindow 等
└── contracttest/                    # 测试辅助包：公开原语合同

node/
├── go.mod
├── cmd/
│   ├── veild/
│   └── veilctl/
├── controlclient/                  # GUI/CLI 可使用的公开客户端
└── internal/
    ├── compose/
    │   ├── common.go               # 注入依赖、组合组件
    │   ├── host_linux.go           # 编译期选择 Linux 实现
    │   └── host_windows.go         # 编译期选择 Windows 实现
    ├── lifecycle/                  # Runner、就绪、监督、退出顺序
    ├── ingress/
    │   ├── socks5/
    │   └── tun/                    # PacketDevice、包到流的桥接合同
    ├── dnsmap/                     # 映射状态机、Store 消费接口
    ├── network/                    # 路由、DNS、保护规则接口及事务
    ├── state/                      # PrivateStore、提交结果、持久事务
    ├── credentials/                # 身份加载、续期、退役与替换
    ├── deployment/                 # 发布代次及激活流程
    ├── control/                    # 管理协议、对端授权
    ├── observability/              # 归档、指标、探测
    └── platform/
        ├── linux/
        │   ├── tun/
        │   ├── network/
        │   ├── state/
        │   ├── ipc/
        │   └── service/
        └── windows/
            ├── tun/
            ├── network/
            ├── state/
            ├── ipc/
            └── service/
~~~

### 依赖约束

- core 根包组合内部实现；内部实现引用 endpoint、transport 等叶子包，不反向导入 core 根包。公共类型不要全部堆在根包后再被内部包引用，避免导入环。
- endpoint 仅依赖标准库；transport 依赖 endpoint；underlay 依赖 transport 和标准库。公开接口和结构字段不得泄露 internal 类型。
- core 不导入 node、桌面框架、TUN 驱动、系统服务管理或安装器。
- node 的共用逻辑依赖接口；平台包导入接口所在包并实现它。只有 compose 引用具体平台实现，共用逻辑不反向导入平台包。
- apps 默认只通过本地控制协议管理 node；直接嵌入的第三方程序可使用 core 公共 API。
- integrations 中的第三方代码不能跨越 Go internal 边界。当前 sing-box 桥接先以受监督的独立进程接入；其冻结版本继续独占自己的 DNS 状态。以后迁移到 node 时显式交接所有权，禁止双进程同时写同一状态。
- core/model、identity、telemetry 的公开表示由本包定义，内部实现负责转换；不能为了导出一个内部结构而让整个实现变成公共 API。

接口集中在它所属的领域内，不建立涵盖文件、网络、证书、日志的万能 Platform 或 BaseService。

## 3. 业务传输原语

以下 Go 片段是签名草案，省略 import 和部分值类型定义，不是可直接编译的 SDK。

### 3.1 Endpoint：保留目的地身份

endpoint.Endpoint 使用构造函数建立规范值，区分域名、IPv4、IPv6，端口独立存储。内部表示不允许调用方任意修改，零值无效。

- 域名由纯校验函数规范化，解析器不参与构造。
- 域名请求进入 core 时仍携带域名；不能在 SOCKS/TUN 层提前转换成一个 IP 并丢弃原身份。
- 数值地址通过 netip 表示；迁移保留现有映射地址、zone、端口等校验规则。
- 会话 ID、流 ID、认证主体、本机用户身份使用不同类型，不能以一个通用字符串相互替代。
- 前端输入、配置值和线路编码分别校验；公开类型不等于允许绕过线路检查。

### 3.2 Stream：字节流和半关闭

~~~go
// core/transport
type Stream interface {
    net.Conn
    CloseWrite() error
}

type StreamDialer interface {
    DialStream(ctx context.Context, target endpoint.Endpoint) (Stream, error)
}
~~~

合同：

1. Stream 提供有序字节读写、期限及并发安全。并发写入会串行化，调用者需要确定业务顺序时自行排序。
2. Write 返回 n 表示本端已接纳的字节数；不表示远端应用消费。部分写入后返回错误时，不能重发整个缓冲区。
3. CloseWrite 对该流提交发送结束，排在此前成功接纳的字节之后；返回成功不等于远端已经读完。它保持接收方向可用，重复调用保持终态。
4. Read 的 io.EOF 表示对端发送方向完成，不能自动关闭本地发送方向。
5. Close 中断该流的阻塞操作。配额释放由 core 内部在相关任务退出后执行；调用方不持有“提前归还配额”的接口。
6. CloseWrite 与 Write 竞争时以内部串行化顺序为准；关闭之后的写入失败。发送关闭控制记录也受已有期限和有界队列约束。
7. DialStream 的 ctx 约束建立过程；建立成功后的流生命周期由 Client.Run 的父上下文、Stream.Close 和 I/O 期限管理。业务适配器若希望业务上下文取消关闭流，应显式建立并撤销对应关闭钩子。
8. 承载失效会结束所属流。新建承载只服务后续新流，当前合同没有跨 TLS 透明续传或自动重放。

逻辑流的地址和外层套接字地址分别记录：公开 Stream 的 RemoteAddr 表达请求的业务目标，域名保持为域名；LocalAddr 表达本地逻辑端点。它们不冒充未获知的目标原生套接字地址。外层 TCP 地址通过独立遥测字段提供。原生拨号器返回的连接则保留真实原生地址，消费者不得依赖具体 *net.TCPAddr 类型断言。

上述接口沿用 Go 的连接、期限及建立上下文概念；Go 文档也明确建立成功后拨号上下文到期不会自行关闭连接。项目额外定义了半关闭、配额及会话语义。[Go net 文档](https://pkg.go.dev/net#Conn)、[DialContext 文档](https://pkg.go.dev/net#Dialer.DialContext)。

core 实现的 Stream 不需要暴露底层 TCPConn、文件描述符或 Windows HANDLE。需要模拟半关闭的测试实现也必须真实保留另一方向，不能将 CloseWrite 简化成 Close。

### 3.3 Association：带目的地的 UDP 关联

~~~go
type Association interface {
    Send(ctx context.Context, target endpoint.Endpoint, payload []byte) error
    Receive(ctx context.Context, dst []byte) (n int, source endpoint.Endpoint, err error)
    Close() error
}

type DatagramDialer interface {
    OpenAssociation(ctx context.Context) (Association, error)
}

type Dialer interface {
    StreamDialer
    DatagramDialer
}
~~~

- 一次 Send 对应一个报文；目标逐报文指定，接收返回来源。关联数量、目标数、报文大小及队列字节都有上限。
- Send 在本地有界队列接纳整条报文时成功；接纳前取消返回错误。接纳和取消竞争时，接纳提交点获胜则返回成功。它不提供远端 UDP 投递或应用确认。
- Send 返回后不保留调用方 payload；需要异步使用时，在受预算约束的缓冲区内复制。
- Receive 把一条报文复制到调用方缓冲区。缓冲区过小则消费并丢弃该条，返回 n=0、ErrShortBuffer；禁止把截断内容当作完整报文交付。空报文是有效数据，n=0 且 err=nil 不表示 EOF。
- 操作按各自 ctx 取消；Close 解除所有阻塞。允许并发调用，内部读、写分别串行化；等待中的工作数量仍受调用方准入限制。
- 不自动分片或重组，不加入隐含重试；诊断分别记录本地拒绝、队列丢失、目标发送错误及承载失败。

SOCKS 和 TUN 的流转换层只使用 transport.Dialer；它们不需要理解 lane、flight、模型代次或 TLS exporter。

## 4. 原生网络依赖与认证边界

### 4.1 解析和拨号分开

~~~go
// core/underlay
type AddressResolver interface {
    Resolve(ctx context.Context, canonicalDomain string) (AddressSet, error)
}

type TCPDialer interface {
    DialTCP(ctx context.Context, target netip.AddrPort) (transport.Stream, error)
}

type UDPDialer interface {
    DialUDP(ctx context.Context, target netip.AddrPort) (ConnectedDatagram, error)
}
~~~

AddressSet 必须表示完整的 A/AAAA 地址结果及完整性状态。超限、任一必要查询失败、无法判断完整性时返回失败，不能裁剪成一个允许子集。两族无地址状态需要显式区分正常不存在与查询失败。服务端策略仍按当前合同检查全部地址，之后只把已检查的 netip.AddrPort 交给原生拨号器。

ConnectedDatagram 表示固定一个数值目标的原生 UDP 通道，提供保留边界的 Send、Receive、Close；它和对外支持多目标的 Association 分开。原生发送返回错误可能无法证明目标没有收到，因此调用层不能自动重发。

拨号器接口只接受数值端点，从接口上消除第二次域名解析。准入、解析结果检查、身份绑定留在 core；平台拨号实现负责接口绑定或系统套接字选项。注入实现属于受信任代码，接口本身不构成对恶意实现的隔离。

客户端受管 DNS 的 CNAME、NXDOMAIN/NODATA、两族一致性及 UDP→TCP 恢复另由 DNS 消费接口表达；不得把简化的 AddressResolver 误当成完整 DNS 消息 API。现有 [DNS/TUN 合同](../../references/veil/docs/v0.5-tun-dns-contract.zh.md) 是迁移基准。

### 4.2 承载 TLS 归 core 管理

- 平台提供原生连接和可选的网络绑定能力；TLS/h2、证书校验、ALPN、模型和 exporter 绑定由 core 统一执行。
- 身份材料以经过验证的不可变快照注入。配置文件路径、账户权限和续期程序属于 node。
- 外部调用方可提供证书和受控签名器等必要材料；公开配置仍必须通过 core 的协议和身份约束检查。
- 不为每个平台另写 TLS 或密码算法，不提供通过平台适配跳过认证的路径。
- 模型随机种子和密码随机性职责分开。生产密码随机数使用可信实现，确定性测试随机源限于测试构造。
- 时间注入首先用于纯状态机的期限测试。时长预算与证书墙上时间分别处理，进程重启不能通过重新起算解除既有持久约束。

## 5. 生命周期、就绪和资源租约

### 5.1 共用生命周期

~~~go
// node/internal/lifecycle；组件可通过薄包装适配此合同。
type Runner interface {
    Run(ctx context.Context) error
}

type Readiness interface {
    WaitReady(ctx context.Context) error
}
~~~

~~~text
Created → Starting → Ready → Stopping → Stopped
             └──────→ Failed ←──────┘
~~~

- 构造函数校验和复制配置，不启动后台任务或修改宿主网络。
- 每个实例 Run 一次；重新启动创建新实例。第二次运行明确失败。
- Run 阻塞至该组件管理的任务全部退出；取消父 ctx 发起关闭。它返回前完成可执行的清理，并保留清理失败。
- WaitReady 返回该次启动的确定结果，启动失败也必须唤醒等待者；等待者自己的 ctx 取消只取消等待。
- 就绪只表示该组件启动条件达成。后续故障通过状态和 Run 结果报告；历史就绪不能充当当前存活信号。
- Supervisor 为整个停止过程设置期限，并分别处理停止超时和正常停止。未退出的任务不能被伪装成已经完成；必要的进程终止及其原因由宿主监督器记录。
- 共用业务包不直接处理 SIGTERM 或 Windows 服务回调。平台 service 将操作系统事件转换为生命周期调用，注册/卸载在 packaging 和部署流程中完成。

core 的 Client.Run 管理会话池；Server.Serve(ctx, listener) 从调用开始接管 listener，并在所有返回路径关闭它、等待其连接处理任务结束。node 组件以薄包装接入 Runner。调用者如需共享监听器，必须先提供独立的分流子监听器。

### 5.2 资源所有权

| 资源 | 持有者 | 释放点 |
|---|---|---|
| core 业务 Stream / Association | 调用方获得句柄，core 管理内部资源 | Close/父运行结束触发关闭；内部任务退出后归还预算 |
| 原生目标连接 | core/destination | 连接关闭且读写泵退出之后 |
| TUN 设备 | node 的 TUN 组件 | 停止接入、退出桥接任务之后 |
| 网络变更 | network 事务 | 根据记录核验并执行恢复或保留保护 |
| DNS 状态锁 | 唯一 DNS 状态所有者 | 全部签发和查询任务退出后 |
| 身份快照 | 当前运行实例 | 该实例全部会话退出后解除引用 |
| 归档文件 | observability | 最终刷新和结果记录后 |

admission 使用内部 Lease 结构体和幂等 Release；Release 不暴露给 GUI 或 SOCKS 调用方。对象回执、应用消费信用、内存/连接租约保持不同类型和不同释放条件。

关闭中的槽位仍占预算；不能为了快速“恢复可用容量”先减计数再等待任务。

## 6. TUN 与操作系统能力

### 6.1 包设备原语

~~~go
// node/internal/ingress/tun
type PacketDevice interface {
    ReadPacket(ctx context.Context, dst []byte) (int, error)
    WritePacket(ctx context.Context, packet []byte) error
    Info() DeviceInfo
    Close() error
}
~~~

- 设备边界传递一个完整 IPv4/IPv6 网络层包，不带平台专用头。
- DeviceInfo 包含逻辑标识、MTU、地址族和能力。原生句柄、驱动环形缓冲区和所有权留在平台实现内部。
- ReadPacket 使用调用者缓冲区，过小时丢弃本包并明确报错；WritePacket 不在返回后持有输入内存。MTU及包长分别检查。
- 基础合同支持一个读取者和一个写入者并行。多队列或零复制以后以可选扩展增加，不改变基础合同。
- 取消或 Close 必须解除阻塞，不能靠每次创建一个永不退出的 goroutine 包装不可中断系统调用。
- TUN 包到 TCP/UDP 流的转换由独立适配器承担。core 的流协议不导入设备驱动。
- 驱动选择和打包验证是后续平台任务；现有 Linux 适配器成功不代表 Windows 已实现。

### 6.2 能力声明

能力同时区分“实现是否支持”“当前机器是否可用”“当前身份是否获准使用”。例如 TUN 已实现但驱动缺失，和进程没有权限是不同结果。

~~~go
type Capability struct {
    Supported bool
    Available bool
    Authorized bool
    Reason Code
}
~~~

能力集合采用有名字的字段或枚举：TUN、IPv4、IPv6、路由接管、DNS 接管、保护规则、持久状态、系统服务。请求操作时仍重新检查实际条件，不能将旧探测结果当永久授权。

配置要求某项能力而不满足时，返回 Unsupported、Unavailable 或 PermissionDenied；可选功能的降级必须在实际配置与状态中显示。尚未完成平台能力可以先以 SOCKS 模式验收，不能把 TUN 静默降级成直连。

## 7. 网络变更原语与恢复事务

路由、系统 DNS 和防直连规则分别定义小接口。以下以路由为例：

~~~go
type RouteBackend interface {
    Inspect(ctx context.Context, scope RouteScope) (RouteObservation, error)
    Apply(ctx context.Context, intent RouteIntent) (RouteReceipt, error)
    Revert(ctx context.Context, receipt RouteReceipt) error
}
~~~

DNSBackend、GuardBackend 使用各自领域类型，不把平台命令或任意键值塞进一个通用执行接口。

### 每项变更的记录

Intent 在执行前包含稳定操作 ID、Veil 安装归属、预期配置代次、目标及必要前置条件。Receipt 保存真实系统对象标识、观察到的前后状态、完成情况。平台专用恢复信息使用带版本的私有载荷，由产生它的实现解读。

共用编排先持久写入 Intent，再调用系统操作，再保存 Receipt 并核验。若操作已生效但进程在写回执之前退出，恢复器依据 Intent 的稳定 ID、归属和真实状态重新核对。

~~~text
Planned → Applying → Applied → Verified
              ↓          ↓
            Unknown → Inspect / Reconcile
                         ↓
                    Reverting → Reverted
~~~

- Apply 错误不能自动解释成“没有变更”；错误结果必须保留部分回执或 Unknown 状态。
- 相同操作 ID 的重试先核对既有结果；不得创建无归属的重复对象。
- Revert 仅恢复仍由本事务持有且符合前置条件的对象。用户或其他程序已经修改时返回 Conflict，保留证据，避免整表覆盖。
- 配置激活时核对预期代次，防止准备期间活动配置变化后仍提交旧计划。
- 网络、证书和部署可以共用持久状态设施，各自保留领域状态机；不抽象出一个假定所有操作都可撤销的万能事务。

受保护 TUN 模式先建立必要的服务端例外路径及保护条件，核验后再切换路由和 DNS、开放应用接入。失败和主动退出的恢复目标不同：故障时可能必须继续保留保护；用户明确退出时按配置恢复原网络。不能将“关闭句柄”统一实现成删除所有保护规则。

### 网络变化和系统恢复通知

另设可选 ChangeSource 接口，由平台报告网络变化、系统恢复等提示：

~~~go
type ChangeSource interface {
    Next(ctx context.Context) (ChangeHint, error)
    Close() error
}
~~~

通知允许有界合并，并报告遗漏；收到提示后共用编排重新 Inspect，启动时和必要的周期核对也执行 Inspect。事件不是系统状态的唯一来源。接口索引、PID 等可能复用的数字不能独自充当永久资源身份。

网络切换或休眠恢复后重新核对路由、保护、身份时间和承载状态；需要重建时遵守既有流中断合同。不能凭一次“网络恢复”通知重新开放流量，也不能自动清空 DNS 高水位。

## 8. 私有存储与持久提交

### 8.1 抽象的是保证，而不是文件函数名

~~~go
// node/internal/state
type PrivateStore interface {
    Load(ctx context.Context, key Key) (Document, error)
    Commit(ctx context.Context, request CommitRequest) (CommitResult, error)
    Close() error
}

type CommitRequest struct {
    OperationID OperationID
    Key         Key
    Expected    Revision
    Payload     []byte
}

type CommitOutcome uint8
const (
    CommitUnknown CommitOutcome = iota
    CommitApplied
    CommitNotApplied
)

type CommitResult struct {
    Outcome  CommitOutcome
    Revision Revision
}
~~~

Store 由平台工厂在核验目录、权限和独占锁之后返回。Key 是该命名空间内允许的逻辑键；大小、键数量、暂存及日志上限由构造配置固定，不接受外部任意文件路径。

合同：

1. 正常 Open 只打开已初始化状态；Init 属于显式安装操作。缺失、损坏或所有者不符应拒绝，不能偷偷重建。
2. Load 返回有界、已验证文档，包含修订号和最近操作 ID。相同 Store 的操作串行化，跨进程最多一个写入者。
3. Commit 先比较 Expected。冲突返回 NotApplied；成功提交递增修订号，校验并复制有界输入。
4. Applied 且 err=nil 表示达到该后端已声明、已验收的持久性合同；仅写入页缓存不能宣称达到持久提交。
5. 一旦可能改变可见状态又无法确认，必须返回 Unknown，即使底层错误是超时或取消。当前 Store 停止继续写入，经过恢复核验后才能重新开放。
6. 操作取消不能推导出未提交。重启通过操作 ID、修订号、摘要及平台提交记录确认结果；不直接重复增加高水位或重新激活身份。
7. DNS 映射只有在 Applied 后才能签发正回答。不确定、损坏或旧修订恢复不满足签发条件。成功签发过的地址高水位不得因恢复而倒退。
8. 后端须在其支持的失败模型下保证已确认提交可恢复，未确认提交可判定或明确停用。实现可以使用经验证的日志或双槽提交机制，不能仅依赖单文件名称变化。
9. 进程崩溃恢复和宿主断电持久性分别记录验收；文件系统、存储类型及硬件假设必须写入平台支持范围。尚未验证的保证不能上报为支持。

Go 文档明确指出，非 Unix 平台上的 os.Rename 即使在同一目录也不保证原子操作，因此不能把 Linux 当前的 rename 流程直接当作 Windows 提交协议。[Go os.Rename](https://pkg.go.dev/os#Rename)。

Windows 的 FlushFileBuffers 提供文件缓冲刷新能力；项目仍需单独设计提交记录、替换、恢复和权限流程，不能凭一次刷新推断整个事务完成。[Microsoft FlushFileBuffers](https://learn.microsoft.com/en-us/windows/win32/api/fileapi/nf-fileapi-flushfilebuffers)。

### 8.2 平台实现范围

| 保证 | Linux 实现职责 | Windows 实现职责 |
|---|---|---|
| 私有访问 | 用户归属、权限、链接与路径检查 | 服务身份、ACL、继承与路径重定向检查 |
| 独占所有者 | 可验证的进程锁及锁定资源身份 | 对应文件/对象锁及服务实例身份 |
| 防路径替换 | 锁定资源与后续操作对象一致 | 句柄绑定与后续对象身份核验 |
| 提交恢复 | 验证写入、同步及恢复协议 | 验证日志/替换、刷新及恢复协议 |
| 发布包激活 | 当前部署代次与固定产物绑定 | 同一合同的平台安装布局 |

state 负责字节和提交保证；dnsmap 负责地址不复用和到期；credentials 负责身份退役；deployment 负责产物绑定。共用存储不会合并这些领域规则。

## 9. 身份、证书与本地控制

### 9.1 身份快照和替换

IdentitySnapshot 是通过 core 验证并封装的不可变运行材料，记录证书身份、用途、有效期和绑定信息。node/credentials 负责私有材料加载与更新来源。快照不可变不代表普通 Go 内存能够保证立即清零；密钥引用范围和释放时机要明确。

普通换证与失效身份退役使用不同流程：

- 普通换证验证新材料、持久准备、停旧启新、实际检查；是否允许回退取决于旧身份当前是否仍可用。
- 紧急退役先持久记录旧身份不再允许启用，再尝试新身份；失败保持停止。恢复器也执行该退役约束。
- 退役提交结果未知时，先核对持久状态，不能冒险启动旧身份。
- TLS 验证结果、撤销检查结果和运维退役状态分别表示；未执行联网撤销检查时明确为未检查。
- 不在平台 CredentialStore 中暗含“失败就恢复旧证书”的通用行为。

ExecutionEpoch（承载执行代次）、DeploymentGeneration（安装代次）、ConfigRevision、StoreRevision、IdentityID 各用独立类型。名字相似的计数器不共享重置规则。

### 9.2 本地控制原语

~~~go
type LocalListener interface {
    Accept(ctx context.Context) (net.Conn, LocalPeer, error)
    Close() error
}
~~~

LocalPeer 由系统适配层在连接建立时取得，包含身份命名空间、主体 ID 及可验证属性；不能信任请求 JSON 自报的用户名、UID、SID 或 PID。

Linux 可使用本地 socket，Windows 可使用命名管道，适配层向上提供有期限和可关闭的字节连接。管理协议统一版本、帧长上限、请求数上限、错误码和事件游标。

授权至少区分查看状态、修改配置、启停和安装级操作。客户端也必须验证所连接的是预期服务端；本地路径名称本身不构成双向信任。

Windows 命名管道的访问检查与安全描述符有关，因此实现时必须显式定义服务身份及允许的客户端，而不是依赖默认 ACL。[Microsoft 命名管道安全](https://learn.microsoft.com/en-us/windows/win32/ipc/named-pipe-security-and-access-rights)。

带副作用的请求包含操作 ID 和预期 ConfigRevision。客户端断开或超时后查询结果；不能把未知结果当作未执行而盲目再次提交。

## 10. 观测、错误和健康原语

### 10.1 有界事件

~~~go
// core/telemetry
type EventSink interface {
    TryEmit(event Event) bool
}
~~~

TryEmit 必须有界、非阻塞，不执行磁盘、网络或 GUI 回调。默认实现是有界队列，事件值包含有界、不可变字段。每次未接纳由生产者记录；消费者归档失败及后续丢失另外计数，避免遗漏或重复计数。

事件含运行实例 ID 和该实例内的递增序号；发生丢失时仍推进序号。node/observability 分段归档，限制文件大小、保留数量和磁盘预算。慢 GUI 消费者有独立队列，不得拖住协议路径。

订阅游标过旧或中间丢失时返回明确缺口及最新快照，不能伪造连续历史。零丢失耐久验收必须配置足够的归档/收集容量，并保留完整已归档片段；磁盘预算不足应使验收失败，不能靠删除丢失检查通过。

这为当前每次启动最多记录 1024 个事件的问题提供替换方向；本文没有实现该替换，也没有完成新耐久任务。

### 10.2 错误分类

公共错误使用稳定 Code，加操作名和可解包原因。至少区分：

Cancelled、DeadlineExceeded、Closed、Unsupported、Unavailable、PermissionDenied、InvalidConfig、AuthenticationFailed、PolicyDenied、ResourceExhausted、Conflict、CorruptState、CommitUnknown、ShortBuffer、Internal。

I/O 原有 EOF、期限及关闭判断保持兼容。平台细节留在受控诊断中，GUI 和恢复状态机不解析错误字符串。是否重试由业务语义、提交结果与调用阶段共同决定，不设计一个对所有操作都适用的 Retryable=true。

### 10.3 健康层次

- Process：该运行实例确实存活，状态时间未过期。
- LocalReady：所需本地组件与策略已就绪。
- Transport：认证和模型会话状态。
- EndToEnd：真实业务探测结果；没有执行时为 NotChecked。
- Protection：路由/DNS/防直连状态及是否退化。
- Credentials：有效期、续期和退役状态。

保持各维度独立。历史 running.json、一次启动成功或本地端口可监听均不能替代实时端到端健康。

## 11. 共用实现和平台实现的分工

| 共用实现 | 平台实现 |
|---|---|
| 协议、模型、预算、认证、流语义 | 原生网络绑定及必要系统选项 |
| 生命周期、取消和任务等待 | 系统服务事件与进程监督接入 |
| DNS 映射与持久提交调用顺序 | 私有文件访问、锁、提交与恢复后端 |
| 网络变更计划、日志、代次冲突处理 | 系统路由/DNS/保护对象操作与核验 |
| 身份更换、失效退役与恢复决策 | 私钥权限、签名器接入及材料读取 |
| 管理协议与操作授权规则 | 本地连接、操作系统对端身份 |
| 事件结构、归档策略、健康定义 | 平台状态采集 |

共享逻辑通过构造参数获得所需的小接口。具体实现可组合 state、network、service 等能力，不要求继承统一基类。

平台配置路径、服务账户和权限需求也通过 node 配置与平台构造处理；core 中没有固定 UID、系统目录或安装路径。抽象迁移保留已有建立期限、流/承载预算和支持模型的限制，新增接口不能顺带扩大默认配额。

纯算法、固定版本协议编码、模型执行器、有界队列优先保留具体类型；只有存在平台差异、外部 I/O、多个消费者或重要故障注入需求时才抽接口。

## 12. 契约测试与并行开发

每个接口冻结前必须明确：输入约束、正常结果、部分成功、取消、并发、所有权、预算、关闭、恢复、能力缺失。实现方共同运行合同测试；假实现用于共用状态机测试，原生实现用于平台合同验收。

| 契约组 | 关键验收 |
|---|---|
| Stream | 顺序、部分写、半关闭后继续接收、期限、并发 Close、建立取消与运行取消 |
| Association | 多目标、零长报文、小缓冲、大小/队列上限、取消、关闭唤醒 |
| Resolver/拨号 | 混合回答拒绝、超限拒绝、查询失败、只拨核验后的数值目标 |
| 生命周期/租约 | 启动失败唤醒、单次运行、退出等待、资源只归还一次、关闭中仍占预算 |
| PacketDevice | 包边界、MTU、双向并行、阻塞取消、驱动缺失 |
| PrivateStore | 并发所有者拒绝、私有权限、路径替换、修订冲突、提交前后强制终止、Unknown 恢复 |
| DNS 映射 | 重启不复用、提交前不签发、损坏拒绝、时钟回退、容量和序号耗尽 |
| 网络事务 | 仅本项目归属、每一步中断、回执丢失、外部修改冲突、重启恢复 |
| 证书恢复 | 普通换证失败、失效身份退役后禁止复活、提交不确定、客户端不必要重启 |
| 本地控制 | 对端身份、未授权请求、消息边界、超时后结果查询、版本不兼容 |
| 事件 | 慢消费者、队列满、归档失败、跨片段游标、耐久全记录核验 |

测试入口：

- core/contracttest 提供库消费者可运行的 Stream/Association 合同；真实双端回归与原始线路向量一起验证。
- node 的合同测试在对应 internal 包内，允许 Linux/Windows 后端分别参与。
- Windows 与 Linux 原生 CI 分别运行平台合同；跨平台编译仅作为第一道检查。
- 测试替身不能证明 ACL、文件持久性、TUN 或系统网络修改有效；这些需要原生受控环境。
- 将核心会话包的外部导入限制、core→node 禁止依赖和公开 API 的 internal 类型泄露纳入检查。
- 两个 Go 模块分别维护依赖和构建检查；本地 go.work 用于联调，发布检查同时验证固定依赖绑定，防止工作区替换掩盖缺失版本。

并行工作建议：

| 工作包 | 负责范围 | 依赖 |
|---|---|---|
| A 原语与合同 | endpoint、transport、underlay、telemetry 及公共合同 | 第一批先收敛签名和语义 |
| B core 迁移 | objecttransport 拆分、会话/承载/流与回归 | A |
| C 共用 node | 生命周期、SOCKS、配置、DNS/网络/证书状态机 | A；以合同替身对接平台 |
| D Linux 后端 | 平台接口、服务、打包、原生测试 | 相关领域接口 |
| E Windows 后端 | 同上，独立实现与原生测试 | 相关领域接口 |
| F 界面 | 控制协议、能力与健康呈现 | 控制协议和测试服务端 |

这些是计划中的工作划分；实际已实现范围与验证结果以第一阶段迁移报告为准。

## 13. 迁移顺序与待定项

### 第一阶段：原语和合同

1. 确认 core 公开包的依赖方向，提取规范地址和传输类型，清除依赖环。
2. 先建立 Stream、Association、拨号/解析及生命周期合同和适配测试。
3. 将原 objecttransport 的 SOCKS、文件读取、事件落盘职责分离；数据传输实现先通过薄适配保持已有行为。
4. 分开 DNS 纯状态机和 Linux 存储保证，依据同一合同设计 Windows 后端。
5. 用新的构建路径和版本绑定做 Linux/Windows SOCKS 基线及实际双端回归；历史产物保持可追溯。

### 第二阶段：平台和运维

在相应原语合同稳定后分别实现 TUN、系统网络、私有存储和本地控制后端；随后接入证书紧急恢复、部署事务和监控。完成事件归档与监督之后再启动独立耐久任务。

### 后续实现前需要继续收敛的技术决策

- Windows TUN 驱动/适配库、安装和分发方式。
- Windows 私有状态后端的具体提交协议及支持文件系统。
- 控制协议具体编码方式和首批消息集合。
- 第一批支持的系统版本、CPU 架构与权限模式。
- 现有 sing-box 桥接向 node 内部 DNS/TUN 的切换时机。

上述待定项通过局部技术验证决定，公共原语先保持稳定。GUI 框架和性能优化不作为第一阶段前置条件。

## 14. 当前代码依据

- [客户端：SOCKS 与会话混合](../../references/veil/internal/objecttransport/client.go)
- [配置、模型与证书文件读取](../../references/veil/internal/objecttransport/config.go)
- [地址类型与规范化](../../references/veil/internal/destination/address.go)
- [服务端完整 DNS 判定与目标固定](../../references/veil/internal/destination/connect.go)
- [目的地准入与幂等释放](../../references/veil/internal/destination/gate.go)
- [UDP 编码及边界](../../references/veil/internal/datagram/wire.go)
- [当前事件和状态类型](../../references/veil/internal/objecttransport/observe.go)
- [Linux DNS 状态与文件系统耦合](../../references/veil/tundns/store_linux.go)
- [当前 CLI 生命周期与事件上限](../../references/veil/cmd/veil-session/main.go)

本文现位于正式工程的 docs/architecture。core、node 和构建入口已经迁到 Project-Veil 根目录；references 保留原型、参考实现和冻结证据。Git 跟踪和实际提交状态以仓库为准。
