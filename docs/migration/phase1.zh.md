# 第一阶段工程化迁移

日期：2026-09-16。版本：0.5.0-engineering1。

## 已迁出的正式工程

core、node、go.work、docs、tools、examples 等现位于 Project-Veil 根目录。新源码不再位于被忽略的 references 内。原始 references/veil、sing-box、实验和冻结结果保持原位，作为可追溯的研发资料。

首次导入 129 个源文件和测试向量；[清单](initial-import.json) 记录原路径、源摘要、首次转换后的摘要及当前目标路径。导入的是交接时工作树，不能宣称等同冻结 session18 二进制来源。tools/verify_migration.py 重新校验导入源文件和三项冻结二进制。

## 当前实现

- 两个独立 Go 模块，根目录统一构建和检查工具。
- 规范 Endpoint、Stream/Dialer、UDP Association、内存模型与身份输入。
- core 直接接入已有多流运行库；新增读接口按实际应用读取释放信用。
- 动态 I/O 期限、独立半关闭、取消、关联边界、等待任务后归还槽位。
- node 中独立的配置、SOCKS、生命周期、装配与 Linux/Windows 文件读取器。
- veild/veilctl 前台入口和独立节点配置格式；版本号和输出路径与冻结候选分开。
- Linux 实际 TLS/h2 双端、SOCKS、TCP 半关闭和 UDP 合同回归；保留继承的核心回归。

## 已知迁移边界

core/internal/session 保留原运行库的聚合实现与历史内部构造；本轮没有把全部 carrier/identity/session 私有代码重新拆包。公开 API 已隔离文件路径，内部历史文件夹具仍用于回归。

Windows 代码交叉构建通过后也只能记为构建结果；本机没有执行原生 Windows、ACL 或系统服务测试。当前文件读取器不等同设计中的完整 PrivateStore；持久提交、DNS 状态、TUN、路由/DNS/保护事务、控制 IPC、换证恢复及安装器仍待实现。

新 node 配置是 version 1，不能直接使用 session18 私有发布包、旧 activate/renew 脚本或旧镜像标签。当前命令为前台运行；后续 packaging 模块负责安装、服务管理和回退。

事件公共类型与有界缓冲区已经加入，完整分段归档、运行身份和耐久监督仍要继续完成。已有短时回归不能替代全天耐久、当前版本检测或性能验收。

## 验证记录

本轮最终验证全部通过：

- core 和 node 的完整 Linux 测试及竞态检查通过，包括直接 TCP/UDP、半关闭、动态期限、SOCKS 全链路、启动失败和组件退出。
- core 和 node 的 Windows/amd64 交叉构建通过；实际生成 Linux/amd64 和 Windows/amd64 的 veild、veilctl，共四个二进制。
- Linux 成品执行 version/capabilities 成功；Windows 成品未在原生系统执行。
- 129 个导入源文件和三个冻结二进制的 SHA-256 保全检查通过。
- 测试和产物绑定同一源码摘要：`7af8659db826c74bebcfd9f6e5fd924a8d046482e4fcd081305b473e71112e3b`。

可跟踪的小型证据保存在 [phase1-evidence](phase1-evidence/checks.json)：[构建清单](phase1-evidence/build.json)、[源码清单](phase1-evidence/source-manifest.json)、[core 竞态日志](phase1-evidence/core-tests.log)、[node 竞态日志](phase1-evidence/node-tests.log)、[保全结果](phase1-evidence/preservation.json)。

原始任务目录为 out/checks/20260915T161135Z-7a1a4422 和 out/builds/20260915T161134Z-f6aeb129；实际二进制在后者的 linux-amd64、windows-amd64 中。out 保持忽略，大产物未进入普通源码跟踪范围。

迁移完成时，新文件尚未提交。2026-09-16 按用户要求将核心库、节点运行程序、工程工具与文档分批纳入 Git；提交记录和后续待办见根目录 [handoff.md](../../handoff.md) 的当前工程交接部分。references 和 out 中的大产物继续保持忽略，未纳入普通源码提交。
