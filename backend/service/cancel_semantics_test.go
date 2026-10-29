package service

import (
	"context"
	"fmt"
	"testing"

	"flowedge/backend/model"
)

// 主动中止必须自带 cancelled 错误码。
//
// 否则 handler 只能看到一句普通 error，把它写成 status=failed，
// 界面就把「我自己按停的」显示成「执行失败」——这是审计里 C 类的状态谎言。
func TestCancelErrorCarriesCancelledCode(t *testing.T) {
	err := &nodeExecError{Code: model.ErrorCodeCancelled, Err: fmt.Errorf("运行已取消")}
	if got := classifyError(err); got != model.ErrorCodeCancelled {
		t.Fatalf("classifyError = %q, want %q", got, model.ErrorCodeCancelled)
	}
	if got := ErrorCodeOf(err); got != model.ErrorCodeCancelled {
		t.Fatalf("ErrorCodeOf = %q, want %q", got, model.ErrorCodeCancelled)
	}
	// 普通错误不能被误判成取消
	if got := ErrorCodeOf(fmt.Errorf("boom")); got == model.ErrorCodeCancelled {
		t.Fatalf("plain error must not be classified as cancelled")
	}
}

// 上下文已取消之后冒出来的错误必须改写成 cancelled。
//
// 实测由来（2026-10-26 run 242）：用户点停止 → 我们把 prompt 从 ComfyUI 队列撤掉 →
// ComfyUI 把它记成 error → 节点把"comfyui 执行失败"原样抛出 → 界面显示"失败"，
// 而实际上 2.2 秒就停了、还是用户自己按的。状态必须和真失败分开。
func TestCancelledIfContextDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := cancelledIfContextDone(ctx, fmt.Errorf("comfyui 执行失败（prompt_id=x）"))
	if code := ErrorCodeOf(got); code != model.ErrorCodeCancelled {
		t.Fatalf("取消后应是 cancelled，实际 %q（%v）", code, got)
	}

	// 没取消时不能改写：真失败还是真失败
	live := context.Background()
	plain := fmt.Errorf("comfyui 执行失败")
	if out := cancelledIfContextDone(live, plain); out != plain {
		t.Fatalf("未取消时不该改写错误，实际 %v", out)
	}
	// 成功返回不受影响
	if out := cancelledIfContextDone(ctx, nil); out != nil {
		t.Fatalf("err=nil 必须保持 nil，实际 %v", out)
	}
}
