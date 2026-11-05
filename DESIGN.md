---
name: braid 控制台
description: "B 风格布局 v2：柔和近黑地面、青绿动作、可折叠导航与按需展开的工作区"
colors:
  # 以下为深色(默认)世界的值；浅色档见 Colors 节(同一批键，另一套值)
  ground-0: "#161719"
  ground-1: "#1c1d20"
  ground-2: "#232529"
  ground-3: "#2b2e32"
  ground-4: "#34383d"
  line-0: "#303338"
  line-1: "#41454b"
  line-2: "#72767d"
  ink-0: "#eeeff1"
  ink-1: "#b2b5bc"
  ink-2: "#a8adb6"
  ink-3: "#a0a5ae"
  sig-idle: "#3a4a5a"
  sig-run: "#2ed3c6"
  sig-ok: "#57d06a"
  sig-wait: "#f0b429"
  sig-fail: "#ff747b"
  sig-fail-hi: "#ff8a8f"
  sig-stop: "#8296ab"
  accent: "#2ed3b2"
  accent-soft: "#173c35"
  primary-action: "#2ed3b2"
  primary-ink: "#161719"
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
    fontSize: "0.813rem"
    lineHeight: "1.125rem"
    fontWeight: 400
  base:
    fontSize: "0.875rem"
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
    backgroundColor: "{colors.primary-action}"
    textColor: "{colors.primary-ink}"
    rounded: "{rounded.control}"
    padding: "0 0.75rem"
    height: "2rem"
    typography: "{typography.sm}"
  button-primary-hover:
    backgroundColor: "color-mix(in oklab, var(--color-primary-action) 88%, var(--color-primary-ink))"
  button-primary-active:
    backgroundColor: "color-mix(in oklab, var(--color-primary-action) 76%, var(--color-primary-ink))"
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
    backgroundColor: "color-mix(in oklab, var(--color-sig-run) 12%, var(--color-ground-1))"
    textColor: "{colors.sig-run}"
  chip-succeeded:
    backgroundColor: "color-mix(in oklab, var(--color-sig-ok) 12%, var(--color-ground-1))"
    textColor: "{colors.sig-ok}"
  chip-waiting-approval:
    backgroundColor: "color-mix(in oklab, var(--color-sig-wait) 14%, var(--color-ground-1))"
    textColor: "{colors.sig-wait}"
  chip-failed:
    backgroundColor: "color-mix(in oklab, var(--color-sig-fail) 12%, var(--color-ground-1))"
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
    textColor: "{colors.ink-2}"
    padding: "0 0.75rem"
    height: "2.375rem"
  nav-tab-active:
    backgroundColor: "{colors.accent-soft}"
    textColor: "{colors.accent}"
---

# Design System: braid 控制台

## Overview

**Creative North Star: 分面取长，运营面安静而紧凑**

2026-11-05 用户批准的 B 风格布局 v2 延续密集、可读的控制台，改用更柔和的近黑地面与青绿动作。主导航、工作流列表、属性栏、执行列表、证据区与审批处理栏按各自任务收展，让正在阅读或编辑的内容取得主要空间。用户明确选择的主题与折叠偏好优先。

三个角色（author 编排、approver 审批、operator 值班）共用桌面工作区，长时间驻留与夜间值班要求清楚的文字层级、细线分组、可见焦点与状态双编码。状态色、动作色和节点类型色分别有语义，不能仅凭相近色相判断业务状态。

本档由当前 `web/canvas/src/styles/` 与相关 TSX 提取。视觉依据是 `.impeccable/mocks/20261105/layout-v2/` 的 `workflow.png`、`execution.png`、`approval-content.png`、`approval-history.png`；图中的样例文字、数据和流程不构成业务契约。这里记录已实现结构，不代表前后端联调、浏览器验收或用户最终验收已通过。旧 UX-F1-a / T21.3 测量仅属于原版本，不能证明本次 token 与布局。

**Key Characteristics:**

