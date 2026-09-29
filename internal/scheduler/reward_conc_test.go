package scheduler

// reward_conc_test.go 代码健康审计（任务书第 1 条）补缺：rewardClaimed
// map 的并发读写压力（claimGrowthRewards 的 markRewardClaimed/rewardClaimedToday
// 全在 s.mu 下，本文件验证该不变量在 -race 下成立 + 同日幂等不被并发打破）。
import (
	"sync"
	"testing"
	"time"
)

// TestRewardClaimedConcurrentMarkAndCheck 多 goroutine 并发 markRewardClaimed /
// rewardClaimedToday 混发：-race 验证 map 访问全在锁内；完成后当日标记必须
// 命中（并发 mark 写同值，最终一致）。
func TestRewardClaimedConcurrentMarkAndCheck(t *testing.T) {
	s := &Scheduler{rewardClaimed: map[string]string{}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				switch (i + n) % 3 {
				case 0:
					s.markRewardClaimed("u1")
				case 1:
					_ = s.rewardClaimedToday("u1")
				default:
					s.markRewardClaimed("u2")
				}
			}
		}(i)
	}
	wg.Wait()
	if !s.rewardClaimedToday("u1") || !s.rewardClaimedToday("u2") {
		t.Fatal("after concurrent marks, both uids must be claimed-today (same-day write wins)")
	}
}

// TestRewardClaimedConcurrentExpiryRead 跨日标记并发读：旧标记在读线程不
// 断 check 的同时被 mark 覆写为当日——无竞态 + 最终态当日命中。
func TestRewardClaimedConcurrentExpiryRead(t *testing.T) {
	s := &Scheduler{rewardClaimed: map[string]string{}}
	s.rewardClaimed["u1"] = travelDay(time.Now().Add(-24 * time.Hour))
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			_ = s.rewardClaimedToday("u1")
		}
	}()
	go func() {
		defer wg.Done()
		for n := 0; n < 200; n++ {
			s.markRewardClaimed("u1")
		}
	}()
	wg.Wait()
	if !s.rewardClaimedToday("u1") {
		t.Fatal("final state must be today after concurrent overwrite")
	}
}
