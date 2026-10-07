package upstream

import (
	"bytes"
	"io"
	"net/http"
	"testing"
	"time"

	"free2api/internal/auth"
)

// mkDetailedResp 构造带 CycleEndTime 的 get-user-resource 响应（R-A/R-B 实测上游
// 不下发 PackageEndTime，到期时间真实字段是 CycleEndTime）。
func mkDetailedResp(accounts string) *http.Response {
	body := `{"code":0,"data":{"Response":{"Data":{"TotalCount":1,"Accounts":[` + accounts + `]}}}}`
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(body))),
		Header:     make(http.Header),
	}
}

func TestUserResourceDetailedSplitsExpiring(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, buckets, err := c.UserResourceDetailed(a, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if buckets.Expiring != 1200 {
		t.Errorf("expiring=%d want 1200 (3天内过期的奖励包)", buckets.Expiring)
	}
	if buckets.Stable != 300 {
		t.Errorf("stable=%d want 300 (30天后才过期的周期包)", buckets.Stable)
	}
	if buckets.Total() != remain {
		t.Errorf("total=%d != remain=%d", buckets.Total(), remain)
	}
}

// TestUserResourceDetailedSplitsUrgent 两级分桶：urgent（3 天）⊆ expiring（7 天）。
// 3 天内到期的进 Urgent，7 天内到期的进 Expiring（不含 Urgent），30 天的落 Stable。
func TestUserResourceDetailedSplitsUrgent(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in5d := now.Add(5 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"紧急","CycleEndTime":"` + in3d + `","CycleCapacitySize":1000,"CycleCapacityRemain":900,"CycleCapacityUsed":100},` +
				`{"PackageName":"临近","CycleEndTime":"` + in5d + `","CycleCapacitySize":1000,"CycleCapacityRemain":700,"CycleCapacityUsed":300},` +
				`{"PackageName":"稳定","CycleEndTime":"` + in30d + `","CycleCapacitySize":1000,"CycleCapacityRemain":500,"CycleCapacityUsed":500}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, buckets, err := c.UserResourceDetailed(a, 7*24*time.Hour, 3*24*time.Hour)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 2100 {
		t.Errorf("remain=%d want 2100", remain)
	}
	if buckets.Urgent != 900 {
		t.Errorf("urgent=%d want 900（3 天内到期的紧急包）", buckets.Urgent)
	}
	if buckets.Expiring != 1600 {
		t.Errorf("expiring=%d want 1600（7 天内，含紧急）", buckets.Expiring)
	}
	if buckets.Stable != 500 {
		t.Errorf("stable=%d want 500（30 天后的稳定包）", buckets.Stable)
	}
	if buckets.Urgent > buckets.Expiring {
		t.Errorf("urgent(%d) 必须是 expiring(%d) 的子集", buckets.Urgent, buckets.Expiring)
	}
	if buckets.Total() != remain {
		t.Errorf("total=%d != remain=%d", buckets.Total(), remain)
	}
}

// TestUserResourceDetailedUrgentDisabled urgent<=0（或 >soon）时紧急档不启用，
// 行为退回单级（本用例锁 urgent=0 的全部归 Expiring）。
func TestUserResourceDetailedUrgentDisabled(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"` + in3d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Urgent != 0 || buckets.Expiring != 80 {
		t.Errorf("urgent=0 时紧急档应关闭: %+v", buckets)
	}
}

func TestUserResourceDetailedNoWindowAllStable(t *testing.T) {
	in3d := time.Now().Add(3 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleEndTime":"` + in3d + `","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	// soon<=0：禁用分桶，全部归 Stable（向后兼容旧行为）。
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("buckets=%+v want {Expiring:0 Stable:80} when soon=0", buckets)
	}
}