- 五级近黑地面与三档细线承担层级，不使用装饰性光晕。
- 青绿标识主动作、焦点与当前所在；状态继续配中文标签与图标形状。
- 工作流以宽画布为主，添加节点与属性按需展开。
- 执行详情上方是定义版本图，下方是可收起的证据标签区。
- 审批内容与审批记录同页互斥切换，处理栏可以收起。
- 自托管 Geist / Geist Mono、系统 CJK 回退与等宽数字保持既有阅读习惯。

## Colors

中性近黑地面配青绿交互，状态保持独立语义。默认深色值以 frontmatter 为准，对应 `styles/index.css` 的 `@theme`；浅色沿用同名 token 覆盖，值与元数据同步在 sidecar。

### Primary

- **动作青绿**（`accent` / `primary-action`）：深色主操作、焦点环、链接、当前导航与局部标签。`accent-soft` 是当前导航与选中时间带的低亮底。
- **主操作文字**（`primary-ink`）：与实体按钮配对。浅色仍使用深实体浅字；浅色 `accent` 为深青绿，不把浅色主题当第二套布局。

### Secondary

- **状态六态**（`sig-idle/run/ok/wait/fail/stop`）：未开始、执行中、成功、等待审批、失败、取消或跳过。状态芯片与时间带消费真实状态；`sig-fail-hi` 用于破坏性动作悬停。
- 未开始状态的文字使用 `ink-2`，不直接用暗的 `sig-idle`。等待使用琥珀及斜纹；失败使用红色及文字/图标。
- 编辑画布的类型色用于图标、类型识别，不能推断执行结果；深色节点主体为中性色，浅色仍保留已有类型底色。

### Neutral

- `ground-0` 是应用与画布地面，`ground-1` 是导航与面板；`ground-2/3/4` 依次承担输入/次级控件、悬停、按下或选中。
- `ink-0/1/2/3` 分别承担主文本、次文本、标签提示与占位。三级及占位文字已随新地面调整。
- `line-0/1/2` 分别承担结构分割、可见分隔及交互控件/连线描边；结构细线与可操作边界分开判定。

### Named Rules

**The Semantic-Color Rule / 色彩分工.** 青绿交互 token 表达可操作与当前所在；`sig-*` 表达真实状态；画布类型色表达节点类型。即使色相相近，也必须用标签、图标与上下文区分。

**The One-Line-Two-Worlds Rule / 同一语义 token，两套主题.** 颜色先在默认深色 `@theme` 定义，浅色只覆盖同名 token；组件不另建主题色板。无明确偏好时所有路由默认深色，用户选择 auto / light / dark 后全局沿用。

**The Measured-Grade Rule / 定档规则.** 正文与占位文本要求至少 4.5:1，非文本必要图形至少 3:1；按实际最不利地面、状态底与焦点合成测量。token 对与真实浏览器分别验证；本次尚不能引用旧版本测量宣称通过。

**The Chip-Background-Is-Opaque Rule / 芯片底色不透明.** 状态芯片用同色相混合不透明 `ground-1` 作底，防止标签对比度随所在行变化。

## Typography

**Display Font:** Geist(自托管可变字体 woff2,OFL 1.1,`font-weight: 100 900`,约 68KB);回落 `system-ui` / `-apple-system` / Segoe UI / PingFang SC / Microsoft YaHei / Noto Sans CJK SC
**Body Font:** 同上——这个世界只有一个拉丁面
**Label/Mono Font:** Geist Mono(自托管可变字体 woff2,OFL 1.1,约 70KB);回落 `ui-monospace` / SFMono-Regular / Menlo / Consolas / 系统中文字面

**Character:** 仪器读数。几何无衬线、字面开阔、没有一个装饰性字形;中文由系统面承担,靠字号阶与钉死的行高把两种字面焊进同一密度里。

### Hierarchy

