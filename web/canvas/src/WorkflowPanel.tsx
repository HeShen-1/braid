// 工作流级面板:name/description/inputs/outputs(画布元数据不在此——
// 由画布拖拽产生)。name 落库后只读:后端语义「改名 = 新建工作流」
// (UpdateDraft 对 name 不一致返回 name_mismatch),可编辑只会让保存必红。
import { Workflow } from 'lucide-react'
import type { DslInput, WorkflowDoc } from './model/dsl.ts'
import { KVRows, ListRows, Section, SelectField, TextField } from './fields.tsx'
import { I } from './ui/I.tsx'
import { Checkbox } from './ui/field.tsx'

const INPUT_TYPES = ['string', 'number', 'boolean', 'object', 'array']

export function WorkflowPanel({
  doc,
  update,
  nameLocked,
}: {
  doc: WorkflowDoc
  update: (patch: (d: WorkflowDoc) => WorkflowDoc) => void
  nameLocked: boolean
}) {
  return (
    <div className="flex min-h-0 flex-col">
      <header className="panel-head sticky top-0 z-10 bg-ground-1">
        <I glyph={Workflow} size={13} className="text-ink-2" />
        <span className="bn-type">工作流</span>
        <code className="mono min-w-0 truncate text-xs text-ink-1">{doc.name}</code>
      </header>

      <Section title="身份">
        <TextField
          label={nameLocked ? '名称(已保存,改名 = 新建工作流)' : '名称'}
          value={doc.name}
          readOnly={nameLocked}
          onChange={(v) => update((d) => ({ ...d, name: v }))}
        />
        <TextField label="描述" value={doc.description ?? ''} onChange={(v) => update((d) => ({ ...d, description: v }))} />
      </Section>

      <Section title="触发参数" hint="inputs">
        <p className="field-hint">声明触发时提供的字段、类型和是否必填；执行页会显示所选发布版本的参数要求。</p>
        <ListRows<DslInput>
          rowLabel="参数"
          items={doc.inputs ?? []}
          make={() => ({ name: '', type: 'string' })}
          onChange={(next) => update((d) => ({ ...d, inputs: next }))}
          renderRow={(inp, patch, i) => (
            <>
              <TextField
                ariaLabel={`参数名 ${i + 1}`}
                value={inp.name}
                onChange={(v) => patch({ name: v })}
                placeholder="参数名"
              />
              <SelectField
                ariaLabel={`参数类型 ${i + 1}`}
                value={inp.type}
                onChange={(v) => patch({ type: v })}
                options={INPUT_TYPES.map((t) => ({ value: t, label: t }))}
              />
              <Checkbox label={`参数必填 ${i + 1}`} checked={inp.required === true} onCheckedChange={(required) => patch({ required })} />
              <TextField
                ariaLabel={`参数说明 ${i + 1}`}
                value={inp.description ?? ''}
                placeholder="用途或填写示例（可选）"
                onChange={(description) => patch({ description })}
              />
            </>
          )}
        />
      </Section>

      <Section title="工作流输出" hint="名 → 纯引用">
        <KVRows
          rows={doc.outputs ?? {}}
          onChange={(next) => update((d) => ({ ...d, outputs: next }))}
          keyLabel="输出名"
          valLabel="引用"
        />
      </Section>
    </div>
  )
}
