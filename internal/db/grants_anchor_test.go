package db

// grants.sh 锁定读授权锚(T15.6 演练发现收口):MySQL 锁定读
// (SELECT ... FOR UPDATE)要求对应表写权限——审批决定事务的 TOCTOU 守卫
// (api/approvalstore.lockApprovalNode 锁 node_execution)与 rollback/resume
// 缝(engine node CAS + scheduler/rollback.go 锁 node_attempt)都在
// braid_api 连接上执行;缺 UPDATE = least-privilege 部署(交付 compose /
// chart)下审批与回滚 1142 全红。t13 集成测试 root 连接结构性不可见,
// 该面由多副本演练(internal/drill,least-privilege 真栈)承载——本锚
// 只防 grants.sh 文本回退。

import (
	"os"
	"strings"
	"testing"
)

func TestGrantsLockingReadAnchor(t *testing.T) {
	b, err := os.ReadFile("../../deploy/grants.sh")
	if err != nil {
		t.Fatalf("read grants.sh: %v", err)
	}
	s := string(b)
	// api 锁定读写权限面:node_execution(审批守卫 + node CAS)+ node_attempt
	// (回滚缝锁读)。措辞漂移在演练红(least-privilege 真审批),本锚钉结构。
	for _, want := range []string{
		"GRANT SELECT, INSERT, UPDATE ON braid.node_execution     TO 'braid_api'@'%'",
		"GRANT SELECT, INSERT, UPDATE ON braid.workflow_execution TO 'braid_api'@'%'",
		"GRANT SELECT, UPDATE         ON braid.node_attempt      TO 'braid_api'@'%'",
		"GRANT INSERT                  ON braid.audit_pending      TO 'braid_api'@'%'",
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("grants.sh missing locking-read write grant:\n  %q\n(api 审批/回滚缝的 FOR UPDATE 在 least-privilege 下 1142;T15.6 演练实证)", want)
		}
	}
}