- **micro**(10.5px / 14px / 600 / +0.09em / 大写):微标签——表头、字段名、分节标题、图例、芯片文字。字距随场景 0.06–0.09em(芯片 0.07em、中性芯片 0.06em、节点类型 0.08em),默认 0.09em。
- **xs**(11.5px / 16px / 400):时间带的节点标签与时间轴、审计行、字段提示。
- **sm**(13px / 18px / 400–500):密集正文——表格、按钮、输入框、面板正文;按钮 500,主操作 600。
- **base**(14px / 20px / 400):默认 UI 字号(`body` 上 14px、行高 1.5)。
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

单 SPA、单套壳、单处登录。B v2 以可收起的左主导航加右侧工作区组织三个业务面。

- **壳层**：主导航展开宽 12rem；折叠后由顶栏「展开侧栏」恢复。顶栏包含路由标题、上下文、运行脉冲、主题与账号；品牌区与路由头最小高度 3.25rem。当前主域使用青绿文字与低亮底。
- **工作流**：无已有偏好时，已有活动草稿的工作流列表收起，无活动草稿时展开；右侧属性栏默认收起；展开后的属性栏宽 22.5rem。添加节点位于画布左上角的 disclosure 工具中；选中或添加节点可打开属性。DSL 保留底部可展开区域，工具不占常驻竖栏。
- **执行**：未选记录时显示总览与列表；详情态可收起 16rem 上下文列表，主体按「执行身份 → 处置区 → 定义版本图 → 证据标签区」排列。处置区保留回滚目标、先停止未结束节点（quiesce）、回滚/续跑/取消及原权限与确认流程，独立于证据标签和证据区折叠。版本图默认展开，高度 `clamp(18rem, 38vh, 30rem)`。下方节点详情、时间线、输入与输出、审计记录互斥显示；证据区整体可收起，选择标签重新展开。
- **审批**：队列与详情保持现有同页导航；详情头之下分为主阅读区和 19rem 处理栏。「审批内容 / 审批记录」切换阅读内容；处理栏单独收起。驳回确认激活时保持处理栏可见，防止确认过程被隐藏。
- **执行时间带**：节点标签与真实时间轨道并排，共享时间轴；等待斜纹与真实时长保持原契约，不将长度解释为完成百分比。
- **密度**：4px 基步，组内 4/6/8px，组间 12/16px，区块间 24px。主要阅读区横向内边距 1.5rem，使用细线分节而不是反复嵌套卡片。
- **窄窗适配**：仍以桌面为目标。900px 以下审批处理栏转到阅读区下方、属性栏限宽；760px 以下工作流和执行详情改纵向排列、顶栏换行、长标识可换行，表格与标签在自身区域滚动。主导航无偏好时按初始窗口小于 900px 默认收起。具体窗口表现仍需浏览器验证。

### Named Rules

**The Deliberate-Disclosure Rule / 主动收展.** 布局变化来自明确的展开、收起或选择动作；悬停、加载与错误不应无故挤动画布或阅读区。所有被收起的区域都有可见恢复入口。

**The Axis-Only-When-There-Is-Time Rule / 无时间不画轴.** 执行未开始且没有 `started_at` 时只给空轨与状态，不编时间；`PENDING` 不画时间段。

## Elevation & Depth

这个系统没有投影。第三方 xyflow 自带的节点悬停/选中投影与控件条投影被显式置 `none`。深度由两样东西承担:**五级亮度台阶**(`ground-0`→`ground-4`)+ **1px 发丝线**。需要"更近"就抬亮度,需要"分开"就画线,没有第三种手段。

粘顶层(表头、面板头、列表头)用不透明地面色盖住滚动内容,而不是投影:`position: sticky` + `background: var(--color-ground-1)`。投影只留给真正浮起的层,而 T21.1 的现实是**没有任何一层用到投影**:唯一需要第二下确认的取消确认条是原地内联展开的,不是浮层。

