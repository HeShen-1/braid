// testid 单一真源:组件与 e2e 共消费,禁止字面量散落(漂移即 e2e 静默失效)。
export const TID = {
  // 壳
  shell: 'shell',
  navWorkflows: 'nav-workflows',
  navExecutions: 'nav-executions',
  navApprovals: 'nav-approvals',
  logout: 'logout',
  whoami: 'whoami',
  /** 主题三态控件(auto/light/dark)。 */
  themeControl: 'theme-control',
  /** 工作流面两栏折叠。 */
  panelLeftToggle: 'panel-left-toggle',
  panelRightToggle: 'panel-right-toggle',

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
  /** 发布成功条(F04):列表态,编辑器卸载后仍可见。 */
  workflowFlash: 'workflow-flash',
  /** 成功条上的「查看已发布」出口(跳执行面触发页)。 */
  viewPublished: 'view-published',
  palette: 'palette',
  nodePanel: 'node-panel',
  /** 画布节点删除的可见入口(T21.4 ④)+ 它的确认层。 */
  nodeDelete: 'node-delete',
  deleteConfirm: 'delete-confirm',
  deleteConfirmSubmit: 'delete-confirm-submit',
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
  rollbackConfirm: 'rollback-confirm',
  resumeSubmit: 'resume-submit',
  cancelSubmit: 'cancel-submit',
  auditList: 'audit-list',
  /** 会签中间态:待决审批人名单(e2e 旅程核心断言)。 */
  pendingApprovers: 'pending-approvers',
  /** 执行终态输出快照。 */
  execOutputs: 'exec-outputs',

  // 审批面(T21.2)
  approvalQueue: 'approval-queue',
  approvalDetail: 'approval-detail',
  approvalApprove: 'approval-approve',
  approvalReject: 'approval-reject',
  approvalComment: 'approval-comment',
  approvalRefresh: 'approval-refresh',
  approvalEmpty: 'approval-empty',
  approvalFlash: 'approval-flash',
  approvalMeta: 'approval-meta',
  approvalTimeline: 'approval-timeline',
  approvalInstructions: 'approval-instructions',
  approvalDeadline: 'approval-deadline',
  approvalPrev: 'approval-prev',
  approvalNext: 'approval-next',
} as const

/** 动态 testid:节点行、草稿项、执行行、调色板按钮、主题档位。 */
export const tid = {
  draftItem: (id: string) => `draft-item-${id}`,
  themeOption: (theme: string) => `theme-option-${theme}`,
  execRow: (id: string) => `exec-row-${id}`,
  nodeRow: (nodeId: string, instanceNo: number) => `node-row-${nodeId}-${instanceNo}`,
  paletteBtn: (type: string) => `palette-${type}`,
  nodeCard: (nodeId: string) => `node-card-${nodeId}`,
  approvalEntry: (nodeId: string) => `approval-${nodeId}`,
  /** 待办行:执行 × 节点是队列行的天然主键。 */
  approvalRow: (executionId: string, nodeId: string) => `approval-row-${executionId}-${nodeId}`,
  /** 待办行「已等待」格(T21.4 增项):同一主键。 */
  approvalWaited: (executionId: string, nodeId: string) => `approval-waited-${executionId}-${nodeId}`,
  /** 审计链按事件词计数锚(rolled_back / approval.decided / node.state_changed)。 */
  auditRow: (eventType: string) => `audit-row-${eventType}`,
} as const
