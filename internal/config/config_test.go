package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAndRelativePaths(t *testing.T) {
	base := filepath.FromSlash("/srv/homebase")
	cfg, err := Parse([]byte(`{"apps":[{"name":"web","dir":"apps/web","command":"web.exe","env_file":"secrets/web.env"}]}`), base)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Apps[0]
	if cfg.Listen != "127.0.0.1:8090" || cfg.LogDir != filepath.Join(base, "logs") {
		t.Errorf("config defaults: %+v", cfg)
	}
	if a.Title != "web" || a.Restart != RestartOnFailure || a.HealthIntervalSec != 15 || a.StopTimeoutSec != 10 || a.UnhealthyAfter != 3 {
		t.Errorf("app defaults: %+v", a)
	}
	if a.Dir != filepath.Join(base, "apps", "web") || a.EnvFile != filepath.Join(base, "secrets", "web.env") {
		t.Errorf("paths not resolved against the config's folder: %q %q", a.Dir, a.EnvFile)
	}
}

func TestInvalidConfigs(t *testing.T) {
	cases := map[string]string{
		"bad name":             `{"apps":[{"name":"My App","dir":"d","command":"c"}]}`,
		"duplicate":            `{"apps":[{"name":"a","dir":"d","command":"c"},{"name":"a","dir":"d","command":"c"}]}`,
		"no command":           `{"apps":[{"name":"a","dir":"d"}]}`,
		"no dir":               `{"apps":[{"name":"a","command":"c"}]}`,
		"bad restart":          `{"apps":[{"name":"a","dir":"d","command":"c","restart":"sometimes"}]}`,
		"bad health url":       `{"apps":[{"name":"a","dir":"d","command":"c","health":"localhost:5206"}]}`,
		"unhealthy no health":  `{"apps":[{"name":"a","dir":"d","command":"c","restart_when_unhealthy":true}]}`,
		"typo'd key":           `{"apps":[{"name":"a","dir":"d","command":"c","autostrat":true}]}`,
		"source without build": `{"apps":[{"name":"a","dir":"d","command":"c","source":{"repo_dir":"r"}}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(input), "/x"); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "app.env")
	content := "# secrets\n\nANTHROPIC_API_KEY=\"sk-ant-test\"\nexport MODE='prod'\nEMPTY=\nURL=http://x/?a=b\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := ReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-test", "MODE": "prod", "EMPTY": "", "URL": "http://x/?a=b"}
	for k, v := range want {
		if env[k] != v {
			t.Errorf("%s = %q, want %q", k, env[k], v)
		}
	}
}

func TestEnvFileErrorsDontLeakSecrets(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.env")
	os.WriteFile(p, []byte("sk-ant-SECRET-without-equals\n"), 0o600)
	_, err := ReadEnvFile(p)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Errorf("err = %v", err)
	}
}
