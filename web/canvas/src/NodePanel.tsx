// 节点属性面板:五类节点全覆盖。编辑 = immutable patch 回写 spec
// (上层 updateSpec 替换 data.spec 引用触发 RF 重渲);词域约束
// (mode/ops/on_timeout)与数值边界以镜像枚举呈现,权威校验仍在服务端。
//
// 布局纪律:标签在上、控件在下(纵向扫读);分节 = 发丝线 + 微标题,
// 不做描边小卡片(禁卡片套卡片);本地可判的错误紧贴控件出现
// (重复审批人 / 数值越界),其余错误等保存后在 issues 区回显。
import { useState, type Ref } from 'react'
import { Trash2 } from 'lucide-react'
import { BRANCH_OPS, type DslNode } from './model/dsl.ts'
import {
  AreaField,
  Group,
  KVRows,
  ListRows,
  NumField,
  Section,
  SelectField,
  TextField,
  parseLiteral,
} from './fields.tsx'
import { nodeTypeMeta } from './ui/status.ts'
import { Button } from './ui/primitives.tsx'
import { I } from './ui/I.tsx'
import { TID } from './ui/testids.ts'

const OPS = BRANCH_OPS as readonly string[]

/**
 * ApproversInput:本地原文态输入。join/split 规范化直接做受控值会在
 * 打字中途吞分隔符(输入 "alice," → 规范化回显 "alice"),原文暂存、
 * 值向外提交,回显不被规范化打断。
 *
 * 就近错误:schema 对 approvers 有 uniqueItems(t14.2,重复名单会让
 * 会签判据永久挂起),重复项在这里当场指出——不等到发布才红。
 */
function ApproversInput({ spec, update }: Sec<Extract<DslNode, { type: 'approval' }>>) {
  const [raw, setRaw] = useState<string | null>(null)
  const commit = (v: string) => {
    setRaw(v)
    update((s) =>
      s.type === 'approval' ? { ...s, approvers: v.split(',').map((x) => x.trim()).filter((x) => x !== '') } : s,
    )
  }
  const dupes = [...new Set(spec.approvers.filter((a, i) => spec.approvers.indexOf(a) !== i))]
  return (
    <TextField
      label="审批人(逗号分隔)"
      value={raw ?? spec.approvers.join(', ')}
      placeholder="alice, bob"
      error={dupes.length > 0 ? `重复的审批人：${dupes.join('、')}（schema 要求名单唯一）` : undefined}
      hint={spec.approvers.length === 0 ? '名单为空；发布校验要求至少 1 人' : undefined}
      onChange={commit}
    />
  )
}

export function NodePanel({
  spec,
  targets = [],
  update,
  onDelete,
  deleteArmRef,
}: {
  spec: DslNode
  /** 分支可选目标(当前画布除自身以外的节点)。 */
  targets?: DslNode[]
  update: (patch: (s: DslNode) => DslNode) => void
  /**
   * 删除该节点(T21.4 ④)。传入才渲染可见入口 —— 删除是危险动作,走画布的
   * 确认层(与执行面「取消执行」、审批面「驳回」同族):这里只发出请求,
   * 确认与焦点回归都由 CanvasEditor 的单一漏斗负责,键盘 Delete 与按钮同路。
   */
  onDelete?: () => void
  /** 确认层收起后焦点回到这个入口按钮(进入画布确认层的焦点回归锚点)。 */
  deleteArmRef?: Ref<HTMLButtonElement>
}) {
  const meta = nodeTypeMeta(spec.type)
  return (
    <div className="flex min-h-0 flex-col" data-testid={TID.nodePanel}>
      {/* 面板身份钉顶:长面板滚到底也知道自己在编辑哪个节点。 */}
      <header className="panel-head sticky top-0 z-10 bg-ground-1">
        <I glyph={meta.glyph} size={13} className="text-ink-2" />
        <span className="bn-type">{meta.label}</span>
        <code className="mono min-w-0 truncate text-xs text-ink-1">{spec.id}</code>
        {onDelete !== undefined ? (
          <Button
            ref={deleteArmRef}
            variant="danger"
            size="sm"
            iconOnly
            icon={Trash2}
            className="ml-auto shrink-0"
            aria-label="删除节点"
            title={`删除节点 ${spec.id}`}
            data-testid={TID.nodeDelete}
            onClick={onDelete}
          />
        ) : null}
      </header>

      <Section title="基本">
        <TextField
          label="标题"
          value={spec.title ?? ''}
          onChange={(v) => update((s) => ({ ...s, title: v }))}
          placeholder="节点标题"
        />
        <TextField
          label="描述"
          value={spec.description ?? ''}
          onChange={(v) => update((s) => ({ ...s, description: v }))}
          placeholder="可选描述"
        />
        <NumField
          label="重试次数"
          min={1}
          max={10}
          hint="schema 允许 1–10"
          value={spec.retry?.max_attempts}
          onChange={(v) => update((s) => ({ ...s, retry: v === undefined ? undefined : { max_attempts: v } }))}
        />
      </Section>

      {spec.type === 'llm' && <LlmSection spec={spec} update={update} />}
      {spec.type === 'tool' && <ToolSection spec={spec} update={update} />}
      {spec.type === 'branch' && <BranchSection spec={spec} update={update} targets={targets} />}
      {spec.type === 'approval' && <ApprovalSection spec={spec} update={update} />}
      {spec.type === 'parallel' && <ParallelSection spec={spec} update={update} />}
    </div>
  )
}

