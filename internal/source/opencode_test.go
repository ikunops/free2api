package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestImportOpenCodeRefreshesChangedKey 回归：OpenCode 重新登录后 auth.json 换 key，
// 但号池里同名文件还在时，重复导入必须刷新旧凭据，不能永远按文件名跳过。
func TestImportOpenCodeRefreshesChangedKey(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "auth.json")
	authDir := filepath.Join(dir, "auths")
	write := func(key string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"opencode": map[string]string{"type": "api", "key": key},
		})
		if err := os.WriteFile(src, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("sk-old")
	res, err := ImportOpenCodeAs(src, authDir, []string{"opencode"}, ImportOptions{})
	if err != nil || len(res.Imported) != 1 {
		t.Fatalf("first import: res=%+v err=%v", res, err)
	}

	write("sk-new")
	res, err = ImportOpenCodeAs(src, authDir, []string{"opencode"}, ImportOptions{})
	if err != nil || len(res.Imported) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("changed key must refresh: res=%+v err=%v", res, err)
	}
	raw, err := os.ReadFile(filepath.Join(authDir, AuthFileName(OpenCodeUID(OpenCodeZenProvider))))
	if err != nil {
		t.Fatal(err)
	}
	var got Flat
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "sk-new" || got.Producer != ProducerOpenCode {
		t.Fatalf("stale token not refreshed: %+v", got)
	}

	res, err = ImportOpenCodeAs(src, authDir, []string{"opencode"}, ImportOptions{})
	if err != nil || len(res.Skipped) != 1 || len(res.Imported) != 0 {
		t.Fatalf("same token must stay idempotent: res=%+v err=%v", res, err)
	}
}
