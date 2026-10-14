package hertz

import (
	"fmt"
	"io"
	"sync"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/yi-nology/ekit-logx"
)

// recordingLogger 记录 SetLevel 调用，用于断言 hlog → logx 级别映射。
// 仅实现 SetLevel，其余方法内嵌 nil 接口（测试只调 SetLevel，不会触发）。
type recordingLogger struct {
	logx.Logger

	mu    sync.Mutex
	level logx.Level
}

func (r *recordingLogger) SetLevel(lv logx.Level) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.level = lv
}

func (r *recordingLogger) current() logx.Level {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.level
}

// TestHlogAdapter_SetLevel_Mapping 验证 SetLevel 与方法级映射一致：
// hlog.LevelNotice 应落 logx.LevelInfo（Noticef → Infof），而非 Warn。
func TestHlogAdapter_SetLevel_Mapping(t *testing.T) {
	cases := []struct {
		in   hlog.Level
		want logx.Level
	}{
		{hlog.LevelTrace, logx.LevelDebug},
		{hlog.LevelDebug, logx.LevelDebug},
		{hlog.LevelInfo, logx.LevelInfo},
		{hlog.LevelNotice, logx.LevelInfo},
		{hlog.LevelWarn, logx.LevelWarn},
		{hlog.LevelError, logx.LevelError},
		{hlog.LevelFatal, logx.LevelError},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("hlog%d", tc.in), func(t *testing.T) {
			rec := &recordingLogger{}
			a := &hlogAdapter{logger: rec}
			a.SetLevel(tc.in)
			if got := rec.current(); got != tc.want {
				t.Errorf("SetLevel(%d) = logx level %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestHlogAdapter_SetOutput_NoOp 文档化行为：logx 后端统一管理输出，
// SetOutput 有意 no-op（hlog.Control 契约要求方法存在）。
func TestHlogAdapter_SetOutput_NoOp(t *testing.T) {
	rec := &recordingLogger{}
	a := &hlogAdapter{logger: rec}
	a.SetOutput(io.Discard) // 不得 panic
}