浏览器的自带表面也属于这个世界,一并按调色板上色:文本选区(`accent` 38%)、caret(`accent`)、滚动条拇指(`ground-4`,悬停转 `line-2`,宽 10px、透明轨道)、焦点环(`accent` 85% 的 2px outline、offset 1px)、链接下划线偏移 3px、`color-scheme: dark`。

### Named Rules

**The Zero-Glow Rule / 零发光零投影.** 表面上不出现 `box-shadow`,也不出现发光(halo/glow/模糊光晕)。层级不够就调亮度台阶,分隔不够就加发丝线。

**The Sticky-Without-Shadow Rule / 贴边不投影.** 粘顶层靠不透明地面色遮挡滚动内容,不靠投影暗示层级。

## Shapes

圆角只有两档加三个特例:控件 **8px**(`--radius-control`)、面板 **12px**(`--radius-panel`);芯片 999px 胶囊、时间带段 3px、焦点环 4px。

- **编辑节点**统一 8px 圆角；分支类型通过图标与文字识别，不再以编辑卡片折角承担类型差异。
- **未配置节点**:虚线 1px `line-1` 边框(不是状态色),扫一眼就能找出还没填的节点。
- **选中节点**:亮度台阶 `ground-2` + 1px `outline`(`ink-2`,offset 2px)——不用彩色粗边。
- **图标**:统一 stroke 1.75、默认 14px(小件 11–13px),全部来自 lucide-react 的 React 内联 SVG(`<svg>` 元素而非图标字体或 `<img>`,同时满足 CSP `img-src 'self'` 与"不用 emoji 当图标");图标默认装饰性 `aria-hidden`,语义由相邻文字或 `aria-label` 承担。
- **字标**是自绘 SVG:三股正弦线(周期 1.5、振幅 4.6、18×18 viewBox),三股分别取 `ink-2` / `ink-3` / `accent`。
- **描边策略**:每个盒子只声明一次边界——要么 1px 边框,要么亮度台阶。

### Named Rules

**The One-Boundary Rule / 一处边界.** 面板、卡片、输入框只声明一次边界(边框或亮度台阶,二选一);"1px 边框 + 宽投影"的幽灵卡片在本世界不存在。

## Components

### Buttons

- **Shape**:高 32px(`2rem`);小号 26px(`1.625rem`)且字号降一档;圆角 8px;横向内边距 12px(小号 8px);图标与文字间距 6.4px(`0.4rem`);文字 13px/500,主操作 600;图标按钮见方(32 / 26px),必须给 `aria-label`。
- **Primary**：深色青绿实体配深字，浅色深实体配浅字；使用 `primary-action` / `primary-ink`。悬停和按下分别混合 88% / 76% 主操作色，禁用使用 `ground-3` 底与 `ink-3` 字。当前配色的对比度需独立验证，不能沿用旧近白按钮的数值。
- **Default(次级)**:`ground-2` 底 + 1px `line-2` 边框 + `ink-0` 字;悬停 `ground-3` + 边框 `ink-3`;按下 `ground-4`;禁用 `ground-1` 底 + `line-0` 边框 + `ink-3` 字 + `cursor: not-allowed`。
- **Quiet(幽灵/图标)**:透明底、`ink-1` 字;悬停 `ground-3` + `ink-0`;小号常与图标组合(新建、添加、删除行)。
- **Danger(破坏性)**:透明底 + `color-mix(fail 55%)` 边框 + `fail` 字;悬停 14% 红 tint + 实边框 + 提亮字 `sig-fail-hi`(深色 `#ff8a8f` / 浅色 `#a03339`)。不可逆动作(取消执行)走 danger 并给第二下确认。
- **Hover / Focus / Motion**:背景色、边框色、文字色 120ms `cubic-bezier(0.4, 0, 0.2, 1)`;焦点环统一走全局 `:focus-visible`。

### Chips(状态芯片)

