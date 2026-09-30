// 工作流级面板:name/description/inputs/outputs(画布元数据不在此——
// 由画布拖拽产生)。name 落库后只读:后端语义「改名 = 新建工作流」
// (UpdateDraft 对 name 不一致返回 name_mismatch),可编辑只会让保存必红。
import type { WorkflowDoc } from './model/dsl.ts'
import { KVRows, ListRows, Row, SelectField, TextField } from './fields.tsx'

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
    <div className="panel" data-testid="workflow-panel">
      <div className="panel-head">
        <span className="bn-badge">工作流</span>
        <code className="panel-id">{doc.name}</code>
      </div>
      <Row label={nameLocked ? '名称(已建档;改名 = 新建工作流)' : '名称'}>
        <input
          className="f-text"
          type="text"
          value={doc.name}
          readOnly={nameLocked}
          aria-readonly={nameLocked}
          onChange={(e) => update((d) => ({ ...d, name: e.target.value }))}
        />
      </Row>
      <Row label="描述">
        <TextField value={doc.description ?? ''} onChange={(v) => update((d) => ({ ...d, description: v }))} />
      </Row>
      <fieldset className="f-fieldset">
        <legend>触发参数(inputs)</legend>
        <ListRows
          rowLabel="参数"
          items={doc.inputs ?? []}
          make={() => ({ name: '', type: 'string' })}
          onChange={(next) => update((d) => ({ ...d, inputs: next }))}
          renderRow={(inp, patch) => (
            <>
              <TextField value={inp.name} onChange={(v) => patch({ name: v })} placeholder="参数名" />
              <SelectField
                value={inp.type}
                onChange={(v) => patch({ type: v })}
                options={INPUT_TYPES.map((t) => ({ value: t, label: t }))}
              />
            </>
          )}
        />
      </fieldset>
      <fieldset className="f-fieldset">
        <legend>工作流输出(名 → 纯引用)</legend>
        <KVRows
          rows={doc.outputs ?? {}}
          onChange={(next) => update((d) => ({ ...d, outputs: next }))}
          keyLabel="输出名"
          valLabel="引用"
        />
      </fieldset>
    </div>
  )
}
