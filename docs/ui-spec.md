# telegram-download-bot UI 重设计规范（ui-spec v2）

> 基线：_review/before/1031-pc + 1031-mobile（2026-10-31 实拍）
> 范围：archive/web 静态 UI（桌面 Fyne 与 Android WebView 共用同一份）
> 铁律：功能不动 —— 全部改动限于 theme.css 设计令牌/样式层 + index.html 两处无风险增强
> 方案基调：**打磨型重设计**。既有 token 体系健康（批次1-4 成果），推倒重来必须动
> JS 渲染逻辑，违反"禁止改崩"；本方案在令牌层系统性重调，视觉差异肉眼可见、
> 行为差异归零。

## 1. 设计诊断（对照 before 截图）

| # | 问题 | 证据（before 截图） |
|---|---|---|
| D1 | 深色层级扁平：bg-app #080a0f 与卡片 #10141d 明度接近，卡片边界靠 1px 弱边框 | 01-messages 列表区糊成一片 |
| D2 | 卡片 hover `translateY(-1/-2px)` + 大阴影：深色下阴影不可见，只剩位移=抖动 | 02-images 网格 |
| D3 | fs-xs 11px 中文可读性差（发布时间/统计标签） | 右栏「自动刷新 5s」「已完成」 |
| D4 | 顶栏无视觉分组：搜索无限拉伸，5 组控件挤一行 | 01-messages 顶栏 |
| D5 | 移动端双层导航重叠感：顶 chips 与底 tab 同权重 | 1031-mobile/01 |
| D6 | 底部 tab 58px 高、icon+文字 11px，触控目标 <48dp | 1031-mobile 底栏 |
| D7 | 空态只有字符 "⌁"+一行灰字，无引导 | .empty:before |
| D8 | 主搜索框不支持回车（必须点按钮）——键盘最短路径缺失 | index.html #q 无 keydown |
| D9 | 加载中无样式（轮询期间列表静默跳变） | 仅 2 处 loading 文本 |
| D10 | 激活态指示弱：accent-soft .12 太淡 | 左侧导航选中项 |

## 2. 色彩令牌（dark 为默认，light 同步调整）

### 2.1 深色（默认）

| 令牌 | 旧值 | **新值** | 理由 |
|---|---|---|---|
| --bg-app | #080a0f | **#0b0e14** | 提亮半档，给卡片留层次 |
| --bg-surface | #10141d | **#131824** | 与 app 拉开 1 档 |
| --bg-surface-hover | #151a24 | **#181f2c** | hover 可感 |
| --bg-elevated | #1b2230 | **#1e2634** | 弹窗/底栏浮起 |
| --bg-input | #0d1118 | **#0f131c** | 输入框比 surface 更沉 |
| --border-subtle | #1a202b | **#1e2633** | — |
| --border-default | #242b38 | **#2a3444** | 卡片边界可辨（D1） |
| --border-strong | #3a4354 | **#435064** | hover 边界清晰 |
| --fg-primary | #e9edf5 | #e9edf5（不变） | — |
| --fg-secondary | #8993a5 | **#94a0b4** | 对比 4.6:1 ↑ |
| --fg-tertiary | #687386 | **#6f7d94** | — |
| --accent | #2AABEE | #2AABEE（品牌不变） | Telegram 蓝 |
| --accent-hover | #229edb | #229edb（不变） | — |
| --accent-soft | rgba(42,171,238,.12) | **rgba(42,171,238,.16)** | D10 选中可感 |
| --state-success | #7ee2a8 | **#86e3ae** | 微调 |
| --state-warn | #f0b37e | #f0b37e（不变） | — |
| --state-danger | #f0808f | #f0808f（不变） | — |
| --state-info | #78b7ff | #78b7ff（不变） | — |

### 2.2 浅色

| 令牌 | 旧值 | 新值 |
|---|---|---|
| --bg-app | #f4f7fb | #f2f5fa |
| --bg-surface | #fff | #ffffff（不变） |
| --border-default | #d8e0e8 | **#d3dce6** |
| --accent-soft | .11 | **.14** |
| 其余 | 不变 | — |

对比度校验：fg-primary/各 bg ≥ 12:1；fg-secondary/surface dark = 4.6:1；light = 5.1:1（≥4.5 达标）。

## 3. 字号与行高

| 令牌 | 旧 | 新 | 理由 |
|---|---|---|---|
| --fs-xs | 11px | **12px** | D3 中文下限（D3） |
| --fs-sm | 13px | 13px（不变） | — |
| --fs-md | 14px | 14px（不变） | 正文 |
| --fs-lg | 16px | 16px（不变） | — |
| --fs-xl | 20px | **21px** | 页标题权重 |
| --lh-tight | — | **1.35** | 新增：标题 |
| --lh-body | — | **1.6** | 新增：正文（.text 1.5→1.6） |