- **Shape**:高 22px(`1.375rem`)、胶囊圆角、内边距 `0 7.2px`;标签 10.5px/600/大写/+0.07em。
- **Color**:1px 边框取状态色相 40–65% 混透明,**底色取同色相 10–14% 不透明盖 `ground-1`**(见 The Chip-Background-Is-Opaque Rule),文字取状态色相本体;例外是 `IDLE`/`PENDING`——文字 `ink-2`、边框 `line-2`、底取 `sig-idle` 22% 盖 `ground-1`。
- **Dual coding**:芯片内必带状态图标(11px)+ 中文标签。词表与 DDL ENUM 一一对应:执行域 `PENDING` 待执行 / `RUNNING` 执行中 / `WAITING_APPROVAL` 等待审批 / `SUCCEEDED` 已成功 / `FAILED` 已失败 / `CANCELLED` 已取消;节点域另有 `EXECUTING` 执行中 / `SKIPPED` 已跳过。不新增、不改名、不换话术;未知状态回 `CircleHelp` + 原样英文码。
- **Quiet variant**:中性元数据(节点类型、触发来源、草稿/已发布/已归档)用 `chip-quiet`——`ground-2` 底、`line-1` 边框、`ink-1` 字、+0.06em,不承载状态。

### Inputs / Fields

- **Style**:`ground-2` 底、1px `line-2` 边框、8px 圆角、内边距 `4.8px 8px`、13px 字;标签永远在控件上方(11.5px `ink-1`),错误紧贴控件下方并优先于提示(11.5px `fail`,`role="alert"` + `aria-invalid`)。
- **Hover**:边框 `ink-3`。**Focus**:边框 `accent` + 底色降回 `ground-1` + 2px `accent` 85% 焦点环。**Disabled**:`ground-1` 底 + `line-0` 边框 + `ink-3` 字。**Invalid**:边框 `sig-fail`。
- **Placeholder**:`ink-3`。`textarea` 允许纵向 resize、行高 1.55;数值输入带 `min`/`max` 镜像 schema,越界就地报错。
- **Select / Checkbox(T21.3 单点收敛)**:外壳仍是自研(`ui/field.tsx` 的标签/hint/错误布局与 id 绑定,调用点继续写原生 `<option>`),内部控制与键盘行为交给 shadcn 的 `select`/`checkbox`(Base UI):焦点管理、弹层 Portal、列表键盘导航、`role="checkbox"`。外观由一层适配 CSS 收回自研词汇——`[data-slot='select-trigger'].select` 与 `.input` 同形(同高、同底、同描边、同焦点环),弹层 = `ground-1` + 1px `line-1` 且**无投影**;勾选框未选 = `ground-2` + `line-2`,选中 = `accent` 实体 + 对侧地面色勾。全仓只有这一处实现,不存在两套。

### Panels

- 12px 圆角、`ground-1` 底、1px `line-1` 边框。头部 ≥44px、内边距 `8px 16px`、底部发丝线 `line-0`,标题 15px/600、副文 11.5px `ink-2` 等宽数字;正文内边距 16px。
- **分节不用盒子**:1px `line-0` 上边线 + 10.5px 微标题(`.panel-section`,内边距 `14px 16px`)。禁卡片套卡片。

### Tables

13px、`border-collapse: separate` + `border-spacing: 0`。表头粘顶:`ground-2` 底、`line-1` 下边、10.5px/600/大写 `ink-2`;单元格内边距 `7.2px 12px`、下边 `line-0`;行悬停 `ground-3` 60% 混透明;选中行 `ground-3` + `ink-0`;末行去线;数字列 tabular-nums。

### Navigation

主域导航在左侧，工作流、执行记录与审批待办各有图标及文字；链接以 `aria-current="page"` 表示当前域。侧栏内链接高 2.375rem，当前域使用 `accent` / `accent-soft`，悬停使用中性地面。顶栏保留唯一可见路由 `h1`、当前上下文、主题与账号；主导航收起后恢复键仍可达；收起时焦点交给「展开侧栏」，展开时焦点交给侧栏内收起按钮。