type Sec<T extends DslNode> = { spec: T; update: (patch: (s: DslNode) => DslNode) => void }

function LlmSection({ spec, update }: Sec<Extract<DslNode, { type: 'llm' }>>) {
  const prompt = spec.prompt
  return (
    <>
      <Section title="模型与采样">
        <TextField
          label="模型"
          hint="留空用平台默认"
          value={spec.model ?? ''}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, model: v } : s))}
        />
        <NumField
          label="温度"
          min={0}
          max={2}
          step={0.1}
          hint="0–2"
          value={spec.temperature}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, temperature: v } : s))}
        />
        <NumField
          label="最大 tokens"
          min={1}
          max={200000}
          hint="1–200000"
          value={spec.max_tokens}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, max_tokens: v } : s))}
        />
        <SelectField
          label="响应格式"
          value={spec.response_format ?? 'text'}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, response_format: v === 'text' ? undefined : v } : s))}
          options={[
            { value: 'text', label: 'text(默认)' },
            { value: 'json', label: 'json' },
          ]}
        />
      </Section>

      <Section title="提示词">
        <AreaField
          label="system 指令"
          hint="只写固定指令，不支持引用占位符。"
          value={prompt.system ?? ''}
          onChange={(v) =>
            update((s) => (s.type === 'llm' ? { ...s, prompt: { ...s.prompt, system: v === '' ? undefined : v } } : s))
          }
        />
        <AreaField
          label="提示词模板"
          hint="模板只写固定指令，不支持 {{…}}；触发参数与节点输出请通过下方 user_data 传入。"
          rows={5}
          value={prompt.template}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, prompt: { ...s.prompt, template: v } } : s))}
          placeholder="受信作者指令;数据经下方 user_data 进入"
        />
      </Section>

      <Section title="user_data" hint="数据通道：name + 纯引用">
        <p className="field-hint">{'引用示例：{{inputs.text}} 或 {{nodes.parse.output.text}}（必须来自上游节点）。运行时会把每项以「名称: 数据」追加在模板后，无需在模板里放占位符。'}</p>
        <ListRows
          rowLabel="user_data 行"
          items={prompt.user_data ?? []}
          make={() => ({ name: '', ref: '' })}
          onChange={(next) => update((s) => (s.type === 'llm' ? { ...s, prompt: { ...s.prompt, user_data: next } } : s))}
          renderRow={(u, patch, i) => (
            <>
              <TextField
                ariaLabel={`user_data 名称 ${i + 1}`}
                value={u.name}
                onChange={(v) => patch({ name: v })}
                placeholder="标签名"
              />
              <TextField
                ariaLabel={`user_data 引用 ${i + 1}`}
                value={u.ref}
                onChange={(v) => patch({ ref: v })}
                placeholder="{{inputs.text}}"
              />
            </>
          )}
        />
      </Section>

      <Section title="输出声明" hint="名 → 类型">
        <KVRows
          rows={spec.outputs ?? {}}
          onChange={(next) => update((s) => (s.type === 'llm' ? { ...s, outputs: next } : s))}
          keyLabel="输出名"
          valLabel="类型"
        />
      </Section>
    </>
  )
}

