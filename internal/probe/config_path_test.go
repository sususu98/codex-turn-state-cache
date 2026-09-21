package probe

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigPathUsesHostFlagsAndWorkingDirectory(t *testing.T) {
	empty := func(string) string { return "" }
	tests := []struct {
		args           []string
		explicit, want string
	}{
		{[]string{"/bin/cpa"}, "", "/work/config.yaml"},
		{[]string{"cpa", "-config", "conf/live.yaml"}, "", "/work/conf/live.yaml"},
		{[]string{"cpa", "--config=/other/cpa.yaml"}, "", "/other/cpa.yaml"},
		{[]string{"cpa", "-config=first.yaml", "--config", "last.yaml"}, "", "/work/last.yaml"},
		{[]string{"cpa", "-password", "SECRET", "-local-model", "--config=/other/cpa.yaml"}, "", "/other/cpa.yaml"},
		{[]string{"cpa", "--", "-config", "ignored.yaml"}, "", "/work/config.yaml"},
		{[]string{"cpa", "positional", "-config", "ignored.yaml"}, "", "/work/config.yaml"},
		{[]string{"cpa", "-local-model", "false", "-config=ignored.yaml"}, "", "/work/config.yaml"},
		{[]string{"cpa", "--plugin-flag=value", "-config=live.yaml"}, "", "/work/live.yaml"},
		{[]string{"cpa", "-config=cli.yaml"}, "legacy.yaml", "/work/legacy.yaml"},
	}
	for _, test := range tests {
		got, err := ResolveHostConfigPath(test.explicit, test.args, "/work", empty)
		if err != nil || got != filepath.Clean(test.want) {
			t.Fatalf("path=%q want=%q err=%v", got, test.want, err)
		}
	}
}
func TestConfigPathRefusesAmbiguousAndNonFileBackends(t *testing.T) {
	for _, args := range [][]string{{"cpa", "-config"}, {"cpa", "--config="}, {"cpa", "-home-jwt=SECRET"}, {"cpa", "-unknown", "SECRET", "-config=x"}} {
		_, err := ResolveHostConfigPath("", args, "/work", nil)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("unsafe flag inference")
		}
	}
	for _, key := range []string{"HOME_JWT", "PGSTORE_DSN", "OBJECTSTORE_ENDPOINT", "GITSTORE_GIT_URL", "pgstore_dsn"} {
		env := func(k string) string {
			if k == key {
				return "SECRET"
			}
			return ""
		}
		_, err := ResolveHostConfigPath("/explicit.yaml", []string{"cpa", "-config=cli.yaml"}, "/work", env)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("non-file backend accepted or leaked")
		}
	}
}
