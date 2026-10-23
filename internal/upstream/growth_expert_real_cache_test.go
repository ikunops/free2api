package upstream

import "testing"

func TestLocalGrowthExpertsContainKnownWorkBuddyExperts(t *testing.T) {
	experts, err := loadLocalGrowthExperts()
	if err != nil {
		t.Skipf("this machine has no WorkBuddy expert cache: %v", err)
	}
	want := []string{"ex_2cvvUZQhDyeJ", "ex_Weo3x2EZuSDm", "ex_xQHHD1u5Jkua", "ex_WZY136wV5aMt", "ex_FbY5nlWTqDod", "ex_GDl31WahyIH0"}
	seen := map[string]bool{}
	for _, e := range experts {
		seen[e.ExpertID] = true
	}
	for _, id := range want {
		if !seen[id] {
			t.Errorf("known expert %s not parsed; got=%v", id, seen)
		}
	}
}
