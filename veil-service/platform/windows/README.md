# Windows CLI

`veild.exe` 与 `veilctl.exe` 通过本机命名管道复用公共控制协议 v1。管理通道和配置目录只授予当前用户与 LocalSystem 访问权限；客户端在发送配置前检查已连接管道的所有者及 ACL。每个实例使用独立状态目录和管道，目录锁在进程退出后由 Windows 自动释放。

## 构建

在仓库根目录，使用 Go 1.26.3 与 Python 3：

```powershell
python veil-service/scripts/build.py batch --version 0.3.1
```

生成 `veil-service/.build/veild.exe`、`veilctl.exe`。也可从 Linux 使用 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 交叉构建；ARM64 将 `GOARCH` 改为 `arm64`。

## 使用

将两个程序放在同一个工作目录。第一个 PowerShell 窗口运行管理进程：

```powershell
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$pipe = "\\.\pipe\veil-$sid-default"
$state = Join-Path $env:LOCALAPPDATA "Veil\cli\default"
.\veild.exe -state-dir $state -socket $pipe
```

第二个窗口使用相同管道名称：

```powershell
$sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
$pipe = "\\.\pipe\veil-$sid-default"
.\veilctl.exe -socket $pipe -config C:\path\client.json validate
.\veilctl.exe -socket $pipe -config C:\path\client.json save
.\veilctl.exe -socket $pipe start
.\veilctl.exe -socket $pipe status
.\veilctl.exe -socket $pipe stop
```

配置支持 `inbound` 为 `socks`、`http` 或 `mixed`；`mixed` 同时接受 SOCKS5、普通 HTTP 和 HTTPS CONNECT。监听 `127.0.0.1` 供本机应用使用，监听 LAN 地址可供局域网使用。

保存后用 `restart` 应用更改。`-if-revision HASH` 可防止旧界面覆盖新配置。`-autostart` 让管理进程启动时恢复已保存的代理；Ctrl+C 关闭管理进程及其代理。状态与错误写到标准输出/标准错误，可由外部进程管理器收集。

新的状态目录会自动创建私有 ACL。已有共享目录会被拒绝，程序不会修改其他目录的权限；可指定一个新的空子目录。配置先写入具有显式私有 ACL 的临时文件，再通过 Windows 替换操作保存。

## 验证入口

Windows 原生测试包含管道首实例互斥、取消关闭、目录和配置权限、重复进程与配置恢复：

```powershell
python veil-service/scripts/build.py batch --check
python veil-service/scripts/windows_smoke.py --bin-dir veil-service/.build
```
