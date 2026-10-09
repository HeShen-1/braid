---
name: braid 控制台
description: "深色仪器盘「控制室夜班」——近黑偏蓝绿的无光地面，层级只由亮度台阶与 1px 发丝线承担，色相只做状态"
colors:
  ground-0: "#0a0e13"
  ground-1: "#101720"
  ground-2: "#16202b"
  ground-3: "#1e2a37"
  ground-4: "#273543"
  line-0: "#1b2530"
  line-1: "#26333f"
  line-2: "#556a7e"
  ink-0: "#e8eff6"
  ink-1: "#a7b7c6"
  ink-2: "#8497ab"
  ink-3: "#76889b"
  sig-idle: "#3a4a5a"
  sig-run: "#2ed3c6"
  sig-ok: "#57d06a"
  sig-wait: "#f0b429"
  sig-fail: "#f4575e"
  sig-stop: "#8296ab"
  accent: "#6e9bf5"
  accent-soft: "#1b2a44"
typography:
  micro:
    fontSize: "0.656rem"
    lineHeight: "0.875rem"
    fontWeight: 600
    letterSpacing: "0.09em"
  xs:
    fontSize: "0.719rem"
    lineHeight: "1rem"
    fontWeight: 400
  sm:
    fontSize: "0.781rem"
    lineHeight: "1.125rem"
    fontWeight: 400
  base:
    fontSize: "0.844rem"
    lineHeight: "1.25rem"
    fontWeight: 400
  md:
    fontSize: "0.938rem"
    lineHeight: "1.375rem"
    fontWeight: 600
  lg:
    fontSize: "1.125rem"
    lineHeight: "1.625rem"
    fontWeight: 600
    letterSpacing: "-0.02em"
  xl:
    fontSize: "1.375rem"
    lineHeight: "1.75rem"
    fontWeight: 600
    letterSpacing: "-0.03em"
  "2xl":
    fontSize: "1.75rem"
    lineHeight: "2.125rem"
    fontWeight: 600
  mono:
    fontFamily: "Geist Mono, ui-monospace, SFMono-Regular, Menlo, Consolas, PingFang SC, Microsoft YaHei, monospace"
    fontSize: "0.94em"
    letterSpacing: "-0.01em"
    fontFeature: "'tnum' 1, 'zero' 1"
rounded:
  control: "8px"
  panel: "12px"
  pill: "999px"
  seg: "3px"
  branch-corner: "24px"
spacing:
  "0.5": "2px"
  "1": "4px"
  "1.5": "6px"
  "2": "8px"
  "3": "12px"
  "4": "16px"
  "6": "24px"
  "8": "32px"
components:
  button-primary:
    backgroundColor: "{colors.ink-0}"
    textColor: "{colors.ground-0}"
    rounded: "{rounded.control}"
    padding: "0 0.75rem"
    height: "2rem"
    typography: "{typography.sm}"
  button-primary-hover:
    backgroundColor: "#ffffff"
  button-primary-active:
    backgroundColor: "{colors.ink-1}"
  button-primary-disabled:
    backgroundColor: "{colors.ground-3}"
    textColor: "{colors.ink-3}"
  button-default:
    backgroundColor: "{colors.ground-2}"
    textColor: "{colors.ink-0}"
    rounded: "{rounded.control}"
    padding: "0 0.75rem"
    height: "2rem"
    typography: "{typography.sm}"
  button-default-hover:
    backgroundColor: "{colors.ground-3}"
  button-default-active:
    backgroundColor: "{colors.ground-4}"
  button-quiet:
    backgroundColor: "transparent"
    textColor: "{colors.ink-1}"
    rounded: "{rounded.control}"
    padding: "0 0.75rem"
    height: "2rem"
  button-danger:
    backgroundColor: "transparent"
    textColor: "{colors.sig-fail}"
    rounded: "{rounded.control}"
    padding: "0 0.75rem"
    height: "2rem"
  chip-status:
    rounded: "{rounded.pill}"
    padding: "0 0.45rem"
    height: "1.375rem"
    typography: "{typography.micro}"
  chip-running:
    backgroundColor: "color-mix(in oklab, #2ed3c6 12%, transparent)"
    textColor: "{colors.sig-run}"
  chip-succeeded:
    backgroundColor: "color-mix(in oklab, #57d06a 12%, transparent)"
    textColor: "{colors.sig-ok}"
  chip-waiting-approval:
    backgroundColor: "color-mix(in oklab, #f0b429 14%, transparent)"
    textColor: "{colors.sig-wait}"
  chip-failed:
    backgroundColor: "color-mix(in oklab, #f4575e 12%, transparent)"
    textColor: "{colors.sig-fail}"
  chip-quiet:
    backgroundColor: "{colors.ground-2}"
    textColor: "{colors.ink-1}"
    rounded: "{rounded.pill}"
    padding: "0 0.45rem"
    height: "1.375rem"
  input:
    backgroundColor: "{colors.ground-2}"
    textColor: "{colors.ink-0}"
    rounded: "{rounded.control}"
    padding: "0.3rem 0.5rem"
  panel:
    backgroundColor: "{colors.ground-1}"
    rounded: "{rounded.panel}"
    padding: "1rem"
  nav-tab:
    backgroundColor: "transparent"
    textColor: "{colors.ink-1}"
    padding: "0 0.75rem"
    height: "2.25rem"
  nav-tab-active:
    textColor: "{colors.ink-0}"
