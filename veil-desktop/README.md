# Veil Desktop

Windows / Linux 桌面端使用 Wails v2 与 Go 公共控制层。界面直接调用 `control.Manager`，数据由同一进程中的 Veil 核心转发。

- 导入 JSON 配置、编辑监听地址及 SOCKS5 / HTTP / mixed 入口。
- 配置校验、私有保存、启停、确认后应用更改。
- 运行状态、连接计数及诊断信息。
- 简体中文 / English、浅色 / 深色外观。
- 独占实例目录，退出关闭代理，重新打开保留配置。

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

## 使用

启动应用，导入服务端管理员提供的 JSON 配置，检查监听地址，校验并保存，再点击「连接」。在浏览器或其他应用中配置相应的本地代理地址。

「保存」保留当前运行连接；存在待生效更改时，「应用更改」会确认后重启代理。JSON 编辑区包含连接凭据，默认折叠。诊断区只显示运行状态与错误，不返回配置。

默认状态目录为 `os.UserConfigDir()/Veil/desktop`；可通过 `-state-dir PATH` 指定独立实例。Webview 的数据及缓存位于此目录下，与其他桌面应用分离。界面语言和外观会记忆选择；系统文件选择器等原生控件遵循系统语言。

窗口退出时若代理仍在运行，会确认断开连接。应用重新打开后可显式连接到已保存配置。
