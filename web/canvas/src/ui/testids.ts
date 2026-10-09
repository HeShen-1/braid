// testid 单一真源:组件与 e2e 共消费,禁止字面量散落(漂移即 e2e 静默失效)。
export const TID = {
  // 壳
  shell: 'shell',
  navWorkflows: 'nav-workflows',
  navExecutions: 'nav-executions',
  navApprovals: 'nav-approvals',
  logout: 'logout',
  whoami: 'whoami',

  // 登录
  loginForm: 'login-form',
  loginUsername: 'login-username',
  loginPassword: 'login-password',
  loginSubmit: 'login-submit',
  loginError: 'login-error',

  // 工作流面
  draftList: 'draft-list',
  draftNew: 'draft-new',
  draftEmpty: 'draft-empty',
  editor: 'editor',
  editorSave: 'editor-save',
  editorPublish: 'editor-publish',
  editorJson: 'editor-json',
  editorFlash: 'editor-flash',
  palette: 'palette',
  nodePanel: 'node-panel',
  issues: 'issues',

  // 执行面
  execTable: 'exec-table',
  execEmpty: 'exec-empty',
  execDetail: 'exec-detail',
  execStatus: 'exec-status',
  execFlash: 'exec-flash',
  execRibbon: 'exec-ribbon',
  triggerPanel: 'trigger-panel',
  triggerDefinition: 'trigger-definition',
  triggerInput: 'trigger-input',
  triggerSubmit: 'trigger-submit',
  rollbackSubmit: 'rollback-submit',
  resumeSubmit: 'resume-submit',
  cancelSubmit: 'cancel-submit',
  auditList: 'audit-list',

  // 审批面(T21.2)
  approvalQueue: 'approval-queue',
  approvalDetail: 'approval-detail',
  approvalApprove: 'approval-approve',
  approvalReject: 'approval-reject',
  approvalComment: 'approval-comment',
} as const

/** 动态 testid:节点行、草稿项、执行行、调色板按钮。 */
export const tid = {
  draftItem: (id: string) => `draft-item-${id}`,
  execRow: (id: string) => `exec-row-${id}`,
  nodeRow: (nodeId: string, instanceNo: number) => `node-row-${nodeId}-${instanceNo}`,
  paletteBtn: (type: string) => `palette-${type}`,
  nodeCard: (nodeId: string) => `node-card-${nodeId}`,
  approvalEntry: (nodeId: string) => `approval-${nodeId}`,
} as const