---

# Design System: braid 控制台

## Overview

**Creative North Star:「控制室夜班」(The Night-Shift Control Room)**

物理场景一句话:夜间机房里的控制台——近黑偏蓝绿的无光地面像一排尚未点亮的荧光屏,信息是被点亮的刻度与读数,不是浮在桌面上的卡片。

三个角色(author 编排、approver 审批、operator 值班)共用同一块 24–27 寸桌面屏幕,单次驻留 20 分钟到数小时,夜间值班是常态。所以这个世界没有装饰性亮度:每一级亮度差要么在回答"现在是什么状态、卡了多久",要么在说"这里可以动手"。密度按仪器读数组织——小字号、等宽数字、1px 发丝分割;留白只用来分组,不用来装饰。深色不是"科技感"品类默认,而是取自使用场景(长时间驻留 + 夜间低环境光)。

两条法则把色板的自由度收窄,反而让密集控制台可读:**色相只承担状态语义**(idle / run / ok / wait / fail / stop 六态),**交互一律靠亮度**(主操作 = 亮底深字,永不与状态色相竞争)。焦点蓝 `accent` 是唯一非状态色相,只做焦点与"当前所在",永不表达状态。状态一律色相 + 形状双编码,色盲可达。

本档记录的是 T21.1 已建成并经收尾截图复核的实现(`.impeccable/review/desktop-*.png` 与前端源码 token);构建路径为 code-led,**没有 approved comp、没有像素级 comp-diff**。不含 benchmark 数字、客户案例、定价或第三方 logo——产品真源 `PRODUCT.md` 已明确这些不存在,界面不得编造。

**Key Characteristics:**

- 四段亮度台阶(`ground-0`…`ground-4`)+ 三档发丝线(`line-0/1/2`)承担全部层级;**零发光、零投影**。
- 六态状态色相只做状态,每个状态同时有图标形状与中文标签。
- 自托管 Geist / Geist Mono 承担拉丁与数字面;CJK 由系统字体栈回落(用户拍板的有意取舍)。
- 一切 ID、计数、时长、时间戳走 tabular numerals;时间同时给相对与绝对两面。
- 签名动作 = **执行时间带**:审批挂起段用 135° 斜纹——等待是有长度、有纹理的时刻,不是转圈。

## Colors

无彩的地面 + 被严格看管的色相:一块尚未点亮的荧光屏上,只有状态被点亮,蓝色只表示"焦点在这里"。颜色键 = CSS 变量去掉 `--color-` 前缀(`ground-2` ↔ `var(--color-ground-2)`)。

### Primary