function ToolSection({ spec, update }: Sec<Extract<DslNode, { type: 'tool' }>>) {
  return (
    <>
      <Section title="工具" hint="file_read / http 等">
        <TextField
          label="工具名"
          value={spec.tool}
          onChange={(v) => update((s) => (s.type === 'tool' ? { ...s, tool: v } : s))}
          placeholder="如 file_read / http"
        />
      </Section>

      <Section title="工具入参" hint="名 → 字面量或引用">
        {/* 已知边界(对抗审查登记):onChange 对全表重解析,字面形态的
            合法字符串值("5"/"true")在同节点编辑其他行时被翻转型。行级
            保留原类型需 KVRows 改造,登记不修;引用 {{...}} 形态安全。 */}
        <KVRows
          rows={Object.fromEntries(
            Object.entries(spec.inputs ?? {}).map(([k, v]) => [k, typeof v === 'string' ? v : JSON.stringify(v)]),
          )}
          onChange={(next) =>
            update((s) =>
              s.type === 'tool'
                ? { ...s, inputs: Object.fromEntries(Object.entries(next).map(([k, v]) => [k, parseLiteral(v)])) }
                : s,
            )
          }
          keyLabel="入参名"
          valLabel="值"
        />
      </Section>

      <Section title="输出声明" hint="名 → 类型">
        <KVRows
          rows={spec.outputs ?? {}}
          onChange={(next) => update((s) => (s.type === 'tool' ? { ...s, outputs: next } : s))}
          keyLabel="输出名"
          valLabel="类型"
        />
      </Section>
    </>
  )
}

function BranchSection({ spec, update, targets }: Sec<Extract<DslNode, { type: 'branch' }>> & { targets: DslNode[] }) {
  const options = targets.map((n) => ({ value: n.id, label: n.title ? `${n.title} (${n.id})` : n.id }))
  const targetOptions = (value: string) => [
    { value: '', label: '选择目标节点…', disabled: true },
    ...options,
    ...(value !== '' && !targets.some((n) => n.id === value) ? [{ value, label: `不存在的节点 (${value})`, disabled: true }] : []),
  ]
  return (
    <>
      <Section title="分支条件" hint="when 命中走 next">
        <p className="field-hint">从条件或默认出口连线，也可在下方选择目标；连线与去向会同步更新。多个条件可指向同一节点。</p>
        {targets.length === 0 ? <p className="field-hint">先在画布添加其他节点，再选择分支目标。</p> : null}
        <ListRows
          rowLabel="分支"
          items={spec.cases}
          make={() => ({ when: { left: '', op: 'eq', right: '' }, next: '' })}
          onChange={(next) => update((s) => (s.type === 'branch' ? { ...s, cases: next } : s))}
          renderRow={(c, patch, i) => (
            <div className="grid grid-cols-[minmax(0,1fr)_5.5rem] gap-1.5">
              <TextField
                ariaLabel={`条件左值 ${i + 1}`}
                value={c.when.left}
                onChange={(v) => patch({ when: { ...c.when, left: v } })}
                placeholder="{{nodes.x.output.y}}"
              />
              <SelectField
                ariaLabel={`条件运算符 ${i + 1}`}
                value={c.when.op}
                onChange={(v) => patch({ when: { ...c.when, op: v } })}
                options={OPS.map((o) => ({ value: o, label: o }))}
              />
              {/* 右值字面量嗅探:数字/布尔/null/合法 JSON 恢复原类型,
                  引用 {{...}} 与普通字符串原样(gt/gte/lt/lte 是强类型比较)。 */}
              <TextField
                ariaLabel={`条件右值 ${i + 1}`}
                value={String(c.when.right ?? '')}
                onChange={(v) => patch({ when: { ...c.when, right: v === '' ? undefined : parseLiteral(v) } })}
                placeholder="右值(exists 留空)"
              />
              <div className="col-span-2">
                <SelectField
                  label={`条件去向 ${i + 1}`}
                  value={c.next}
                  onChange={(v) => patch({ next: v })}
                  options={targetOptions(c.next)}
                  error={c.next !== '' && !targets.some((n) => n.id === c.next) ? '目标节点不存在，请重新选择。' : undefined}
                />
              </div>
            </div>
          )}
        />
      </Section>

      <Section title="兜底">
        <SelectField
          label="默认去向"
          value={spec.default}
          onChange={(v) => update((s) => (s.type === 'branch' ? { ...s, default: v } : s))}
          options={targetOptions(spec.default)}
          hint="条件都未命中时走此节点。"
          error={spec.default !== '' && !targets.some((n) => n.id === spec.default) ? '目标节点不存在，请重新选择。' : undefined}
        />
      </Section>
    </>
  )
}