运行脉冲展示真实租户计数，未知与读取失败不画成零；窄窗隐藏脉冲与部分次要身份信息。主域导航和详情标签分工明确，避免把内容切换误认为页面跳转。

### Section Tabs

同页标签高至少 2.75rem，当前标签使用青绿文字和 2px 下划线。`tablist` / `tab` / `tabpanel` 成对绑定；左右方向键循环切换，Home / End 到首尾，只有当前标签进入 Tab 顺序。非当前内容使用 `hidden`，证据或处理栏的控制键通过 `aria-expanded` / `aria-controls` 说明收展对象。

### Approval Queue and Detail

队列保持无外框的行式表面、六列表格与细线分隔，工作流/版本、执行/节点和截止时间依真实数据展示；逾期同时显示文字。`j` / `k` 移动及 Enter 打开仍属于队列操作。

详情头保留身份与元信息，下面通过「审批内容 / 审批记录」互斥切换；记录使用表格呈现已读到的决定、处理人、备注与时间，空记录明确留空态。正文说明保留换行、行高 1.8；既有执行上下文与说明按细线分节。右侧处理栏保留备注、批准、驳回及原防重/校验/确认行为，收起处理栏不改变审批权限与后端契约。处理栏已收起时按 r 布防驳回会显示确认区；Esc 取消后恢复收起状态，焦点回「展开处理栏」。

共享 shadcn 适配继续使用语义 token，菜单焦点底保持中性地面。全局及 Select 等控件保留可见焦点环；键盘回归和真实浏览器证据需在本次布局上验证，不能由文档同步代替。

### Theme Control(主题三态分段)

- 壳顶栏右侧的三颗等宽图标按钮(`auto` 跟随系统 / `light` / `dark`):容器高 26px、`ground-2` 底 + 1px `line-0` + 8px 圆角、内边距 2px;按钮 24×22px、未选中 `ink-2`、悬停 `ground-3`、选中 `ground-4` + `ink-0`。
- 语义是**设定偏好**而不是开关:当前档用 `aria-pressed`,`title` 与 `sr-only` 给中文名(仅图标必须有无障碍名);三颗等宽,切换时右侧不位移(布局不动)。
- 落点只有 `<html data-theme>` 一个属性;偏好存 `localStorage`(`braid.theme`),首绘前由 `assets/theme-boot.js`(同源外部脚本——CSP 是 `script-src 'self'`,内联脚本在生产不执行)落定,浅色用户不会先看到一闪的深色。

### Collapsible Panels

- 主导航、工作流列表、属性栏、执行上下文列表、执行证据区和审批处理栏分别保存收展偏好。工作流列表收起后用画布内恢复键，属性栏开关保留在编辑器顶栏；没有常驻的空白侧轨。
- 偏好使用 `localStorage`（`src/prefs.ts`），与主题偏好一样允许存储失败后在本次会话继续使用。认证 token 仍仅在 `sessionStorage`，两种载体分开。
- 主题默认深色并尊重已有 auto / light / dark；工作流列表无偏好时按是否已有活动草稿决定收起，属性栏默认收起。添加节点的 disclosure 与详情标签属于当前交互状态，不宣称跨会话保存。

### Alerts / Empty / Skeleton

- **alert**:12px 圆角、`ground-2` 底、`line-1` 边框、13px `ink-1`;`data-tone="error"` 换 `fail` 45% 边框 + 10% 底 + `ink-0` 字;`warn` 换 `wait` 45% 边框 + 10% 底。
- **空态**:居中,内边距 `40px 24px`;22px `ink-3` 图标 + 15px/600 标题 + 13px 提示(≤72ch),必要时给一个动作——空态回答"下一步做什么",不是感叹号。
- **骨架**:`ground-2`↔`ground-3` 的 1.4s 横向扫掠、圆角 4px;`prefers-reduced-motion: reduce` 下全站动效压到 0.01ms。

### Execution Overview(UX-F1-c)

