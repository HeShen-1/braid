// 节点属性面板:五类节点全覆盖。编辑 = immutable patch 回写 spec
// (上层 updateSpec 替换 data.spec 引用触发 RF 重渲);词域约束
// (mode/ops/on_timeout)以镜像枚举呈现,权威校验仍在服务端。
import { useState } from 'react'
import { BRANCH_OPS, type DslNode } from './model/dsl.ts'
import { AreaField, KVRows, ListRows, NumField, Row, SelectField, TextField, parseLiteral } from './fields.tsx'
import { TYPE_LABELS } from './defaults.ts'

const OPS = BRANCH_OPS as readonly string[]

/**
 * ApproversInput:本地原文态输入。join/split 规范化直接做受控值会在
 * 打字中途吞分隔符(输入 "alice," → 规范化回显 "alice"),原文暂存、
 * 值向外提交,回显不被规范化打断。
 */
function ApproversInput({ spec, update }: Sec<Extract<DslNode, { type: 'approval' }>>) {
  const [raw, setRaw] = useState<string | null>(null)
  const commit = (v: string) => {
    setRaw(v)
    update((s) =>
      s.type === 'approval' ? { ...s, approvers: v.split(',').map((x) => x.trim()).filter((x) => x !== '') } : s,
    )
  }
  return <input className="f-text" type="text" value={raw ?? spec.approvers.join(', ')} onChange={(e) => commit(e.target.value)} placeholder="alice, bob" />
}

export function NodePanel({
  spec,
  update,
}: {
  spec: DslNode
  update: (patch: (s: DslNode) => DslNode) => void
}) {
  return (
    <div className="panel" data-testid="node-panel">
      <div className="panel-head">
        <span className={`bn-badge bn-badge-${spec.type}`}>{TYPE_LABELS[spec.type]}</span>
        <code className="panel-id">{spec.id}</code>
      </div>

      <Row label="标题">
        <TextField value={spec.title ?? ''} onChange={(v) => update((s) => ({ ...s, title: v }))} placeholder="节点标题" />
      </Row>
      <Row label="描述">
        <TextField
          value={spec.description ?? ''}
          onChange={(v) => update((s) => ({ ...s, description: v }))}
          placeholder="可选描述"
        />
      </Row>
      <Row label="重试次数">
        <NumField
          value={spec.retry?.max_attempts}
          onChange={(v) => update((s) => ({ ...s, retry: v === undefined ? undefined : { max_attempts: v } }))}
        />
      </Row>

      {spec.type === 'llm' && <LlmSection spec={spec} update={update} />}
      {spec.type === 'tool' && <ToolSection spec={spec} update={update} />}
      {spec.type === 'branch' && <BranchSection spec={spec} update={update} />}
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
      <Row label="模型">
        <TextField value={spec.model ?? ''} onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, model: v } : s))} placeholder="留空用平台默认" />
      </Row>
      <Row label="温度">
        <NumField
          step={0.1}
          value={spec.temperature}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, temperature: v } : s))}
        />
      </Row>
      <Row label="最大 tokens">
        <NumField value={spec.max_tokens} onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, max_tokens: v } : s))} />
      </Row>
      <Row label="响应格式">
        <SelectField
          value={spec.response_format ?? 'text'}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, response_format: v === 'text' ? undefined : v } : s))}
          options={[
            { value: 'text', label: 'text(默认)' },
            { value: 'json', label: 'json' },
          ]}
        />
      </Row>
      <Row label="system 指令">
        <AreaField
          value={prompt.system ?? ''}
          onChange={(v) =>
            update((s) => (s.type === 'llm' ? { ...s, prompt: { ...s.prompt, system: v === '' ? undefined : v } } : s))
          }
        />
      </Row>
      <Row label="提示词模板">
        <AreaField
          rows={5}
          value={prompt.template}
          onChange={(v) => update((s) => (s.type === 'llm' ? { ...s, prompt: { ...s.prompt, template: v } } : s))}
          placeholder="受信作者指令;数据经下方 user_data 进入"
        />
      </Row>
      <fieldset className="f-fieldset">
        <legend>user_data(数据通道,name + 纯引用)</legend>
        <ListRows
          rowLabel="user_data 行"
          items={prompt.user_data ?? []}
          make={() => ({ name: '', ref: '' })}
          onChange={(next) => update((s) => (s.type === 'llm' ? { ...s, prompt: { ...s.prompt, user_data: next } } : s))}
          renderRow={(u, patch) => (
            <>
              <TextField value={u.name} onChange={(v) => patch({ name: v })} placeholder="标签名" />
              <TextField value={u.ref} onChange={(v) => patch({ ref: v })} placeholder="{{nodes.x.output.y}}" />
            </>
          )}
        />
      </fieldset>
      <fieldset className="f-fieldset">
        <legend>输出声明(名 → 类型)</legend>
        <KVRows
          rows={spec.outputs ?? {}}
          onChange={(next) => update((s) => (s.type === 'llm' ? { ...s, outputs: next } : s))}
          keyLabel="输出名"
          valLabel="类型"
        />
      </fieldset>
    </>
  )
}

