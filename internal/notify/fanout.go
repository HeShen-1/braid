package notify

// Fanout 是多通道通知扇出(T14.3,D-2026-09-30-t143 决策 5):worker 的
// approval.notify 事件经单一 ApprovalNotifier 换装点扇出到 SMTP + IM 两
// 通道,per-channel 去重锚(approval.notified / approval.im_notified)把
// 失败域隔离——任一通道失败 ⇒ errors.Join 上抛 ⇒ asynq 重投 ⇒ 成功通道
// 被自家锚短路(ErrAlreadyNotified 吸收为该通道成功),只补发失败通道,
// 不双发。两通道全未配置时装配面返回 nil(worker 保留 T4.3 占位降级),
// Fanout 不处理空集。

import (
	"context"
	"errors"
	"fmt"
)

// Notifier 是通知通道的最小接口(worker.ApprovalNotifier 的包内镜像:
// 消费面接口在 worker 侧定义,本包零反向依赖——Sender/IMSender 结构化
// 满足,扇出面只依赖本包符号)。
type Notifier interface {
	NotifyApproval(ctx context.Context, execID, nodeExecID uint64, reason string, round int) error
}

// Channel 是扇出的一个通道:Label 只用于错误归因(errors.Join 聚合后
// 运维要能分辨哪个通道在烧重试)。
type Channel struct {
	Label string // "smtp" | "im"(词面自由,错误面消费)
	N     Notifier
}

// Fanout 见包注释。
type Fanout struct {
	chans []Channel
}

// NewFanout 构造扇出;空集返回 nil(装配面 switch 到单通道直连,不造
// 空扇出假成功面)。
func NewFanout(chans ...Channel) *Fanout {
	if len(chans) == 0 {
		return nil
	}
	return &Fanout{chans: chans}
}

// NotifyApproval 逐通道发送:ErrAlreadyNotified = 该通道已送达(幂等
// 收敛,吸收为成功);其余错误裹通道名聚合上抛。聚合语义:全通道成功
// (或已送达)⇒ nil;任一失败 ⇒ joined error(worker 重投,成功通道
// 下轮被自家锚短路)。
func (f *Fanout) NotifyApproval(ctx context.Context, execID, nodeExecID uint64, reason string, round int) error {
	var errs []error
	for _, ch := range f.chans {
		if err := ch.N.NotifyApproval(ctx, execID, nodeExecID, reason, round); err != nil {
			if errors.Is(err, ErrAlreadyNotified) {
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %w", ch.Label, err))
		}
	}
	return errors.Join(errs...)
}
