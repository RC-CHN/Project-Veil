# Veil Desktop

Windows / Linux 桌面端使用 Wails v2 与 Go 公共控制层。界面直接调用 `control.Manager`，数据由同一进程中的 Veil 核心转发。

- 管理多条连接与共用中转，导入/导出包含完整路径的连接包；表单编辑服务器、认证、SNI、REALITY 公钥、浏览器模板、CA 证书和 SOCKS5 / HTTP / mixed 入口。
- 配置校验、私有保存、启停、确认后应用更改。
- 运行状态、连接计数及诊断信息。
- 简体中文 / English、浅色 / 深色外观。
- 关闭窗口收进托盘，托盘菜单或界面可明确退出，自绘确认面板。
- Windows / GNOME 系统代理自动设置与恢复、不改变设置、显式清除。
- 独占实例目录，退出关闭代理，重新打开保留配置。

## 安装

Linux 发行包解压后，以当前桌面用户执行：

```sh
python3 install.py install
```

程序安装到 `~/.local/lib/veil-desktop`，应用菜单中显示 Veil。升级时退出应用，再从新包执行同一命令。卸载保留连接配置：

```sh
python3 ~/.local/lib/veil-desktop/install.py uninstall
```

安装器需要 Python 3，可用 `--prefix PATH` 指定其他安装前缀；自定义前缀的 `share` 需在桌面的 `XDG_DATA_DIRS` 中才能显示菜单入口。Linux 程序动态链接 GTK3、WebKit2GTK 4.1 和构建系统的 libc，请使用匹配发行版的包或在目标系统构建。

Windows ZIP 解压后运行 `veil-desktop.exe`，系统需安装 WebView2 Runtime。升级时退出应用再替换程序目录；移除程序目录会保留用户配置。启动失败或同一配置目录已被占用时，会显示系统对话框。

## 构建

Go 1.26.3、Python 3。Linux 使用 GTK3 与 WebKit2GTK 4.1 开发库；Windows 使用系统 WebView2 Runtime。前端为内嵌 HTML/CSS/JavaScript。

```sh
# Debian / Ubuntu 构建依赖
sudo apt-get install libgtk-3-dev libwebkit2gtk-4.1-dev

# 在仓库根目录执行
python3 veil-desktop/scripts/build.py --version 0.3.4
python3 veil-desktop/scripts/build.py --check
```

输出 `veil-desktop/.build/veil-desktop`，Windows 输出 `veil-desktop.exe`。构建器复用核心固定版本的 TLS 适配与 batch 后端。Linux 可用 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 交叉构建 Windows 二进制。

`--check` 包含系统代理生命周期回归。Linux 原生设置测试使用内存后端；Windows 原生 WinINet 写入测试仅在环境变量 `VEIL_TEST_SYSTEM_PROXY=1` 时运行，请在隔离测试会话中启用，测试结束会恢复原设置。Windows CI 已启用此检查。

生成发行包：

```sh
python3 veil-desktop/scripts/package.py --version 0.3.4
python3 veil-desktop/scripts/package.py --version 0.3.4 --target windows --arch amd64
```

输出位于 `veil-desktop/.build/releases`：Linux 为 tar.gz，Windows 为 ZIP。每个包附带 SHA-256 文件，包内 `manifest.json` 记录源码提交、工作区状态、Go 版本及文件摘要。Linux 包使用本机工具链和开发库构建。

## 使用

启动应用，新建连接或导入服务端管理员提供的 JSON 配置，检查服务器与本地监听地址，勾选「应用时启动此连接」，点击「保存并应用」。保存会自动校验，无需先单独校验。在浏览器或其他应用中配置相应的本地代理地址。

「仅保存」保留当前运行连接；「保存并应用」会使用编辑区里的最新配置，确认后保存并重启。已保存的待生效配置可用「应用更改」。表单与 JSON 双向同步，高级参数会保留。JSON 编辑区包含连接凭据，默认折叠。诊断区只显示运行状态与错误，不返回配置。

默认状态目录为 `os.UserConfigDir()/Veil/desktop`；可通过 `-state-dir PATH` 指定独立实例。Webview 的数据及缓存位于此目录下，与其他桌面应用分离。界面语言和外观会记忆选择；系统文件选择器等原生控件遵循系统语言。

关闭窗口会收进托盘，代理继续运行；没有可用托盘的桌面会最小化到任务栏。通过托盘菜单或界面「退出」并确认后关闭程序。应用重新打开后可显式连接到已保存配置。

系统代理默认「不改变」。选定要用于系统代理的连接，再选择「自动配置系统代理」；连接时将系统 HTTP / HTTPS 代理指向选定连接的 HTTP 或 mixed 监听端口，断开或明确退出时恢复原设置；收进托盘保持连接和设置。切回「不改变」也会恢复 Veil 接管前的设置。「清除系统代理」需确认，清除后切到「不改变」，本地代理继续运行。恢复前会检查设置是否仍属于 Veil，保留其他软件在此期间作出的修改；异常退出后，下次启动会尝试恢复。

Windows 使用当前用户的 WinINet 设置，Linux 支持 GNOME 系统代理。应用需遵循系统代理设置才能自动使用；其他应用可手动填写本地 SOCKS5 / HTTP 地址。

## 中转连接

完整的服务器安装、同端口多出口、连接包和双层 Veil 配置见[中转部署与连接](../veil-service/RELAY.md)。
