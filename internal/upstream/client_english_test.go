package upstream

import (
	"testing"
	"time"
)

func TestParseRateReset_English(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")

	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{
			name: "英文 will reset at 带 UTC+8",
			body: `{"code":6004,"msg":"usage exceeds frequency limit, but don't worry, your usage will reset at ` + ts + ` UTC+8, alternatively, you can switch to the other models to continue using it."}`,
			ok:   true,
		},
		{
			name: "英文 reset at 不带 UTC+8",
			body: `{"code":6004,"msg":"your usage will reset at ` + ts + `"}`,
			ok:   true,
		},
		{
			name: "英文大小写混合",
			body: `{"code":6004,"msg":"Your Usage Will RESET AT ` + ts + ` UTC+8"}`,
			ok:   true,
		},
		{
			name: "英文自然语言不应匹配",
			body: `{"code":6004,"msg":"your usage will reset at the end of the day"}`,
			ok:   false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if _, off := got.Zone(); off != 8*60*60 {
					t.Errorf("zone offset=%d want +08:00", off)
				}
			}
		})
	}
}
