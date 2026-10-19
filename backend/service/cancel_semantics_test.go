package service

import (
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
