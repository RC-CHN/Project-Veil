# Linux 正式成品双端操作

版本：0.5.0-engineering2。当前是前台 SOCKS 客户端/服务端的准备、运行和诊断入口。系统服务、TUN、ACME、升级/回退、完整事件归档仍待实现；操作不需要 systemd 或 Docker。

## 构建与材料准备

先按根 README 配置 Go 工具链并运行 `python3 tools/build.py`。以下命令使用同一构建目录中的 Linux `veild` 和 `veilctl`。

真实部署使用已有的服务端证书完整链和匹配私钥；证书必须覆盖实际 IP 或域名。prepare 检查链、名称、用途、密钥及剩余期限，默认至少 48 小时；没有自行签发公开证书、联网检查撤销或执行 ACME。`--server-ca` 不提供时使用系统信任根；提供时采用显式根，不会把测试根自动装进操作系统。

```sh
veilctl prepare --out /private/new-pair \
  --server-address 192.0.2.1:443 \
  --server-cert /private/server-fullchain.pem \
  --server-key /private/server-key.pem
```

地址为占位，换成自己的 VPS。`--server-name` 可指定证书域名，`--server-address` 仍为实际拨号的数值 IP:port。`--server-listen` 默认监听相应地址族的通配地址和端口，`--client-listen` 默认 `127.0.0.1:1080`。输入文件路径不能经过符号链接，私钥应为 0600；ACME lineage 的链接需要由后续证书接入器验证并导出为受控普通文件，不能直接假定当前读取器会接受它。

输出目录必须不存在，父目录必须已经存在；Linux 写入器创建 0700 暂存目录和 0600 文件，全部准备后拒绝覆盖地发布新目录。文件/目录会同步，但尚未做宿主断电验收。失败或中断可能保留 `.veil-prepare-*` 私有暂存目录，需要检查后人工清理；本命令不更新活动安装、不覆盖已有身份，也不是通用持久事务。重复调用拒绝，不会悄悄更换模型。

```text
new-pair/
├── client/       node.json、模型、客户端身份，以及可选的服务端信任根
├── server/       node.json、相同模型、服务端身份、客户端 CA 与授权指纹
├── admin/        客户端 CA 和私钥，仅用于离线身份管理
└── prepared.json 不含私钥或种子的摘要、指纹与有效期记录
```

只将 `client/` 交给客户端，将 `server/` 交给 VPS；`admin/` 不部署到任一节点。运行用户需要拥有并可读取私有目录和文件。生成的客户端身份有效期为 30 天、客户端 CA 为一年；自动客户端身份轮换尚未实现，过期前必须安排后续身份更换。

## 本地测试身份

仅在本机联调时显式使用：

```sh
veilctl prepare --local-test --out /private/local-pair \
  --server-address 127.0.0.1:24443
```

此模式要求服务端拨号及监听均为环回地址；生成 24 小时的私有测试服务端证书、显式客户端信任根，并允许服务端连接环回目标。默认有效期余量降为 1 小时。它不代表公开证书签发，也不能作为公网 VPS 验收。普通准备模式不自动允许私有/环回目的地。

## 检查、运行与观察

```sh
veilctl configcheck --config /private/new-pair/server/node.json
veilctl configcheck --config /private/new-pair/client/node.json
veild --config /private/new-pair/server/node.json --status-interval 5s
veild --config /private/new-pair/client/node.json --status-interval 5s
```

双端通常分别运行在 VPS 和本机。当前没有安装器自动授予低端口权限；首次非特权联调可以显式使用 8443 等高端口，443 的最小权限配置留给 Linux 服务安装阶段。

`configcheck` 通过实际构造器校验输入，输出版本、角色、完整模型摘要、证书身份/到期时间和本地字节预算。它不拨号，`end_to_end=not_checked`；预算报告表示本地上限，对端协商会取较小值。

