package main

import "testing"

func TestCompareVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"3.17.0", "3.17.2", -1},
		{"3.17.2", "3.17.2", 0},
		{"4.0.0", "3.99.99", 1},
		{"3.9.0", "3.10.0", -1},
		{"3.17.2", "bogus", 0},
	}
	for _, c := range cases {
		if got := compareVersion(c.a, c.b); got != c.want {
			t.Fatalf("compareVersion(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestMaxVersion(t *testing.T) {
	if got := maxVersion("3.14.4", "bogus", "3.17.2", ""); got != "3.17.2" {
		t.Fatalf("maxVersion = %q, want 3.17.2", got)
	}
	if got := maxVersion("bogus", ""); got != "" {
		t.Fatalf("maxVersion of invalid input = %q, want empty", got)
	}
}

// static 模式不联网: 只比较配置值与下限, 保证改动默认行为前 UA 不会掉到风控线以下。
func TestAppVersionStaticRespectsFloor(t *testing.T) {
	cases := []struct {
		name string
		cfg  pluginConfig
		want string
	}{
		{"below floor", pluginConfig{AppVersion: "3.14.4", VersionSource: versionSourceStatic}, defaultAppVersionFloor},
		{"empty source behaves static", pluginConfig{AppVersion: "3.14.4"}, defaultAppVersionFloor},
		{"above floor", pluginConfig{AppVersion: "3.18.1", VersionSource: versionSourceStatic}, "3.18.1"},
		{"custom floor", pluginConfig{AppVersion: "3.14.4", VersionSource: versionSourceStatic, AppVersionFloor: "3.20.0"}, "3.20.0"},
		{"empty app_version", pluginConfig{VersionSource: versionSourceStatic}, defaultAppVersion},
	}
	for _, c := range cases {
		if got := appVersion(c.cfg); got != c.want {
			t.Fatalf("%s: appVersion = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestVersionParts(t *testing.T) {
	if _, ok := versionParts("3.17"); ok {
		t.Fatal("two-component version must be rejected")
	}
	if _, ok := versionParts("v3.17.2"); ok {
		t.Fatal("prefixed version must be rejected")
	}
	parts, ok := versionParts("3.17.2")
	if !ok || parts[0] != 3 || parts[1] != 17 || parts[2] != 2 {
		t.Fatalf("versionParts = %v, %v", parts, ok)
	}
}

// record 是版本缓存的唯一写入口: 首次写入、只升不降、失败只记冷却。
func TestResolverRecord(t *testing.T) {
	resolver := &versionResolver{}

	resolver.record("3.17.2", nil)
	if got := resolver.current(); got != "3.17.2" {
		t.Fatalf("first record = %q, want 3.17.2", got)
	}

	resolver.record("3.14.4", nil)
	if got := resolver.current(); got != "3.17.2" {
		t.Fatalf("downgrade must be ignored, got %q", got)
	}

	resolver.record("3.18.0", nil)
	if got := resolver.current(); got != "3.18.0" {
		t.Fatalf("upgrade = %q, want 3.18.0", got)
	}

	resolver.record("", nil)
	if got := resolver.current(); got != "3.18.0" {
		t.Fatalf("empty result must keep the cached value, got %q", got)
	}
}