- **焦点蓝 Interaction Blue**(#6e9bf5):唯一的非状态色相。落点:焦点环(`:focus-visible` 2px outline、85% 混透明、offset 1px)、当前页签 2px 下划线、链接文字与 3px 下划线偏移、DAG 连线/把手选中态、框选边框、文本选区、caret 与 `accent-color`。
- **焦点蓝暗底 Focus Blue, Sunken**(#1b2a44):时间带选中行的底色(55% 混透明落在地面上)。

### Secondary(状态色相六态)

- **等待琥珀 Waiting Amber**(#f0b429):`WAITING_APPROVAL`。braid 的招牌状态,也是整屏唯一暖色——它必须在一屏冷色里被一眼找到。
- **执行青 Running Teal**(#2ed3c6):`RUNNING` / `EXECUTING`。
- **成功绿 Succeeded Green**(#57d06a):`SUCCEEDED`。
- **失败红 Failed Red**(#f4575e):`FAILED` / `ROLLED_BACK`,兼作破坏性动作与错误文案。
- **取消灰蓝 Stopped Slate**(#8296ab):`CANCELLED` / `SKIPPED`。
- **未开始暗板 Idle Slate**(#3a4a5a):六态里最暗的一档,只做底色与边框基;**永不做文字色**——它落在自身 tint 上只有 1.9:1,`IDLE`/`PENDING` 芯片的文字改用 `ink-2`。

### Neutral

- **无光地面 Unlit Ground**(#0a0e13):应用底、画布地面、JSON/DSL 只读底。
- **面板 Panel**(#101720)/ **抬升 Raised**(#16202b)/ **悬停 Hover**(#1e2a37)/ **选中·按下 Pressed**(#273543):四段亮度台阶,逐级抬升。ground-2 承载输入框、表头、次级按钮;ground-3 承载悬停与选中行;ground-4 承载按下与滚动条拇指。
- **结构分割 Structural Hairline**(#1b2530)/ **可见分隔 Visible Divider**(#26333f)/ **可辨描边 Discernible Stroke**(#556a7e):一律 1px。`line-2` 是交互控件描边与 DAG 连线的唯一来源。
- **主读数 Primary Reading**(#e8eff6)/ **次读数 Secondary**(#a7b7c6)/ **三级标签 Tertiary**(#8497ab)/ **待填 Quaternary**(#76889b):四级墨色,每一级按它真正落地的地面定档。

### Named Rules

**The Hue-Is-State Rule / 色相即状态.** 六态色相只允许出现在状态本身与由状态派生的图形上(状态芯片、时间带段、节点卡描边、告警边框、脉冲圆点)。悬停、按下、选中、禁用一律走亮度台阶与发丝线,永不借状态色相。

**The Focus-Hue Rule / 焦点色独占.** `accent` 蓝只做"焦点、当前所在、可交互线索";它一旦出现在状态芯片或时间带段上就是错误,反之状态色相也不得用来画焦点环。

**The Measured-Grade Rule / 定档规则.** 每档墨色与发丝线都按它真正落地的地面定档,不留"看起来更安静"的欠对比档。以下为收尾评审实测值;改任一 `ground`/`ink`/`line` 的值必须用同一方法重算这四个对,不得凭观感放行:

- `--color-ink-2` #8497ab → 表头文字落在地面 `ground-2` 上:**5.48:1**。
- `--color-ink-3` #76889b → 输入框占位符落在地面 `ground-2` 上:**4.52:1**。
- `--color-line-2` #556a7e → DAG 连线落在 `ground-0` 上:**3.46:1**;交互控件描边落在 `ground-1` 上:**3.22:1**(非文本图形 ≥3:1)。
- 状态芯片标签落在其自色 tint 上:**4.8–7.7:1**(最紧的一档是 FAILED;tint = 状态色 10–14% 的 `color-mix(in oklab)` 落在地面上)。

口径:不透明对按 WCAG 2.x 相对亮度公式(正文与占位文本 ≥4.5:1,非文本图形 ≥3:1);芯片 tint 是实际渲染色,故给区间不给单值。禁用态不参与定档,靠背景与 `cursor: not-allowed` 区分。

## Typography

**Display Font:** Geist(自托管可变字体 woff2,OFL 1.1,`font-weight: 100 900`,约 68KB);回落 `system-ui` / `-apple-system` / Segoe UI / PingFang SC / Microsoft YaHei / Noto Sans CJK SC
**Body Font:** 同上——这个世界只有一个拉丁面
**Label/Mono Font:** Geist Mono(自托管可变字体 woff2,OFL 1.1,约 70KB);回落 `ui-monospace` / SFMono-Regular / Menlo / Consolas / 系统中文字面

**Character:** 仪器读数。几何无衬线、字面开阔、没有一个装饰性字形;中文由系统面承担,靠字号阶与钉死的行高把两种字面焊进同一密度里。

### Hierarchy

- **micro**(10.5px / 14px / 600 / +0.09em / 大写):微标签——表头、字段名、分节标题、图例、芯片文字。字距随场景 0.06–0.09em(芯片 0.07em、中性芯片 0.06em、节点类型 0.08em),默认 0.09em。
- **xs**(11.5px / 16px / 400):时间带的节点标签与时间轴、审计行、字段提示。
- **sm**(12.5px / 18px / 400–500):密集正文——表格、按钮、输入框、面板正文;按钮 500,主操作 600。
- **base**(13.5px / 20px / 400):默认 UI 字号(`body` 上 13.5px、行高 1.5)。
- **md**(15px / 22px / 600):面板头标题。
- **lg**(18px / 26px / 600 / −0.02em):路由页标题。
- **xl**(22px / 28px / 600 / −0.03em):登录页产品名。
- **2xl**(28px / 34px / 600):尺度端点,当前没有落地用法(T21.1 无 28px 用例)。
- 标题统一 600、`letter-spacing: -0.015em`、`text-wrap: balance`;字距地板 −0.04em,本世界不越。
- **Measure**:`.measure` = `max-width: 72ch`,只给空态提示这类连续文本。

### Named Rules

**The Two-Face Rule / 两面分工.** 展示声音永远是自托管的 Geist;系统面只承担自托管文件里没有的 CJK 字形。CJK 策略是用户 2026-10-09 的拍板(D-2026-10-09-ui-console 决策 5),不是遗漏:中文字体动辄 3–10MB 不进包,`font-src 'self'` 是 CSP 的唯一放宽项。代价是中文灰度由操作系统决定(macOS PingFang SC 与 Windows 微软雅黑略有差异),这是接受的取舍——不要用"补一个中文字体"来"修复"它,那会推翻用户决策并把 dist 从 ~137KB 推到数 MB。

**The Tabular Rule / 数字必等宽.** ID、计数、时长、时间戳、表格数字一律 `font-variant-numeric: tabular-nums` + `font-feature-settings: 'tnum' 1, 'zero' 1`(`.num`、`td.num`、`time`、`code`、`kbd`、`samp` 全部继承)。时间是一等公民:相对时长(`3m12s 前`)与绝对时刻(`2026-10-09 09:29:39`)同时在位。

**The Mono-Is-Machine-Readable Rule / 等宽只给机读.** Geist Mono 只给代码、JSON/DSL、`node_id`/`trace_id` 这类机读标识。数字对齐用 tabular numerals,不用等宽字体假扮"技术感"。

## Layout

单 SPA、单套壳、单处登录。壳层只有一条顶栏,导航只有一处。

- **壳**:顶栏 52px(`h-13`),ground-1 底 + 1px `line-1` 下边;左起字标(17px 自绘 SVG + "braid")、域导航(工作流 / 执行 / 审批)、运行脉冲(执行中 / 待审批 / 失败三计数,`xl` 1280px 起显示)、账号与登出。页头 56px(`h-14`):h1 + 计数 + 动作,下边 `line-0`。
- **工作流面**:左列草稿列表 256px(`w-64`,右发丝线 `line-1`)+ 编辑器。编辑器横档 48px,工具条 72px(`4.5rem`,左发丝线),画布自适应,属性面板常驻 360px(`22.5rem`,左发丝线),DSL 抽屉折叠态 32px。
- **执行面**:两列网格 `minmax(20rem, 26rem) | minmax(0, 1fr)`;左轨内边距 12px + 右发丝线;右详情纵向分层:`gap 12px`、内边距 `12px 16px 32px`,超长内容滚动而不是被压扁。
- **审批面**(与执行面同构):两列网格 `minmax(22rem, 34rem) | minmax(0, 1fr)` —— 左列上限必须容得下队列表的「处理」动作列(收到 30rem 时它会落进无提示的横向滚动);左轨内边距 12px + 右发丝线;右详情纵向分层(meta / 审批说明 / 决定时间线 / 决定表单)。
  - 键盘:`j`/`k` 移动、`Enter` 打开、`a` 批准、`r` 驳回(布防一次 + 确认一次)、`Esc` 收起确认层或回队列;**输入框/文本域聚焦时整段让位,`ev.repeat`(长按 auto-repeat)一律忽略** —— 否则长按 `r` 等于布防即定局,而驳回落库不可改判。
  - 驳回确认层是**一次性浮起层**:布防即把焦点移进去,与执行面「取消执行」确认条同契约。
- **执行时间带**:行 = 两列网格 `minmax(12rem, 18rem) | minmax(0, 1fr)`;行高 ≥28px(`min-height: 1.75rem`);段上下各内缩 4.8px(`0.3rem`)、圆角 3px、亚像素段保底 2px;底部一条共享时间轴,5 刻度时另给 25/50/75 三条网格发丝线。
- **刻度密度双向自适应**:区间 <2 分钟或 ≥6 小时给 3 刻度(端 + 中),其间给 5 刻度;区间 <10 秒刻度补毫秒;跨天刻度带月-日;完整时刻始终进 `title`。
- **密度节奏**:4px 基步。组内 4/6/8px,组间 12/16px,区块之间 24px;分节内边距 `14px 16px`;表格单元格 `7.2px 12px`;空态 `40px 24px`。标题上方留白 ≥ 下方。
- **响应**:桌面优先。全站只有两处断点差异(`lg` 1024px 显示字标副题、`xl` 1280px 显示脉冲计数);不做移动端布局,窄窗只保证不横向溢出(仅收尾截图复核,未做逐宽度扫描)。
- 列宽稳定:属性面板常驻而不是按需插入;表格与列表用固定列宽 + `truncate`,数字列等宽对齐。

### Named Rules

**The No-Layout-Jump Rule / 布局不动.** 悬停、选中、加载、错误都不改变主要列宽与面板位置;控制台不该边看边晃。

**The Axis-Only-When-There-Is-Time Rule / 无时间不画轴.** 执行还没开始(没有任何 `started_at`)时,时间带只给空轨与状态芯片,不画坐标轴、不编时间;`PENDING` 节点不画段。

## Elevation & Depth

这个系统没有投影。前端源码里 `box-shadow` 出现 **0 次**,第三方 xyflow 自带的节点悬停/选中投影与控件条投影被显式置 `none`。深度由两样东西承担:**四段亮度台阶**(`ground-0`→`ground-4`)+ **1px 发丝线**。需要"更近"就抬亮度,需要"分开"就画线,没有第三种手段。

粘顶层(表头、面板头、列表头)用不透明地面色盖住滚动内容,而不是投影:`position: sticky` + `background: var(--color-ground-1)`。投影只留给真正浮起的层,而 T21.1 的现实是**没有任何一层用到投影**:唯一需要第二下确认的取消确认条是原地内联展开的,不是浮层。

浏览器的自带表面也属于这个世界,一并按调色板上色:文本选区(`accent` 38%)、caret(`accent`)、滚动条拇指(`ground-4`,悬停转 `line-2`,宽 10px、透明轨道)、焦点环(`accent` 85% 的 2px outline、offset 1px)、链接下划线偏移 3px、`color-scheme: dark`。

### Named Rules

**The Zero-Glow Rule / 零发光零投影.** 表面上不出现 `box-shadow`,也不出现发光(halo/glow/模糊光晕)。层级不够就调亮度台阶,分隔不够就加发丝线。

**The Sticky-Without-Shadow Rule / 贴边不投影.** 粘顶层靠不透明地面色遮挡滚动内容,不靠投影暗示层级。

## Shapes

圆角只有两档加三个特例:控件 **8px**(`--radius-control`)、面板 **12px**(`--radius-panel`);芯片 999px 胶囊、时间带段 3px、焦点环 4px。

- **分支节点**右上角 24px(`1.5rem`)折角——唯一为类型引入的轮廓差异,有几何语义(分叉)。
- **未配置节点**:虚线 1px `line-1` 边框(不是状态色),扫一眼就能找出还没填的节点。
- **选中节点**:亮度台阶 `ground-2` + 1px `outline`(`ink-2`,offset 2px)——不用彩色粗边。
- **图标**:统一 stroke 1.75、默认 14px(小件 11–13px),全部来自 lucide-react 的 React 内联 SVG(`<svg>` 元素而非图标字体或 `<img>`,同时满足 CSP `img-src 'self'` 与"不用 emoji 当图标");图标默认装饰性 `aria-hidden`,语义由相邻文字或 `aria-label` 承担。
- **字标**是自绘 SVG:三股正弦线(周期 1.5、振幅 4.6、18×18 viewBox),三股分别取 `ink-2` / `ink-3` / `accent`。
- **描边策略**:每个盒子只声明一次边界——要么 1px 边框,要么亮度台阶。

### Named Rules

**The One-Boundary Rule / 一处边界.** 面板、卡片、输入框只声明一次边界(边框或亮度台阶,二选一);"1px 边框 + 宽投影"的幽灵卡片在本世界不存在。

## Components

### Buttons

- **Shape**:高 32px(`2rem`);小号 26px(`1.625rem`)且字号降一档;圆角 8px;横向内边距 12px(小号 8px);图标与文字间距 6.4px(`0.4rem`);文字 12.5px/500,主操作 600;图标按钮见方(32 / 26px),必须给 `aria-label`。
- **Primary**:亮底深字——`background: var(--color-ink-0)`、文字 `ground-0`、边框同底色;悬停纯白 `#fff`,按下 `ink-1`;禁用 `ground-3` 底 + `ink-3` 字。这是"主操作 = 亮度最高的实体"的执行:全屏最亮的一块永远是当前该按的那个。
- **Default(次级)**:`ground-2` 底 + 1px `line-2` 边框 + `ink-0` 字;悬停 `ground-3` + 边框 `ink-3`;按下 `ground-4`;禁用 `ground-1` 底 + `line-0` 边框 + `ink-3` 字 + `cursor: not-allowed`。
- **Quiet(幽灵/图标)**:透明底、`ink-1` 字;悬停 `ground-3` + `ink-0`;小号常与图标组合(新建、添加、删除行)。
- **Danger(破坏性)**:透明底 + `color-mix(fail 55%)` 边框 + `fail` 字;悬停 14% 红 tint + 实边框 + 提亮字 `#ff8a8f`。不可逆动作(取消执行)走 danger 并给第二下确认。
- **Hover / Focus / Motion**:背景色、边框色、文字色 120ms `cubic-bezier(0.4, 0, 0.2, 1)`;焦点环统一走全局 `:focus-visible`。

### Chips(状态芯片)

- **Shape**:高 22px(`1.375rem`)、胶囊圆角、内边距 `0 7.2px`;标签 10.5px/600/大写/+0.07em。
- **Color**:1px 边框取状态色相 40–65% 混透明,底色取同色相 10–14% 混透明,文字取状态色相本体;例外是 `IDLE`/`PENDING`——文字 `ink-2`、边框 `line-2`、底取 `sig-idle` 22%。
- **Dual coding**:芯片内必带状态图标(11px)+ 中文标签。词表与 DDL ENUM 一一对应:执行域 `PENDING` 待执行 / `RUNNING` 执行中 / `WAITING_APPROVAL` 等待审批 / `SUCCEEDED` 已成功 / `FAILED` 已失败 / `CANCELLED` 已取消;节点域另有 `EXECUTING` 执行中 / `SKIPPED` 已跳过。不新增、不改名、不换话术;未知状态回 `CircleHelp` + 原样英文码。
- **Quiet variant**:中性元数据(节点类型、触发来源、草稿/已发布/已归档)用 `chip-quiet`——`ground-2` 底、`line-1` 边框、`ink-1` 字、+0.06em,不承载状态。

### Inputs / Fields

- **Style**:`ground-2` 底、1px `line-2` 边框、8px 圆角、内边距 `4.8px 8px`、12.5px 字;标签永远在控件上方(11.5px `ink-1`),错误紧贴控件下方并优先于提示(11.5px `fail`,`role="alert"` + `aria-invalid`)。
- **Hover**:边框 `ink-3`。**Focus**:边框 `accent` + 底色降回 `ground-1` + 2px `accent` 70% 焦点环。**Disabled**:`ground-1` 底 + `line-0` 边框 + `ink-3` 字。**Invalid**:边框 `sig-fail`。
- **Placeholder**:`ink-3`(实测 4.52:1)。`select` 用双 5×5 线性渐变自绘箭头(`ink-2`),`textarea` 允许纵向 resize、行高 1.55。数值输入带 `min`/`max` 镜像 schema,越界就地报错。

### Panels

- 12px 圆角、`ground-1` 底、1px `line-1` 边框。头部 ≥44px、内边距 `8px 16px`、底部发丝线 `line-0`,标题 15px/600、副文 11.5px `ink-2` 等宽数字;正文内边距 16px。
- **分节不用盒子**:1px `line-0` 上边线 + 10.5px 微标题(`.panel-section`,内边距 `14px 16px`)。禁卡片套卡片。

### Tables

12.5px、`border-collapse: separate` + `border-spacing: 0`。表头粘顶:`ground-2` 底、`line-1` 下边、10.5px/600/大写 `ink-2`(实测 5.48:1);单元格内边距 `7.2px 12px`、下边 `line-0`;行悬停 `ground-3` 60% 混透明;选中行 `ground-3` + `ink-0`;末行去线;数字列 tabular-nums。

### Navigation

域导航就是页签:高 36px、12.5px/500、`ink-1`;悬停 `ink-0`;当前页 `ink-0` + 600 + 底部 2px `accent` 下划线(左右内缩 8px)+ 14px 图标。全站只有这一处导航:没有第二套导航、没有二级侧边导航、没有面包屑。

### Alerts / Empty / Skeleton

- **alert**:12px 圆角、`ground-2` 底、`line-1` 边框、12.5px `ink-1`;`data-tone="error"` 换 `fail` 45% 边框 + 10% 底 + `ink-0` 字;`warn` 换 `wait` 45% 边框 + 10% 底。
- **空态**:居中,内边距 `40px 24px`;22px `ink-3` 图标 + 15px/600 标题 + 12.5px 提示(≤72ch),必要时给一个动作——空态回答"下一步做什么",不是感叹号。
- **骨架**:`ground-2`↔`ground-3` 的 1.4s 横向扫掠、圆角 4px;`prefers-reduced-motion: reduce` 下全站动效压到 0.01ms。

### Execution Ribbon(签名动作)

- **每节点一行**:左标签列 = 状态芯片 + `node_id`(mono)+ 类型芯片 + 实例号 + 尝试次数(`试 2/3`)+ 时长;右轨道按**真实时间**线性铺开;底部一条共享时间轴。
- **段**:实心状态色块(执行青 / 成功绿 / 失败红 / 取消灰蓝);`PENDING` 不画段。终态执行的右边界冻结在最后一次结束时刻(否则 `now` 会把历史执行压成左侧一条缝);进行中的右边界 = `now`,每秒推进;`now` 落在区间内部时另画一根 55% `ink-0` 的 1px 现在线。
- **挂起段**:135° 斜纹(琥珀 3px 实 + 7px 22% 琥珀底),与节点卡的挂起顶边同一约定。
- **行选中**:`accent-soft` 55% 混透明的行底;标签列是真正的按钮(键盘可达、`aria-pressed`),点击行任意位置等效——选中的就是回滚目标。
- **可读性**:段悬停给完整绝对起止与时长(`title` 与 `aria-label` 同一文本);图例复用同一套 `[data-state]` 配色,不建第二真源。
- **挂起读数**:面板头在 `waitMs > 0` 时给出「挂起 15h41m · 未占 worker」——这是本产品最该被看见的数字。

**为什么它不是进度条。** 进度条编码的是"完成了几分之几",但 DSL 里没有"完成百分比"这个量——节点只有状态机的迁移,画进度就是编数据。时间带编码的是另一件事:每节点在**同一条真实时间轴**上占多长。于是一次 15h41m 的审批挂起会在 28s、32s、44s 的节点旁边摊开成整幅宽的斜纹带,长度差本身就是"资源没被占用,但时间在走"这句话;进度条或进度环没有长度可比,只能转圈——那正是这类产品表达等待的默认做法,也正是要拒绝的那个。斜纹是工程图/Gantt 的数据图形约定(在图上量时间),不是装饰性背景纹理。

### Canvas Node Card

- 12px 圆角、`ground-1` 底、1px `line-2` 边框、12.5px 字;悬停 `ground-2` + 边框 `ink-3`。
- **类型 = 形状**:头部类型图标 + 大写微标签;`branch` 另给右上折角。
- **状态 = 色相 + 形状**:仅当执行面注入 `status` 时出现——状态色相 55–65% 的 1px 边框 + 状态芯片;挂起节点另加 3px 斜纹顶边。编辑器不虚构执行状态。
- **未配置 = 亮度/发丝线**:虚线 `line-1` 边框 + `ink-3` 待填占位,不占状态色相。

### Named Rules

**The Hatch-Means-Waiting Rule / 斜纹只有一个含义.** 135° 斜纹 = 等待在走、资源没占。它只允许出现在时间/度量图形里(执行时间带、节点卡顶边),不许当面板或空态的装饰背景。

**The No-Invented-State Rule / 不发明状态.** 状态色相只在有真实状态注入时出现;编辑面在没有 `status` 时不显示任何状态色,状态词表只认与 DDL ENUM 对应的那些值。

**The One-Authored-Moment Rule / 只编排一次入场.** 全站唯一被编排过的入场是时间带段的展开(320ms `cubic-bezier(0.16, 1, 0.3, 1)`,`scaleX 0.35→1` + 透明度);其余动效只有 120ms 的状态过渡与 1.4s 的骨架扫掠。不做逐区块入场。时长目前是实现里的字面量,尚未抽成 token。

## Do's and Don'ts

### Do:

- **Do** 先问"现在是什么状态、卡了多久、下一步谁动",再决定画什么。
- **Do** 用亮度表达交互:主操作 = 亮底深字(`ink-0` 底 + `ground-0` 字),悬停/按下/选中走 `ground-2→ground-4`。
- **Do** 每个状态同时给色相与形状(11px 图标 + 中文标签),`WAITING_APPROVAL` 给最暖的琥珀。
- **Do** 让等待有长度:挂起段按真实时间铺开并给「挂起 X · 未占 worker」。
- **Do** 所有数字等宽(tabular numerals),时间同时给相对与绝对两面。
- **Do** 守住两档圆角(8px 控件 / 12px 面板)与 4px 基步节奏;表格行 `7.2px 12px`、面板正文 16px。
- **Do** 把浏览器自带表面主题化:选区、caret、滚动条、焦点环、下划线偏移、`color-scheme: dark`。
- **Do** 用发丝线分组(结构 `line-0` / 分隔 `line-1` / 交互描边 `line-2`),分节用"发丝线 + 微标题"。
- **Do** 图标统一 1.75 笔画、默认 14px、`aria-hidden`,语义交给相邻文字或 `aria-label`。
- **Do** 尊重 `prefers-reduced-motion`,并且贴边层用不透明地面色而不是投影。

### Don't:

- **Don't** 用投影或发光。本世界零 `box-shadow`;投影只留给真正浮起的层,而 T21.1 没有任何一层用到。
- **Don't** 把状态色相当交互色或装饰色,也别让 `accent` 蓝表达状态(色相只做状态,交互靠亮度)。
- **Don't** 只靠色相编码状态——色盲可达是产品硬约束,色相必须配形状。
- **Don't** 卡片套卡片:面板内用发丝线 + 微标题分节,不做描边小卡片;也不做同尺寸"图标 + 标题 + 文字"卡片阵列当页面结构。
- **Don't** 用彩色 `border-left`/`border-right`(>1px)当选中态或强调;选中 = 亮度台阶 + 1px 偏移环。
- **Don't** 用渐变文字或玻璃拟态/装饰性模糊;强调靠字重与字号。
- **Don't** 拿 emoji 或 Unicode 字形当图标;图标一律是 lucide-react 的内联 SVG。
- **Don't** 把等宽字体当"技术感"装饰(只给代码、机读标识、JSON/DSL)。
- **Don't** 用系统字体当拉丁展示声音;也不要为了"统一"把 CJK 塞进自托管字体(推翻用户决策,且体积不划算)。
- **Don't** 发明 DSL 里没有的执行状态:编辑面在没有 `status` 注入时不显示状态色,状态词表不扩写。
- **Don't** 在时间数据不存在时画时间:无 `started_at` 不画坐标轴,`PENDING` 节点不画段。
- **Don't** 用进度条、进度环或 sparkline 表达执行进度——DSL 里没有"完成百分比",时间带的长度就是进度信息。
- **Don't** 给标题加 kicker/eyebrow;微标签只做字段名、列名、分节名,不许出现在标题上方。
- **Don't** 做移动端布局或第二主题(浅色主题不做):桌面优先、单世界,窄窗只保证不横向溢出。
