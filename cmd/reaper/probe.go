package main

// inspectorProbe 实现 reaper.AsynqProbe(T17.4,D-2026-10-04-t174):asynq
// Inspector 的活态判定,装配在组合根(internal/reaper 保持零 asynq 依赖,
// D-t25 #1「正确性结构性不依赖 Redis」的包图形态)。真实链路覆盖 =
// T15.6 多副本演练栈(kill -9 残窗接管臂);本文件是 asynq API 的薄映射。

import (
	"context"
	"errors"

	"github.com/hibiken/asynq"
)

type inspectorProbe struct{ insp *asynq.Inspector }

// TaskAlive:NotFound/archived/completed = 非活(archived 死信另有
// AsynqArchivedDeadLetters 告警面;completed 即删无 retention);pending/
// active/scheduled/retry/aggregating = 活(积压证据,不是死)。Inspector
// 自身无 ctx 面,入参 ctx 刻意不用(接口为调用方形态统一保留)。
func (p inspectorProbe) TaskAlive(_ context.Context, queue, taskID string) (bool, error) {
	info, err := p.insp.GetTaskInfo(queue, taskID)
	if err != nil {
		if errors.Is(err, asynq.ErrTaskNotFound) {
			return false, nil
		}
		return false, err
	}
	switch info.State {
	case asynq.TaskStatePending, asynq.TaskStateActive,
		asynq.TaskStateScheduled, asynq.TaskStateRetry, asynq.TaskStateAggregating:
		return true, nil
	}
	return false, nil
}