- 未选择执行时，总览位于执行页上方；选择后进入定义版本图与证据标签的上下分区，左侧保留可收起的紧凑上下文列表。Vercel 参照落在 Geist 大数字、相邻的 1px 读数格和真实创建分布，不引入新色板、图表依赖或装饰曲线。
- 四个读数是**当前租户、全部工作流、当前保留执行**的总数与 `RUNNING` / `WAITING_APPROVAL` / `FAILED` 计数，均消费既有列表 API 的 `page.total`；与列表的名称、状态和分页筛选无关，也不是当前账号的待办数。四次查询分别读取，不承诺原子同刻。
- 创建分布仅使用 `limit=100, offset=0` 的最近样本中真实 `created_at`。明示实际样本条数、有效/缺失时间、UTC 样本起止、等宽桶宽与「条/桶」；按 UTC epoch 对齐的桶含起点、不含终点，完整桶表可键盘展开。它描述本次样本，不是固定 24 小时时间窗，也不从轮询快照推导状态历史。
- 初载显示未知；零条响应才显示 0 与空样本。缺总数不回退到 `items.length`，缺失/无效时间不计入图；不足两个不同有效时间时说明原因并不绘图。每 5 秒自动刷新、操作后刷新及手动刷新共用在途去重；单项失败保留其最近成功值/样本并标明失败与旧读取时间，其他成功项仍更新。
- 仅复用现有地面、文字与状态令牌；计数用 tabular numerals，状态标签复用图标与中文。图形用 `ink-1`、坐标线用 `line-2`，总览按钮与桶表入口焦点为实体 2px `accent` 内环。验收范围为桌面 1440 与窄窗 1024 的双主题；静态 token 或专项测试不替代真实浏览器验收。

### Execution Ribbon(签名动作)

- **每节点一行**:左标签列 = 状态芯片 + `node_id`(mono)+ 类型芯片 + 实例号 + 尝试次数(`试 2/3`)+ 时长;右轨道按**真实时间**线性铺开;底部一条共享时间轴。
- **段**:实心状态色块(执行青 / 成功绿 / 失败红 / 取消灰蓝);`PENDING` 不画段。终态执行的右边界冻结在最后一次结束时刻(否则 `now` 会把历史执行压成左侧一条缝);进行中的右边界 = `now`,每秒推进;`now` 落在区间内部时另画一根 55% `ink-0` 的 1px 现在线。
- **挂起段**:135° 斜纹(琥珀 3px 实 + 7px 22% 琥珀底),与节点卡的挂起顶边同一约定。
- **行选中**:`accent-soft` 55% 混透明的行底;标签列是真正的按钮(键盘可达、`aria-pressed`),点击行任意位置等效——选中的就是回滚目标。
- **可读性**:段悬停给完整绝对起止与时长(`title` 与 `aria-label` 同一文本);图例复用同一套 `[data-state]` 配色,不建第二真源。
- **挂起读数**:面板头在 `waitMs > 0` 时给出「挂起 15h41m · 未占 worker」——这是本产品最该被看见的数字。

**为什么它不是进度条。** 进度条编码的是"完成了几分之几",但 DSL 里没有"完成百分比"这个量——节点只有状态机的迁移,画进度就是编数据。时间带编码的是另一件事:每节点在**同一条真实时间轴**上占多长。于是一次 15h41m 的审批挂起会在 28s、32s、44s 的节点旁边摊开成整幅宽的斜纹带,长度差本身就是"资源没被占用,但时间在走"这句话;进度条或进度环没有长度可比,只能转圈——那正是这类产品表达等待的默认做法,也正是要拒绝的那个。斜纹是工程图/Gantt 的数据图形约定(在图上量时间),不是装饰性背景纹理。

### Canvas Node Card

