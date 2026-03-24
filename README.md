# hanime-hunter

`hanime-hunter` 是一个面向 `hanime1.me` 与 `hanime.tv` 的下载工具，当前提供两种使用方式：

- 原生 Windows GUI：适合日常使用，支持预览、缩略图、任务队列、进度条与设置面板
- CLI 命令行：适合脚本化、批量下载与调试

当前项目以原生 GUI 为主，不再推荐使用旧版 WebUI。

## 功能特性

- 支持 `hanime1.me`、`hanime.tv`
- 支持单集下载、整季/全集下载、播放列表下载
- 支持下载前预览视频信息与缩略图
- 支持质量选择：`自动选择`、`1080p`、`720p`、`480p`、`360p`、`240p`
- 支持重试次数设置
- 支持下载线程数设置，范围 `1-64`
- 支持 GUI 任务并发设置，范围 `1-16`
- 支持系统代理与环境变量代理
- 支持结构化日志、失败重试、任务取消、清理已完成任务
- 支持响应式 GUI 布局、自动适应屏幕尺寸
- 支持 GUI 默认设置持久化保存

## 支持站点

> NSFW 警告：以下站点可能包含成人内容。

| 站点 | 单集 | 整季/全集 | 播放列表 | 说明 |
| --- | --- | --- | --- | --- |
| `hanime1.me` | 支持 | 支持 | 支持 | 中文站点 |
| `hanime.tv` | 支持 | 支持 | 支持 | 英文站点 |

## 运行环境

### GUI

- Windows 10 / 11
- 建议安装 `FFmpeg`

### CLI / 源码构建

- Go `1.21` 或更高版本
- 建议安装 `FFmpeg`

## 快速开始

### 直接使用已编译程序

如果你已经拿到了打包目录，优先使用：

- GUI：`bulid/package/hanime-hunter-gui.exe`
- CLI：`bulid/package/hanime-hunter.exe`

如果你在项目根目录直接使用，也可以运行：

- GUI：`hanime-hunter-gui.exe`
- CLI：`hanime-hunter.exe`

### 从源码编译

在项目根目录执行：

```powershell
go build -o .\hanime-hunter.exe .
go build -ldflags='-H windowsgui' -o .\hanime-hunter-gui.exe .
```

### 打包目录说明

当前项目约定了一个 `bulid` 目录用于整理源码和成品：

- `bulid/source`：源码副本
- `bulid/package`：可执行文件与说明文档

## GUI 使用说明

### 启动 GUI

推荐直接双击：

```text
hanime-hunter-gui.exe
```

也可以通过命令行启动：

```powershell
.\hanime-hunter.exe gui
```

### 基本下载流程

1. 打开 GUI
2. 粘贴 `hanime1.me` 或 `hanime.tv` 链接
3. 点击 `预览信息`
4. 选择输出目录、质量、重试次数、线程数
5. 点击 `开始下载`
6. 在右侧查看 `预览 / 详情 / 日志 / 设置`

### GUI 支持的主要能力

- 视频缩略图预览
- 任务进度条与实时速度显示
- 失败任务重试
- 取消选中任务
- 自动打开输出目录
- 自动预览链接
- 下载失败自动切换到日志页
- 默认下载参数保存
- 多任务并发下载控制

### 设置面板说明

GUI 右侧 `设置` 标签页支持保存以下默认值：

- 默认输出目录
- 默认工作目录
- 默认质量
- 默认重试次数
- 默认线程数
- 默认超时秒数
- 默认日志级别
- 默认仅获取信息
- 默认整季/全集
- 默认最低质量
- 任务并发上限 `1-16`
- 粘贴链接后自动预览
- 下载完成后自动打开输出目录
- 新任务加入后自动选中
- 任务失败时自动切换到日志页

GUI 设置文件默认保存在：

```text
%AppData%\hanime-hunter\gui-settings.json
```

## CLI 使用说明

### 查看帮助

```powershell
.\hanime-hunter.exe -h
.\hanime-hunter.exe dl -h
.\hanime-hunter.exe gui -h
```

### 常用命令

下载单个视频：

```powershell
.\hanime-hunter.exe dl "https://hanime1.me/watch?v=94898"
```

下载整季 / 全集：

```powershell
.\hanime-hunter.exe dl "https://hanime1.me/watch?v=94898" --series
```

只获取视频信息，不实际下载：

```powershell
.\hanime-hunter.exe dl "https://hanime1.me/watch?v=94898" --info
```

指定输出目录、质量、线程数与重试次数：

```powershell
.\hanime-hunter.exe dl "https://hanime1.me/watch?v=94898" `
  --output-dir "D:\Downloads\HAnime" `
  --quality 720p `
  --threads 32 `
  --retry 10
```

启动 GUI 并指定任务并发上限：

```powershell
.\hanime-hunter.exe gui --max-concurrent 4
```

### CLI 参数说明

#### `dl`

- `--output-dir`：输出目录
- `--quality`：指定质量，例如 `1080p`、`720p`
- `--info`：仅获取信息
- `--low-quality`：下载最低清晰度
- `--retry`：重试次数，默认 `10`
- `--series`：下载整季 / 全集
- `--threads`：下载线程数，范围 `1-64`

#### 全局参数

- `--log-level`：日志级别，可选 `debug`、`info`、`warn`、`error`、`fatal`

## 代理说明

项目默认支持以下代理来源：

- 环境变量代理，例如 `HTTP_PROXY`、`HTTPS_PROXY`
- 系统代理设置

也就是说，如果你的系统已经配置了代理，程序会优先复用，不需要额外再开独立代理。

## FFmpeg 说明

部分视频流在下载或合并时依赖 `FFmpeg`。如果你遇到下载后无法正确合并、转封装或播放异常，优先确认：

```powershell
ffmpeg -version
```

如果命令不存在，请先安装 `FFmpeg` 并加入系统 `PATH`。

## 常见问题

### 1. 双击 GUI 没有反应

建议按这个顺序检查：

1. 使用最新的 `hanime-hunter-gui.exe`
2. 查看任务管理器里是否已有旧的 `hanime-hunter-gui.exe` 进程
3. 结束旧进程后重新双击启动
4. 如果程序弹出错误框，按错误内容排查依赖或权限问题

### 2. 命令行中文乱码

Windows 终端建议切到 UTF-8：

```powershell
chcp 65001
```

### 3. 下载失败、超时、EOF 或站点响应慢

这类问题通常来自目标站点波动或网络链路不稳定，可以尝试：

- 增加 `--retry`
- 将线程数调整到 `16`、`32` 或 `64`
- GUI 中把超时设置为 `0`，表示不限制
- 检查系统代理是否可用

### 4. 预览有文字但没有缩略图

部分页面不一定提供可用封面，这种情况下文字预览仍然可用，不影响下载。

### 5. GUI 下载时卡顿

当前版本已经对高频日志刷新与进度更新做了节流处理。如果仍感觉界面卡顿，优先降低：

- 同时运行任务数
- 单任务线程数

## 项目结构

```text
hanime-hunter/
├─ cmd/                 # CLI 与 GUI 命令入口
├─ internal/            # 下载器、解析器、请求逻辑
├─ pkg/                 # 通用工具
├─ docs/                # 文档资源
├─ bulid/source/        # 源码副本
├─ bulid/package/       # 打包输出
├─ main.go              # 主入口
├─ README.md            # 主文档
└─ README_ZH_CN.md      # 中文文档入口说明
```

## 开发说明

### 本地测试

```powershell
go test ./...
```

### 本地构建

```powershell
go build ./...
```

## 许可证

项目使用 [LICENSE](./LICENSE) 中提供的许可证。