function ToolSection({ spec, update }: Sec<Extract<DslNode, { type: 'tool' }>>) {
  return (
    <>
      <Row label="工具名">
        <TextField value={spec.tool} onChange={(v) => update((s) => (s.type === 'tool' ? { ...s, tool: v } : s))} placeholder="如 file_read / http" />
      </Row>
      <fieldset className="f-fieldset">
        <legend>工具入参(名 → 字面量或引用)</legend>
        {/* 已知边界(对抗审查登记):onChange 对全表重解析,字面形态的
            合法字符串值("5"/"true")在同节点编辑其他行时被翻转型。行级
            保留原类型需 KVRows 改造,登记不修;引用 {{...}} 形态安全。 */}
        <KVRows
          rows={Object.fromEntries(Object.entries(spec.inputs ?? {}).map(([k, v]) => [k, typeof v === 'string' ? v : JSON.stringify(v)]))}
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
      </fieldset>
      <fieldset className="f-fieldset">
        <legend>输出声明(名 → 类型)</legend>
        <KVRows rows={spec.outputs ?? {}} onChange={(next) => update((s) => (s.type === 'tool' ? { ...s, outputs: next } : s))} keyLabel="输出名" valLabel="类型" />
      </fieldset>
    </>
  )
}


function BranchSection({ spec, update }: Sec<Extract<DslNode, { type: 'branch' }>>) {
  return (
    <>
      <fieldset className="f-fieldset">
        <legend>分支条件(when 命中走 next)</legend>
        <ListRows
          rowLabel="分支"
          items={spec.cases}
          make={() => ({ when: { left: '', op: 'eq', right: '' }, next: '' })}
          onChange={(next) => update((s) => (s.type === 'branch' ? { ...s, cases: next } : s))}
          renderRow={(c, patch) => (
            <div className="f-case">
              <TextField
                value={c.when.left}
                onChange={(v) => patch({ when: { ...c.when, left: v } })}
                placeholder="{{nodes.x.output.y}}"
              />
              <SelectField
                value={c.when.op}
                onChange={(v) => patch({ when: { ...c.when, op: v } })}
                options={OPS.map((o) => ({ value: o, label: o }))}
              />
              {/* 右值字面量嗅探:数字/布尔/null/合法 JSON 恢复原类型,
                  引用 {{...}} 与普通字符串原样(gt/gte/lt/lte 是强类型比较)。 */}
              <TextField value={String(c.when.right ?? '')} onChange={(v) => patch({ when: { ...c.when, right: v === '' ? undefined : parseLiteral(v) } })} placeholder="右值(exists 留空)" />
              <TextField value={c.next} onChange={(v) => patch({ next: v })} placeholder="目标节点 id" />
            </div>
          )}
        />
      </fieldset>
      <Row label="默认去向">
        <TextField value={spec.default} onChange={(v) => update((s) => (s.type === 'branch' ? { ...s, default: v } : s))} placeholder="未命中时走" />
      </Row>
    </>
  )
}

