# Veil Desktop

Windows / Linux 桌面端使用 Wails v2 与 Go 公共控制层。界面直接调用 `control.Manager`，数据由同一进程中的 Veil 核心转发。

- 导入 JSON 配置、编辑监听地址及 SOCKS5 / HTTP / mixed 入口。
- 配置校验、私有保存、启停、确认后应用更改。
- 运行状态、连接计数及诊断信息。
- 简体中文 / English、浅色 / 深色外观。
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
python3 veil-desktop/scripts/build.py --version 0.3.1
python3 veil-desktop/scripts/build.py --check
```

输出 `veil-desktop/.build/veil-desktop`，Windows 输出 `veil-desktop.exe`。构建器复用核心固定版本的 TLS 适配与 batch 后端。Linux 可用 `GOOS=windows GOARCH=amd64 CGO_ENABLED=0` 交叉构建 Windows 二进制。

生成发行包：

```sh
python3 veil-desktop/scripts/package.py --version 0.3.1
python3 veil-desktop/scripts/package.py --version 0.3.1 --target windows --arch amd64
```

输出位于 `veil-desktop/.build/releases`：Linux 为 tar.gz，Windows 为 ZIP。每个包附带 SHA-256 文件，包内 `manifest.json` 记录源码提交、工作区状态、Go 版本及文件摘要。Linux 包使用本机工具链和开发库构建。

## 使用

启动应用，导入服务端管理员提供的 JSON 配置，检查监听地址，校验并保存，再点击「连接」。在浏览器或其他应用中配置相应的本地代理地址。

「保存」保留当前运行连接；存在待生效更改时，「应用更改」会确认后重启代理。JSON 编辑区包含连接凭据，默认折叠。诊断区只显示运行状态与错误，不返回配置。

默认状态目录为 `os.UserConfigDir()/Veil/desktop`；可通过 `-state-dir PATH` 指定独立实例。Webview 的数据及缓存位于此目录下，与其他桌面应用分离。界面语言和外观会记忆选择；系统文件选择器等原生控件遵循系统语言。

窗口退出时若代理仍在运行，会确认断开连接。应用重新打开后可显式连接到已保存配置。
