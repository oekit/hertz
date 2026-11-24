package registry

import (
	"reflect"
	"testing"

	hregistry "github.com/cloudwego/hertz/pkg/app/server/registry"

	ereg "github.com/oekit/registry"
)

// TestInstancesToHertz_WeightFallback #318 回归：发现侧 Weight <= 0 回填
// hregistry.DefaultWeight，与注册侧 toInstance / sdbridge.ToInstance 的
// defaultWeight 兜底同口径（kitex/kitex #92 的 hertz 孪生）。registry.Instance
// 的零值写法不带 Weight，透传 0 会让零/负权重原样进入发现侧。
func TestInstancesToHertz_WeightFallback(t *testing.T) {
	instances := []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080, Weight: 0},  // 零值写法（静态发现测试同形）
		{IP: "10.0.0.2", Port: 8081, Weight: -5}, // 负权重同样兜底
		{IP: "10.0.0.3", Port: 8082, Weight: 77}, // 显式正权重保留
	}
	result := InstancesToHertz(instances)
	if len(result) != 3 {
		t.Fatalf("len = %d, want 3", len(result))
	}

	if got := result[0].Weight(); got != hregistry.DefaultWeight {
		t.Errorf("zero weight -> %d, want DefaultWeight %d", got, hregistry.DefaultWeight)
	}
	if got := result[1].Weight(); got != hregistry.DefaultWeight {
		t.Errorf("negative weight -> %d, want DefaultWeight %d", got, hregistry.DefaultWeight)
	}
	if got := result[2].Weight(); got != 77 {
		t.Errorf("explicit weight -> %d, want 77", got)
	}

	// hertz 自带 instance 的 Weight() 在读取时兜底（weight<=0 →
	// DefaultWeight），仅断言接口无法区分「构造期已回填」与「读取时兜底」，
	// 用反射钉住原始存储值——这是 #318 真正的修复点（转换产物权重恒为正，
	// 契约不依赖框架实现细节）。hertz 内部字段若重构更名则跳过该断言，
	// 接口契约断言仍在。
	if v := reflect.ValueOf(result[0]).Elem(); v.IsValid() {
		if f := v.FieldByName("weight"); f.IsValid() && f.CanInt() {
			if got := int(f.Int()); got != hregistry.DefaultWeight {
				t.Errorf("raw stored weight = %d, want DefaultWeight %d（构造期回填应生效）", got, hregistry.DefaultWeight)
			}
		}
	}
}

// TestInstancesToHertz_Empty 输入为空返回空切片（非 nil）。
func TestInstancesToHertz_Empty(t *testing.T) {
	result := InstancesToHertz(nil)
	if result == nil {
		t.Error("InstancesToHertz(nil) 应返回空切片而非 nil")
	}
	if len(result) != 0 {
		t.Errorf("len = %d, want 0", len(result))
	}
}

// TestInstancesToHertz_FractionalWeightNotTruncatedToZero #335 回归：权重
// 先判定后截断时，(0,1) 区间的小数权重（registry.Instance.Weight 是
// float64，nacos 可配）会被 int() 截成 0 透传——违反「转换产物权重恒为正」
// 契约。修法对齐 kitex #92：先截断再回填。
func TestInstancesToHertz_FractionalWeightNotTruncatedToZero(t *testing.T) {
	instances := []ereg.Instance{
		{IP: "10.0.0.1", Port: 8080, Weight: 0.5},
		{IP: "10.0.0.2", Port: 8081, Weight: 0.99},
		{IP: "10.0.0.3", Port: 8082, Weight: 1.7}, // >1 的小数截断保留整数部分
	}
	got := InstancesToHertz(instances)
	for i, inst := range got {
		w := inst.Weight()
		if w <= 0 {
			t.Fatalf("实例 %d 权重 %v 截断后为 %d——(0,1) 区间权重被截成 0 透传（#335）", i, instances[i].Weight, w)
		}
	}
	if got[2].Weight() != 1 {
		t.Fatalf("权重 1.7 截断应得 1，got %d", got[2].Weight())
	}
}