function ApprovalSection({ spec, update }: Sec<Extract<DslNode, { type: 'approval' }>>) {
  return (
    <>
      <Row label="审批人(逗号分隔)">
        <ApproversInput spec={spec} update={update} />
      </Row>
      <Row label="签署模式">
        <SelectField
          value={spec.mode ?? 'any'}
          onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, mode: v } : s))}
          options={[
            { value: 'any', label: 'any · 或签(任一票定局)' },
            // all 会签:t14.2 已合入(schema enum ["any","all"]),D-t141 #5 留位已消费。
            { value: 'all', label: 'all · 会签(全部批准才过,任一驳回即败)' },
          ]}
        />
      </Row>
      <Row label="超时(小时)">
        <NumField value={spec.timeout_hours} onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, timeout_hours: v } : s))} />
      </Row>
      <Row label="超时策略">
        <SelectField
          value={spec.on_timeout ?? 'escalate'}
          onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, on_timeout: v } : s))}
          options={[
            { value: 'escalate', label: 'escalate · 升级通知' },
            { value: 'fail', label: 'fail · 判定失败' },
          ]}
        />
      </Row>
      <Row label="审批说明">
        <AreaField value={spec.instructions ?? ''} onChange={(v) => update((s) => (s.type === 'approval' ? { ...s, instructions: v } : s))} />
      </Row>
      <fieldset className="f-fieldset">
        <legend>输出声明(名 → 类型)</legend>
        <KVRows rows={spec.outputs ?? {}} onChange={(next) => update((s) => (s.type === 'approval' ? { ...s, outputs: next } : s))} keyLabel="输出名" valLabel="类型" />
      </fieldset>
    </>
  )
}

function ParallelSection({ spec, update }: Sec<Extract<DslNode, { type: 'parallel' }>>) {
  const body = spec.parallel.body
  return (
    <>
      <Row label="foreach(数组引用)">
        <TextField
          value={spec.parallel.foreach}
          onChange={(v) => update((s) => (s.type === 'parallel' ? { ...s, parallel: { ...s.parallel, foreach: v } } : s))}
          placeholder="{{inputs.items}}"
        />
      </Row>
      <Row label="失败策略">
        <SelectField
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
      </Row>
      <fieldset className="f-fieldset">
        <legend>body 模板(逐实例展开)</legend>
        <Row label="类型">
          <SelectField
            value={body.type}
            onChange={(v) =>
              update((s) =>
                s.type === 'parallel'
                  ? {
                      ...s,
                      parallel: {
                        ...s.parallel,
                        body: v === 'llm' ? { type: 'llm', prompt: { template: '' }, outputs: {} } : { type: 'tool', tool: '', inputs: {}, outputs: {} },
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
        </Row>
        {body.type === 'llm' ? (
          <>
            <Row label="提示词模板">
              <AreaField
                rows={4}
                value={body.prompt.template}
                onChange={(v) =>
                  update((s) =>
                    s.type === 'parallel' && s.parallel.body.type === 'llm'
                      ? { ...s, parallel: { ...s.parallel, body: { ...s.parallel.body, prompt: { ...s.parallel.body.prompt, template: v } } } }
                      : s,
                  )
                }
                placeholder="元素绑定 {{item}}"
              />
            </Row>
            <fieldset className="f-fieldset">
              <legend>body 输出声明</legend>
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
            </fieldset>
          </>
        ) : (
          <>
            <Row label="工具名">
              <TextField
                value={body.tool}
                onChange={(v) =>
                  update((s) =>
                    s.type === 'parallel' && s.parallel.body.type === 'tool'
                      ? { ...s, parallel: { ...s.parallel, body: { ...s.parallel.body, tool: v } } }
                      : s,
                  )
                }
              />
            </Row>
            <fieldset className="f-fieldset">
              <legend>body 输出声明</legend>
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
            </fieldset>
          </>
        )}
      </fieldset>
    </>
  )
}