func TestUserResourceDetailedMissingEndTimeStable(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		// 无 CycleEndTime 字段：保守归 Stable，不误标快过期插队。
		return mkDetailedResp(
			`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	_, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if buckets.Expiring != 0 || buckets.Stable != 80 {
		t.Errorf("buckets=%+v want {Expiring:0 Stable:80} for missing end time", buckets)
	}
}

func TestUserResourceBackwardCompat(t *testing.T) {
	// 旧 UserResource 签名与总量口径不变。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 80 {
		t.Errorf("UserResource remain=%d err=%v, want 80/nil", remain, err)
	}
}

// TestUserResourceDetailedExpiringBucketFromCycleEndTime 锁定 Expiring 分桶判据是
// CycleEndTime（R-A/R-B 实测：CN/global 两域 42 字段均无 PackageEndTime，判 PackageEndTime
// 恒 miss → Expiring 恒 0，快过期积分因子从未生效）。上游只下发 CycleEndTime
// （global Bonus Pack 实测 14 天赠送积分到期时刻即此字段）。
func TestUserResourceDetailedExpiringBucketFromCycleEndTime(t *testing.T) {
	now := time.Now()
	in3d := now.Add(3 * 24 * time.Hour).Format(packageEndLayout)
	in30d := now.Add(30 * 24 * time.Hour).Format(packageEndLayout)
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"奖励包","CycleEndTime":"` + in3d + `","CycleCapacitySize":1500,"CycleCapacityRemain":1200,"CycleCapacityUsed":300},` +
				`{"PackageName":"周期包","CycleEndTime":"` + in30d + `","CycleCapacitySize":500,"CycleCapacityRemain":300,"CycleCapacityUsed":200}`), nil
	})
	remain, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
	if buckets.Expiring != 1200 {
		t.Errorf("expiring=%d want 1200 (CycleEndTime 在 7 天窗内的奖励包)", buckets.Expiring)
	}
	if buckets.Stable != 300 {
		t.Errorf("stable=%d want 300 (CycleEndTime 在窗外的周期包)", buckets.Stable)
	}
}

// TestUserResourceDetailedSharesPackageRemainUsed 锁定单套餐取数统一到
// packageRemainUsed 口径：脏数据 CycleCapacityRemain=600 > Size=500 必须钳到 500
// （旧中间 switch 只钳负值不钳超限，会高估 remain）。
func TestUserResourceDetailedSharesPackageRemainUsed(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			`{"PackageName":"脏数据","CycleCapacitySize":500,"CycleCapacityRemain":600,"CycleCapacityUsed":0}`), nil
	})
	remain, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 7*24*time.Hour, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	if remain != 500 {
		t.Errorf("remain=%d want 500 (CycleRemain>Size 脏数据钳到 size)", remain)
	}
	if buckets.Total() != 500 {
		t.Errorf("total=%d want 500", buckets.Total())
	}
}

// TestUserResourceDetailedTotalMatchesLegacy 零回归锚：三种套餐形态的 Total 与
// 旧口径（中间 switch + 负值钳 0）逐一相等——口径统一到 packageRemainUsed 不得
// 改变 remain 聚合结果。
func TestUserResourceDetailedTotalMatchesLegacy(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return mkDetailedResp(
			// 形态 1：纯 Cycle 活跃（当天有消耗，R-C 实测只扣 Cycle 字段）。
			`{"PackageName":"体验版","CycleCapacitySize":500,"CycleCapacityRemain":17,"CycleCapacityUsed":482,"CapacitySize":500,"CapacityRemain":500,"CapacityUsed":0},` +
				// 形态 2：Cycle 三零（从未使用）→ 回退 Capacity。
				`{"PackageName":"未使用","CapacitySize":300,"CapacityRemain":300,"CapacityUsed":0},` +
				// 形态 3：status=3 已结束裂变包（Cycle/Capacity 全套 0/100/100）。
				`{"PackageName":"已结束","CycleCapacitySize":100,"CycleCapacityRemain":0,"CycleCapacityUsed":100,"CapacitySize":100,"CapacityRemain":0,"CapacityUsed":100}`), nil
	})
	remain, buckets, err := c.UserResourceDetailed(&auth.Auth{AccessToken: "at"}, 0, 0)
	if err != nil {
		t.Fatalf("detailed: %v", err)
	}
	// 旧口径：形态1 → CycleRemain=17；形态2 → CapacityRemain=300；形态3 → ②分支 CycleRemain=0。
	if remain != 317 {
		t.Errorf("remain=%d want 317 (17+300+0, 与旧口径逐一相等)", remain)
	}
	if buckets.Total() != remain {
		t.Errorf("total=%d != remain=%d", buckets.Total(), remain)
	}
}
