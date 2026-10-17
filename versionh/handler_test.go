package versionh

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/common/test/assert"
	"github.com/oekit/core/version"
)

func TestHandler_Response(t *testing.T) {
	version.BuildCommit = "test-commit"
	version.BuildBranch = "test-branch"
	version.BuildTime = "2026-01-01"

	c := app.NewContext(128)
	c.Set("trace_id", "test-trace")
	Handler(context.Background(), c)

	body := c.Response.Body()
	var result map[string]any
	_ = json.Unmarshal(body, &result)

	assert.DeepEqual(t, float64(0), result["code"])
	assert.DeepEqual(t, "ok", result["message"])
	assert.DeepEqual(t, "test-trace", result["trace_id"])

	data := result["data"].(map[string]any)
	assert.DeepEqual(t, "test-commit", data["buildCommit"])
	assert.DeepEqual(t, "test-branch", data["buildBranch"])
	assert.DeepEqual(t, "2026-01-01", data["buildTime"])

	if data["startTime"] == nil {
		t.Error("expected startTime")
	}
	if data["uptime"] == nil {
		t.Error("expected uptime")
	}
}

func TestBuildVersionResp_Fields(t *testing.T) {
	r := BuildVersionResp{
		Version:     "v0.8.0",
		BuildCommit: "abc",
		BuildTime:   "2026-01-01",
		BuildBranch: "main",
		StartTime:   "2026-01-01T00:00:00Z",
		Uptime:      "1h0m0s",
	}
	data, _ := json.Marshal(r)
	var m map[string]any
	_ = json.Unmarshal(data, &m)

	assert.DeepEqual(t, "v0.8.0", m["version"])
	assert.DeepEqual(t, "abc", m["buildCommit"])
	assert.DeepEqual(t, "2026-01-01", m["buildTime"])
	assert.DeepEqual(t, "main", m["buildBranch"])
	assert.DeepEqual(t, "2026-01-01T00:00:00Z", m["startTime"])
	assert.DeepEqual(t, "1h0m0s", m["uptime"])
}
