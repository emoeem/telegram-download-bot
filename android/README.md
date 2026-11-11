# TG 归档 · Android 客户端

把 [tdl](https://github.com/iyear/tdl) 的归档核心整个编进一个 APK。装上就能用，不需要服务器、不需要 Docker、不需要电脑。

## 架构

```
┌──────────────────────────────────────────────┐
│ Android App (Java)                           │
│                                              │
│  MainActivity ── WebView ──► 127.0.0.1:PORT  │
│       │                                      │
│       └─► ArchiveService (前台服务, 常驻通知)  │
│                 │                            │
│                 └─► libgojni.so              │
│                       ├── gotd/td   MTProto  │
│                       ├── modernc   SQLite   │
│                       ├── 媒体下载 worker      │
│                       └── net/http  回环服务   │
└──────────────────────────────────────────────┘
```

关键点：**Go 核心是同一份代码**，通过 `gomobile bind` 编成 `.so`，不是子进程、不是 Termux、不是远程网站套壳。

前端也是同一份：`archive/web/index.html` 原样由 Go 的 embed 提供，WebView 加载本机回环地址。

## 为什么这样设计

| 决策 | 理由 |
|---|---|
| 前台服务承载核心 | 归档必须 7×24 保持 MTProto 长连接。Activity 会被销毁，普通后台线程会被系统冻结 |
| HTTP 绑 `127.0.0.1:0` | 端口由内核分配，局域网内其他设备扫不到，同机其他 App 也猜不到 |
| WebView 而非原生 UI | 现有 92KB 前端（含移动断点、底部导航、刘海适配）一行不改全部复用 |
| 数据在 `filesDir` | 里面是 Telegram 会话密钥，绝不能放外部存储 |
| 只出 `arm64-v8a` | Go 核心按 ABI 各编一份，多 ABI 只是让包更大 |

## 构建

前置：Go 1.25+、JDK 17+、Android SDK（platform 35、build-tools 35）、NDK 27.2。

```bash
# 1) 编 Go 核心 -> AAR
export ANDROID_HOME=/path/to/android-sdk
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/27.2.12479018
export GOWORK=off

go get -tool golang.org/x/mobile/cmd/gobind   # 只需一次

gomobile bind \
  -target=android/arm64 \
  -androidapi 24 \
  -javapkg=com.tdl.archive \
  -ldflags="-s -w" \
  -o android/app/libs/tdl-bridge.aar \
  ./mobile

# 2) 编 APK
cd android
./gradlew assembleRelease     # 或 assembleDebug
```

产物：`android/app/build/outputs/apk/release/app-release.apk`

构建期可注入共享凭据（默认留空，由用户首次启动时填）：

```bash
./gradlew assembleRelease \
  -Ptdl.appId=12345678 \
  -Ptdl.appHash=0123456789abcdef0123456789abcdef
```

需要在 `app/build.gradle` 的 `buildConfigField` 里接这两个属性。当前构建内置了 `2040` / `b18441a1ff607e10a989891a5462e627`（Telegram Desktop 与上游 tdl 长期使用的公开应用凭据），因此安装后不需要填任何东西。

## 首次使用

1. 安装 APK，打开
2. 直接进入主界面 → 账号面板 → 扫码登录（**不再有凭据填写步骤**）
3. 通知栏出现「归档服务运行中」即表示核心已接管

**内置凭据的取舍**：`api_id` 是按应用发放的，不是按用户，所以把它编进包里不涉及泄露任何人的账号。代价是同一对凭据被所有安装复用，聚合用量大时可能被限流。为此保留了一条逃生口：**通知栏那条常驻通知上有「更换 API 凭据」按钮**，点进去可以保存自己申请的一对——用户保存的值优先于内置值，「沿用内置凭据」则清掉保存值回到默认。

首屏配置页在这种情况下不会自动出现（内置值让 `hasCredentials()` 恒为真），只能从通知栏进入。这是刻意的：绝大多数人不需要看到它。

改了凭据后核心会重启（`applyCredentials()`），因为 Go 侧在构造客户端时就解析了 `api_id`，只刷新页面会继续沿用旧的应用身份。

## 已知限制

| 限制 | 说明 |
|---|---|
| **仅 Android** | iOS 不给长时间后台 TCP，做不到"一直在归档"，只能做"打开 App 才补收" |
| **未在真机验证** | 本轮环境无设备、无模拟器。已静态验证到 JNI 符号层（见下），运行时行为需要真机确认 |
| **约 81 MB** | Go runtime + 全部依赖 + 前端资源。单 ABI 已是最小形态 |
| 存储只增不减 | 与 CLI 版本一致：目前没有自动清理，手机空间需要手动管理 |
| 换机要迁移 | 数据只在这台设备上，`filesDir/tdl` 需要手工导出 |

## 已验证 / 未验证

**已验证（有输出证据）**

- `modernc.org/sqlite` 等纯 Go 依赖能编进 Android AAR（最小样例 6.6 MB，16.8 秒）
- 主项目完整依赖树能 bind（`gotd/td` + `bbolt` + `sqlite`），AAR 24 MB
- 生成的 Java API 签名正确：`Bridge.start(long, String, long)` 等
- `.so` 导出全部 JNI 符号：`Java_com_tdl_archive_mobile_Bridge_start` 等 11 个
- APK 构建通过，`aapt2 dump badging` 显示 package/minSdk/targetSdk/权限全部正确
- dex 中包含 `MainActivity` / `ArchiveService` / `CoreHolder` / `Prefs` / `Bridge`

**未验证**

- 真机安装与启动
- 前台服务在厂商 ROM（小米/华为/OPPO）上的存活率
- 扫码登录在 WebView 里的完整链路
- 实际归档吞吐与耗电

## 目录

```
android/
├── app/
│   ├── build.gradle                        # AGP 8.7.3, minSdk 24, arm64 only
│   ├── libs/tdl-bridge.aar                 # gomobile 产物，需先按上文生成
│   └── src/main/
│       ├── AndroidManifest.xml
│       ├── java/com/tdl/archive/app/
│       │   ├── MainActivity.java           # 凭据引导 + WebView
│       │   ├── ArchiveService.java         # 前台服务，承载 Go 核心
│       │   ├── CoreHolder.java             # 进程内单例，发布核心状态
│       │   └── Prefs.java                  # api_id/api_hash 持久化
│       └── res/
└── ../mobile/bridge.go                     # Go 侧导出层（gomobile 入口）
```