- 编辑卡片统一 8px 圆角、1px 中性描边；默认深色节点使用中性不透明底，图标以小面积颜色区分 LLM、工具、分支、审批和并行，配合类型文字。浅色保留现有类型底/字色。
- 只有真实执行状态注入时才展示状态芯片与状态描边；等待节点仍有斜纹顶边。编辑器不虚构执行状态。
- 未配置节点使用虚线及待填占位，选中使用细 outline；工具提示、把手与方向信息继续承担编辑语义。

### Named Rules

**The Hatch-Means-Waiting Rule / 斜纹只有一个含义.** 135° 斜纹 = 等待在走、资源没占。它只允许出现在时间/度量图形里(执行时间带、节点卡顶边),不许当面板或空态的装饰背景。

**The No-Invented-State Rule / 不发明状态.** 状态色相只在有真实状态注入时出现;编辑面在没有 `status` 时不显示任何状态色,状态词表只认与 DDL ENUM 对应的那些值。

**The One-Authored-Moment Rule / 只编排一次入场.** 全站唯一被编排过的入场是时间带段的展开(320ms `cubic-bezier(0.16, 1, 0.3, 1)`,`scaleX 0.35→1` + 透明度);其余动效只有 120ms 的状态过渡与 1.4s 的骨架扫掠。不做逐区块入场。时长目前是实现里的字面量,尚未抽成 token。

## Do's and Don'ts

### Do:

- **Do** 先问"现在是什么状态、卡了多久、下一步谁动",再决定画什么。
- **Do** 用语义 token 表达交互：深色主动作使用青绿实体，普通悬停/按下使用中性地面，保留清楚的焦点与选中标识。
- **Do** 每个状态同时给色相与形状(11px 图标 + 中文标签),`WAITING_APPROVAL` 给最暖的琥珀。
- **Do** 让等待有长度:挂起段按真实时间铺开并给「挂起 X · 未占 worker」。
- **Do** 所有数字等宽(tabular numerals),时间同时给相对与绝对两面。
- **Do** 守住两档圆角(8px 控件 / 12px 面板)与 4px 基步节奏;表格行 `7.2px 12px`、面板正文 16px。
- **Do** 把浏览器自带表面主题化:选区、caret、滚动条、焦点环、下划线偏移、`color-scheme: dark`。
- **Do** 用发丝线分组(结构 `line-0` / 分隔 `line-1` / 交互描边 `line-2`),分节用"发丝线 + 微标题"。
- **Do** 图标统一 1.75 笔画、默认 14px、`aria-hidden`,语义交给相邻文字或 `aria-label`。
- **Do** 尊重 `prefers-reduced-motion`,并且贴边层用不透明地面色而不是投影。
- **Do** 主题只落 `data-theme` 一个属性:组件不读主题、不分支,切主题 = 换一套 token 值;偏好走 `localStorage`,首绘前由同源外部启动脚本落定。
- **Do** 把跨会话 UI 偏好(主题三态、区域折叠)放 `localStorage`,认证 token 留 `sessionStorage`——两种载体分离,互不引用。
- **Do** 状态芯片底色不透明:标签对比度不能随所在行/悬停状态变化。

### Don't:

- **Don't** 用投影或发光。本世界零 `box-shadow`;投影只留给真正浮起的层,而 T21.1 没有任何一层用到。
- **Don't** 把青绿动作、执行状态和画布类型混为同一语义；状态必须来自真实数据并配文字与形状。
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
- **Don't** 把浅色做成第二套视觉语言:浅色是同一世界的日间形态(同一批 token 键、同一套类词汇、同一批色相),不是新主题;全部路由无偏好时默认深色，用户明确选择后全局沿用。
- **Don't** 在组件层引第二套语言:shadcn 只到 8 个复合交互组件,基础原语与 Select/Checkbox 外壳保持自研;registry 自带的外观 utility 必须被适配层收回(`styles/index.css` 末节),不许两套并存。
- **Don't** 用半透明底承载状态标签:对比度会随所在行漂移(T21.3 实测曾掉到 3.2:1)。
- **Don't** 做移动端布局:桌面优先,窄窗只保证不横向溢出。
