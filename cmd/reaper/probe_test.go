package main

// inspectorProbe 映射锚(T18.7,D-t174 已知边界收口:适配层原无直接单测):
// 活态词表五态 / 死态两态 / NotFound 干净死判 / 其余错误原样上抛(reaper
// 侧 fail-open)。零 Redis 消费(taskInfoGetter 缝 + fake,不触发槽位扩表);
// 真链覆盖仍在 T15.6 drill 的 kill -9 残窗臂。

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hibiken/asynq"
)

// fakeGetter 记录 (queue, taskID) 透传并回放可编程结果。
type fakeGetter struct {
	queue, taskID string
	info          *asynq.TaskInfo
	err           error
}

func (g *fakeGetter) GetTaskInfo(queue, taskID string) (*asynq.TaskInfo, error) {
	g.queue, g.taskID = queue, taskID
	return g.info, g.err
}

func TestInspectorProbeTaskAliveStates(t *testing.T) {
	cases := []struct {
		state asynq.TaskState
		alive bool
		desc  string
	}{
		{asynq.TaskStatePending, true, "pending = 活(积压证据)"},
		{asynq.TaskStateActive, true, "active = 活(执行中)"},
		{asynq.TaskStateScheduled, true, "scheduled = 活(定时未到)"},
		{asynq.TaskStateRetry, true, "retry = 活(退避待重跑)"},
		{asynq.TaskStateAggregating, true, "aggregating = 活(组聚合未决)"},
		{asynq.TaskStateArchived, false, "archived = 非活(死信,另有告警面)"},
		{asynq.TaskStateCompleted, false, "completed = 非活(retention 内已成功)"},
	}
	for _, tc := range cases {
		g := &fakeGetter{info: &asynq.TaskInfo{State: tc.state}}
		p := inspectorProbe{insp: g}
		alive, err := p.TaskAlive(context.Background(), "braid-reaper", "att:1:2")
		if err != nil {
			t.Fatalf("%s: err = %v, want nil", tc.desc, err)
		}
		if alive != tc.alive {
			t.Errorf("%s: alive = %v, want %v", tc.desc, alive, tc.alive)
		}
		if g.queue != "braid-reaper" || g.taskID != "att:1:2" {
			t.Errorf("%s: 透传 = (%q, %q), want 原样", tc.desc, g.queue, g.taskID)
		}
	}
}

func TestInspectorProbeTaskAliveErrors(t *testing.T) {
	// NotFound = 干净死判(重派照旧);其余错误 = 上抛(reaper 侧 fail-open,
	// T18.7 计数面消费)。两者必须分族:把可达错误折成死判 = 硬化静默失效。
	p := inspectorProbe{insp: &fakeGetter{err: asynq.ErrTaskNotFound}}
	if alive, err := p.TaskAlive(context.Background(), "q", "t"); alive || err != nil {
		t.Fatalf("NotFound: = (%v, %v), want (false, nil)", alive, err)
	}
	boom := fmt.Errorf("redis: connection refused")
	p = inspectorProbe{insp: &fakeGetter{err: boom}}
	if _, err := p.TaskAlive(context.Background(), "q", "t"); !errors.Is(err, boom) {
		t.Fatalf("可达错误: err = %v, want 原样上抛", err)
	}
	// nil info + nil err 的防御形态(asynq API 契约外):不 panic,判非活。
	p = inspectorProbe{insp: &fakeGetter{}}
	if alive, err := p.TaskAlive(context.Background(), "q", "t"); alive || err != nil {
		t.Fatalf("nil info: = (%v, %v), want (false, nil) 防御性死判", alive, err)
	}
}
