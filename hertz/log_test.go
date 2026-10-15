package hertz

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/cloudwego/hertz/pkg/common/hlog"
	"github.com/oekit/logx"
	logxhlog "github.com/oekit/logx/hlog"
)

func TestNewLogger_ImplementsFullLogger(t *testing.T) {
	logger := NewLogger(logx.NewSlogLogger("test"))
	//nolint:staticcheck // 显式 interface compliance check，省略 type 后断言失去意义 (QF1011)
	var _ hlog.FullLogger = logger
}

func TestNewLogger_VariadicMethods(t *testing.T) {
	logger := NewLogger(logx.NewSlogLogger("test-variadic"))

	// Should not panic
	logger.Trace("trace", 1)
	logger.Debug("debug", 2)
	logger.Info("info", 3)
	logger.Notice("notice", 4)
	logger.Warn("warn", 5)
	logger.Error("error", 6)
	logger.Fatal("fatal", 7)
}

func TestNewLogger_FormatMethods(t *testing.T) {
	logger := NewLogger(logx.NewSlogLogger("test-format"))

	// Should not panic
	logger.Tracef("trace %d", 1)
	logger.Debugf("debug %d", 2)
	logger.Infof("info %d", 3)
	logger.Noticef("notice %d", 4)
	logger.Warnf("warn %d", 5)
	logger.Errorf("error %d", 6)
	logger.Fatalf("fatal %d", 7)
}

func TestNewLogger_CtxMethods(t *testing.T) {
	logger := NewLogger(logx.NewSlogLogger("test-ctx"))
	ctx := context.Background()

	// Should not panic
	logger.CtxTracef(ctx, "trace %d", 1)
	logger.CtxDebugf(ctx, "debug %d", 2)
	logger.CtxInfof(ctx, "info %d", 3)
	logger.CtxNoticef(ctx, "notice %d", 4)
	logger.CtxWarnf(ctx, "warn %d", 5)
	logger.CtxErrorf(ctx, "error %d", 6)
	logger.CtxFatalf(ctx, "fatal %d", 7)
}

func TestNewLogger_SetLevel(t *testing.T) {
	logger := NewLogger(logx.NewSlogLogger("test-level"))

	// Should not panic
	logger.SetLevel(hlog.LevelDebug)
	logger.SetLevel(hlog.LevelInfo)
	logger.SetLevel(hlog.LevelWarn)
	logger.SetLevel(hlog.LevelError)
}

func TestNewLogger_SetOutput_NoOp(t *testing.T) {
	logger := NewLogger(logx.NewSlogLogger("test-output"))

	// Should not panic (no-op)
	logger.SetOutput(nil)
}

func TestNewLogger_NilSafety(t *testing.T) {
	// NewLogger should work with a valid logger
	logger := NewLogger(logx.NewSlogLogger("test-nil"))
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}
}

func TestNewLogger_PanicsOnHlogBacked(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic when passing hlog-backed Logger")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("expected string panic value, got %T", r)
		}
		if !strings.Contains(msg, "infinite recursion") {
			t.Fatalf("unexpected panic message: %s", msg)
		}
	}()

	// hlog.NewHlogLogger returns hlog-backed Logger — must panic
	NewLogger(logxhlog.NewHlogLogger("test-hlog"))
}

// TestNewLogger_CallerAttribution 验证适配器帧不再被归属为 caller：
// 经由 hlogAdapter 输出的日志，caller 应指向本测试文件而非 hertz/log.go
// （与 kitex/log.go 对称）。
func TestNewLogger_CallerAttribution(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	NewLogger(logx.NewSlogLogger("caller-test")).Info("caller probe")
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)

	outStr := string(out)
	if !strings.Contains(outStr, `"caller":`) {
		t.Fatalf("log output missing caller field: %s", outStr)
	}
	if strings.Contains(outStr, `"caller":"log.go:`) {
		t.Fatalf("caller still points into adapter file hertz/log.go: %s", outStr)
	}
	if !strings.Contains(outStr, `"caller":"log_test.go:`) {
		t.Fatalf("caller should point into this test file: %s", outStr)
	}
}