veild 输出 JSON Lines，包含 `schema_version`、`kind=node_status`、`version`、随机 `run_id`、`pid`、`phase`，以及原有快照字段 `LocalReady`、`EndToEnd`、`ActiveStreams`、`ActiveCarriers`、失败/拒绝/清理计数和 `Updated`。`phase` 包括 ready、running、stopped，启动失败可能输出 failed；配置文件读取失败仅通过非零退出和 stderr 报告。

每次进程启动有新 run_id。状态采样范围为 100ms..1h，0 表示仅启动/退出；SIGTERM 或 SIGINT 取消节点并等待组件退出。stdout 需要持续消费，当前没有异步日志归档和磁盘限额，阻塞输出会影响监督。监控必须同时核对实际进程和状态新鲜度；旧状态文件和 `LocalReady=true` 都不证明远端可用。

## TCP/UDP 端到端诊断

准备自己控制的 TCP/UDP 回显目标，TCP 目标需支持对端半关闭后返回全部收到的字节并结束响应：

```sh
veilctl probe --socks 127.0.0.1:1080 \
  --tcp-echo echo.example:7000 \
  --udp-echo echo.example:7001 \
  --timeout 10s
```

至少选择一种目标；名称和端口为占位。探测只连接环回 SOCKS 入口，不直连业务目标，也不在本地解析目标域名；域名由 SOCKS 请求交给 Veil 服务端处理。TCP 校验随机 32 字节、半关闭及完整响应；UDP 校验报文地址/边界和随机回显。总期限默认 10 秒，可设 100ms..2m。

输出 JSON `kind=probe`、逐项结果、核验字节数与错误；全部选中目标通过才返回零退出和 `end_to_end=passed`。结果只覆盖 `scope=requested_echo_targets`，不会把它写成节点永久健康，也不能证明任意站点、TUN、防直连或抗识别能力。

## 字节预算

缺省值保持不变。需要较大的业务流时，在双端配置中明确设置，例如：

```json
"limits": {
  "stream_bytes": 16777216,
  "carrier_bytes": 134217728
}
```

也可在首次准备时使用 `--stream-bytes 16777216 --carrier-bytes 134217728`。省略或设 0 分别使用 8 MiB 和 64 MiB。流上限为 1..1 TiB，承载上限为 4096..1 TiB；上限大于已验证的场景不代表该场景已经验收。

这些是累计字节预算，不是一次分配同等大小的内存。每流窗口仍为 64 KiB，每承载并发流默认 8，默认累计 OPEN 128；node 默认并发连接 8、最大 32，其余资源限制未放宽。对端取较小值，一端增大不能越过另一端限制。承载预算包含内层编码开销，不能按它精确推算可交付的应用字节。

达到流或承载字节预算会中断受影响业务；没有跨承载的透明字节续传或应用重放。后续新业务可建立新流/承载。协议结束、网络 EOF 或写入被本地接受都不等于文件完整，应由业务长度/摘要或应用层确认判断。12 MiB 下载完整性属于有界配置的正确性验收，不是性能达标。

## 可重复进程验收

```sh
python3 tools/accept_linux.py --build out/builds/本次构建目录
```

脚本验证构建源码及二进制摘要，启动正式 veild 进程和本次独占的环回 TCP/UDP/DNS 夹具，使用 prepare/configcheck/probe 走真实文件与网络路径。默认维持同一 TCP 和 UDP 关联至少 260 秒；5 秒先导只用于快速诊断。测试包括：权限拒绝、域名、目的地拒绝、并发半关闭、错误客户端身份/模型种子、服务端被杀后的新连接恢复、默认/协商/承载预算与显式较大预算的下载摘要。

退出时等待自己的节点和夹具退出、删除临时私有材料；状态、目标见证及验收结果写在独立 `out/linux-acceptance/` 目录。测试不会修改系统路由、防火墙、DNS 或启动服务管理器，也不访问公网。结果不能当作 TUN、systemd、真实 VPS、Windows/OpenWrt 原生支持或全天耐久的证据。