function ApprovalSection({ spec, update }: Sec<Extract<DslNode, { type: 'approval' }>>) {
  return (
    <>
      <Section title="审批人">
        <ApproversInput spec={spec} update={update} />
        <SelectField
          label="签署模式"
          value={spec.mode ?? 'any'}
          onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, mode: v } : s))}
          options={[
            { value: 'any', label: 'any · 或签(任一票定局)' },
            // all 会签:t14.2 已合入(schema enum ["any","all"]),D-t141 #5 留位已消费。
            { value: 'all', label: 'all · 会签(全部批准才过,任一驳回即败)' },
          ]}
        />
      </Section>

      <Section title="超时">
        <NumField
          label="超时(小时)"
          min={0.1}
          max={2160}
          hint="0.1–2160(缺省 72)"
          value={spec.timeout_hours}
          onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, timeout_hours: v } : s))}
        />
        <SelectField
          label="超时策略"
          value={spec.on_timeout ?? 'escalate'}
          onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, on_timeout: v } : s))}
          options={[
            { value: 'escalate', label: 'escalate · 升级通知' },
            { value: 'fail', label: 'fail · 判定失败' },
          ]}
        />
      </Section>

      <Section title="审批说明">
        <AreaField
          label="审批说明"
          value={spec.instructions ?? ''}
          onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, instructions: v } : s))}
        />
      </Section>

      <Section title="输出声明" hint="名 → 类型">
        <KVRows
          rows={spec.outputs ?? {}}
          onChange={(next) => update((s) => (s.type === 'approval' ? { ...s, outputs: next } : s))}
          keyLabel="输出名"
          valLabel="类型"
        />
      </Section>
    </>
  )
}

function ParallelSection({ spec, update }: Sec<Extract<DslNode, { type: 'parallel' }>>) {
  const body = spec.parallel.body
  return (
    <>
      <Section title="扇出" hint="foreach → body 逐实例展开">
        <TextField
          label="foreach(数组引用)"
          value={spec.parallel.foreach}
          onChange={(v) => update((s) => (s.type === 'parallel' ? { ...s, parallel: { ...s.parallel, foreach: v } } : s))}
          placeholder="{{inputs.items}}"
        />
        <SelectField
          label="失败策略"
          hint="缺省 = fail_fast"
          value={spec.parallel.on_failure ?? ''}
          onChange={(v) =>
            update((s) =>
              s.type === 'parallel' ? { ...s, parallel: { ...s.parallel, on_failure: v === '' ? undefined : v } } : s,
            )
          }
          options={[
            { value: '', label: '(缺省 = fail_fast)' },
            { value: 'fail_fast', label: 'fail_fast' },
            { value: 'wait_all', label: 'wait_all' },
          ]}
        />
      </Section>

      <Section title="body 模板" hint="每个实例都按它展开">
        <SelectField
          label="类型"
          value={body.type}
          onChange={(v) =>
            update((s) =>
              s.type === 'parallel'
                ? {
                    ...s,
                    parallel: {
                      ...s.parallel,
                      body:
                        v === 'llm'
                          ? { type: 'llm', prompt: { template: '' }, outputs: {} }
                          : { type: 'tool', tool: '', inputs: {}, outputs: {} },
                    },
                  }
                : s,
            )
          }
          options={[
            { value: 'llm', label: 'LLM' },
            { value: 'tool', label: '工具' },
          ]}
        />
        {body.type === 'llm' ? (
          <>
            <AreaField
              label="提示词模板"
              rows={4}
              value={body.prompt.template}
              onChange={(v) =>
                update((s) =>
                  s.type === 'parallel' && s.parallel.body.type === 'llm'
                    ? {
                        ...s,
                        parallel: {
                          ...s.parallel,
                          body: { ...s.parallel.body, prompt: { ...s.parallel.body.prompt, template: v } },
                        },
                      }
                    : s,
                )
              }
              placeholder="受信作者指令"
              hint="模板不支持 {{…}}；元素数据须在 DSL 的 body.prompt.user_data 中用 {{item}} 或 {{item.field}} 引用，运行时追加在模板后。"
            />
            <Group label="body 输出声明" hint="名 → 类型">
              <KVRows
                rows={body.outputs ?? {}}
                onChange={(next) =>
                  update((s) =>
                    s.type === 'parallel' && s.parallel.body.type === 'llm'
                      ? { ...s, parallel: { ...s.parallel, body: { ...s.parallel.body, outputs: next } } }
                      : s,
                  )
                }
                keyLabel="输出名"
                valLabel="类型"
              />
            </Group>
          </>
        ) : (
          <>
            <TextField
              label="工具名"
              value={body.tool}
              onChange={(v) =>
                update((s) =>
                  s.type === 'parallel' && s.parallel.body.type === 'tool'
                    ? { ...s, parallel: { ...s.parallel, body: { ...s.parallel.body, tool: v } } }
                    : s,
                )
              }
            />
            <Group label="body 输出声明" hint="名 → 类型">
              <KVRows
                rows={body.outputs ?? {}}
                onChange={(next) =>
                  update((s) =>
                    s.type === 'parallel' && s.parallel.body.type === 'tool'
                      ? { ...s, parallel: { ...s.parallel, body: { ...s.parallel.body, outputs: next } } }
                      : s,
                  )
                }
                keyLabel="输出名"
                valLabel="类型"
              />
            </Group>
          </>
        )}
      </Section>
    </>
  )
}
