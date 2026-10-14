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
