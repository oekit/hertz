package validatorh

import (
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
)

// newTestContext builds a hertz RequestContext with optional query params
func newTestContext(query string) *app.RequestContext {
	h := app.NewContext(0)
	if query != "" {
		h.Request.URI().SetQueryString(query)
	}
	return h
}

func TestBindPage_Defaults(t *testing.T) {
	c := newTestContext("")
	p := BindPage(c)
	if p.Page <= 0 || p.PageSize <= 0 {
		t.Errorf("expected normalized defaults, got %+v", p)
	}
}

func TestBindPage_FromQuery(t *testing.T) {
	c := newTestContext("page=3&page_size=50")
	p := BindPage(c)
	if p.Page != 3 {
		t.Errorf("Page = %d, want 3", p.Page)
	}
	if p.PageSize != 50 {
		t.Errorf("PageSize = %d, want 50", p.PageSize)
	}
}

func TestBindPageWithDefault(t *testing.T) {
	c := newTestContext("")
	p := BindPageWithDefault(c, 5, 25)
	if p.Page != 5 {
		t.Errorf("Page = %d, want 5", p.Page)
	}
	if p.PageSize != 25 {
		t.Errorf("PageSize = %d, want 25", p.PageSize)
	}
}

func TestBindPageWithDefault_FromQuery(t *testing.T) {
	c := newTestContext("page=10&page_size=100")
	p := BindPageWithDefault(c, 1, 20)
	if p.Page != 10 {
		t.Errorf("Page = %d, want 10", p.Page)
	}
	if p.PageSize != 100 {
		t.Errorf("PageSize = %d, want 100", p.PageSize)
	}
}

// TestBindPage_RangeOverflowFallsBack 回归 #236：越界 page（Atoi 返回钳位值
// + ErrRange）必须按非法值回落默认——此前错误被丢弃，Page=MaxInt64 一路传给
// 消费方（拼游标/塞 SQL/翻页溢出），与 page=abc 回落默认的口径分裂。
func TestBindPage_RangeOverflowFallsBack(t *testing.T) {
	c := newTestContext("page=99999999999999999999")
	p := BindPage(c)
	if p.Page != 1 {
		t.Errorf("Page = %d, want 1 (range overflow must fall back to default, not MaxInt64)", p.Page)
	}
	if p.PageSize != 20 {
		t.Errorf("PageSize = %d, want 20 (default)", p.PageSize)
	}
}

// TestBindPageWithDefault_RangeOverflow 同上，自定义默认值形态。
func TestBindPageWithDefault_RangeOverflow(t *testing.T) {
	c := newTestContext("page=99999999999999999999&page_size=99999999999999999999")
	p := BindPageWithDefault(c, 5, 25)
	if p.Page != 5 {
		t.Errorf("Page = %d, want 5 (custom default; range overflow is an illegal value)", p.Page)
	}
	if p.PageSize != 25 {
		t.Errorf("PageSize = %d, want 25 (custom default)", p.PageSize)
	}
}

// TestBindPage_IllegalInputsUnified 钉住：page=abc（invalid syntax）与越界
// 数值（ErrRange）口径一致——都回落默认。
func TestBindPage_IllegalInputsUnified(t *testing.T) {
	for _, q := range []string{"page=abc", "page=3.0", "page=9223372036854775808"} {
		c := newTestContext(q)
		p := BindPage(c)
		if p.Page != 1 {
			t.Errorf("q=%q: Page = %d, want 1 (all illegal values must fall back to default)", q, p.Page)
		}
	}
}