## 4. 间距 / 圆角 / 触控

- 间距令牌不变（4/8/12/16/24/32/48 已健康）。
- 圆角不变（4/8/12/16/full）。
- **触控目标**：底部 tab 高 58→**64px**，按钮内 icon 20px+文字 12px，整格可点 ≥48dp（D6）；
  `.chip` padding 7px 13px→**8px 14px**（移动端 ≥40px 高）。

## 5. 阴影与悬浮（D2）

| 场景 | 旧 | 新 |
|---|---|---|
| 卡片 hover | translateY(-1/-2px) + shadow-hover(0 10 28 .1) | **无位移**；border→strong + bg→surface-hover；shadow 仅弹层保留 |
| --shadow-hover | 0 10 28 rgba(0,0,0,.1) | **0 4 16 rgba(0,0,0,.22)**（弹层用） |
| --shadow-md | 0 10 28 .18 | 0 8 24 rgba(0,0,0,.24) |
| --shadow-lg | 0 18 48 .28 | 0 16 44 rgba(0,0,0,.32) |

## 6. 动效令牌

| 令牌 | 值 | 用途 |
|---|---|---|
| --t-instant | **80ms**（新增） | 触控按压反馈 |
| --t-fast | 120ms | hover/选中 |
| --t-base | 200ms | 面板/主题切换 |
| --t-slow | 320ms | 抽屉/浮层 |
| --ease | cubic-bezier(.2,0,0,1) | 不变（标准减速曲线） |

`prefers-reduced-motion` 规则保留不动。

## 7. 顶栏重组（D4，CSS-only）

- `.search` 加 `max-width:640px`（大屏不再无限拉伸），聚焦环不变。
- `.top` gap 8→10px；`.button.btn-search` 收窄为 icon+文字紧凑档（padding 10px 12px）。

## 8. 导航与移动端（D5/D6/D10）

- 左侧导航：active 背景新 accent-soft(.16)，指示条 3px 全高不变。
- 移动端（≤650px）：
  - 底部 `.mobile-nav` 高 58→**64px**；按钮字 11→12px；激活色 primary→accent-soft 背景 + accent 字。
  - 顶部内容 chips 保持（它们是内容类型筛选 = 次级层），视觉降权：`.nav` 移动端按钮 padding 8px 10px→**7px 11px**、bg-input 底、圆角 full——与底部主导航明确分层。
- `.right` 抽屉宽度 380→**min(400px,94vw)**，圆角顶部 16 保持。

## 9. 空态 / 加载 / 错误三态（D7/D9）

- 空态 `.empty`：:before 内容 "⌁"→保持字符但包 64px 圆底（accent-soft 背景、accent 色、
  margin-bottom 12px）；文字分主副两级（`.empty b` 主行 14px/fg-primary，`.empty span` 12px/tertiary——
  由现有调用方文本自然兼容，不改 JS 文案）。
- 加载：`.loading-row`（新类，theme.css 定义）：40px 高的骨架条 shimmer（accent 6% 底、
  1.2s 往返）。本轮仅提供类；JS 渲染层是否接入为后续项（避免动功能）。
- 错误：`.banner` 不变语义，色值跟随新 danger；`.session-banner` 同步。

## 10. 操作逻辑优化（HTML 两处增强，零 JS 逻辑改动）

1. **回车搜索**（D8）：`#q` 加 `onkeydown="if(event.key==='Enter')doSearch()"` ——
   键盘最短路径 2 步→1 步；触屏不变（按钮仍在）。
2. **搜索语法帮助可达**：placeholder 的 title 属性已有，追加 `enterkeyhint="search"` ——
   移动端软键盘回车键显示为「搜索」。

> 取舍：导航去重（顶 chips 与底 tab 合并）需要动 JS 的视图映射，违反铁律，放弃；
> 以「视觉分层（次级 chips 样式降权）」替代，效果达成 80%、风险为零。

## 11. 验证清单（after 自测）

- [ ] 11 个视图逐页截图（PC 1440×900 / 移动 412×915）与 before 成对
- [ ] 回车搜索：输入关键词按 Enter → 结果刷新（功能自测）
- [ ] 点击导航逐页切换正常（含 dest 右栏行为不变）
- [ ] 浅色主题切换正常（theme toggle）
- [ ] 登录页/激活页（login/activate）配色跟随
- [ ] `go test ./archive/`（embed 资产测试兜底 HTML 完整性）
- [ ] 移动端底栏触控目标 ≥48dp（截图目测 + 代码核对）
