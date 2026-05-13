package config

import (
	"runtime"
	"strings"
	"testing"
)

func TestParse_Minimal(t *testing.T) {
	yaml := `
version: "1"
servers:
  echo:
    command: /bin/echo
`
	c, err := Parse([]byte(yaml), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(c.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(c.Servers))
	}
	s := c.Servers["echo"]
	if s.Command != "/bin/echo" {
		t.Errorf("command: %q", s.Command)
	}
}

func TestParse_RejectsUnknownFields(t *testing.T) {
	yaml := `
version: "1"
servers:
  echo:
    command: /bin/echo
    bogus_field: oops
`
	_, err := Parse([]byte(yaml), "test")
	if err == nil {
		t.Error("expected error on unknown field")
	}
}

func TestParse_RejectsBadVersion(t *testing.T) {
	yaml := `
version: "99"
servers:
  echo:
    command: /bin/echo
`
	_, err := Parse([]byte(yaml), "test")
	if err == nil || !strings.Contains(err.Error(), "unsupported config version") {
		t.Errorf("want version error, got %v", err)
	}
}

func TestParse_RequiresAtLeastOneServer(t *testing.T) {
	yaml := `version: "1"`
	_, err := Parse([]byte(yaml), "test")
	if err == nil {
		t.Error("expected error when no servers configured")
	}
}

func TestParse_ServerNameValidation(t *testing.T) {
	yaml := `
version: "1"
servers:
  "bad name":
    command: /bin/echo
`
	_, err := Parse([]byte(yaml), "test")
	if err == nil {
		t.Error("expected error on bad server name")
	}
}

func TestServer_MergesDefaultsAndOverrides(t *testing.T) {
	yaml := `
version: "1"
defaults:
  env:
    allow:
      - DEFAULT_VAR
servers:
  echo:
    command: /bin/echo
    env:
      allow:
        - SERVER_VAR
`
	c, err := Parse([]byte(yaml), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s, err := c.Server("echo")
	if err != nil {
		t.Fatalf("Server: %v", err)
	}
	have := map[string]bool{}
	for _, v := range s.Env.Allow {
		have[v] = true
	}
	if !have["DEFAULT_VAR"] || !have["SERVER_VAR"] {
		t.Errorf("merged allow missing entries: %v", s.Env.Allow)
	}
}

func TestFilterEnv_SystemDefaultsIncluded(t *testing.T) {
	// All system names should pass through with allow_system=true (default).
	var sample string
	if runtime.GOOS == "windows" {
		sample = "PATH"
	} else {
		sample = "HOME"
	}
	source := []string{sample + "=/x", "AWS_SECRET_ACCESS_KEY=hunter2"}
	cfg := EnvConfig{}
	out := FilterEnv(source, cfg)
	if !containsKey(out, sample) {
		t.Errorf("system var %q should pass through, got %v", sample, out)
	}
	if containsKey(out, "AWS_SECRET_ACCESS_KEY") {
		t.Errorf("AWS_SECRET_ACCESS_KEY should be stripped, got %v", out)
	}
}

func TestFilterEnv_AllowExtra(t *testing.T) {
	source := []string{"PATH=/x", "MY_CUSTOM_VAR=value", "AWS_SECRET=nope"}
	cfg := EnvConfig{Allow: []string{"MY_CUSTOM_VAR"}}
	out := FilterEnv(source, cfg)
	if !containsKey(out, "MY_CUSTOM_VAR") {
		t.Errorf("MY_CUSTOM_VAR should pass through, got %v", out)
	}
	if containsKey(out, "AWS_SECRET") {
		t.Errorf("AWS_SECRET should still be stripped, got %v", out)
	}
}

func TestFilterEnv_DenyWinsOverAllow(t *testing.T) {
	source := []string{"PATH=/x", "MY_VAR=value"}
	cfg := EnvConfig{
		Allow: []string{"MY_VAR"},
		Deny:  []string{"MY_VAR"},
	}
	out := FilterEnv(source, cfg)
	if containsKey(out, "MY_VAR") {
		t.Errorf("MY_VAR should be denied, got %v", out)
	}
	if !containsKey(out, "PATH") {
		t.Errorf("PATH should pass through, got %v", out)
	}
}

func TestFilterEnv_AllowSystemFalse(t *testing.T) {
	source := []string{"PATH=/x", "MY_VAR=value"}
	f := false
	cfg := EnvConfig{
		AllowSystem: &f,
		Allow:       []string{"MY_VAR"},
	}
	out := FilterEnv(source, cfg)
	if containsKey(out, "PATH") {
		t.Errorf("PATH should NOT pass through when allow_system=false, got %v", out)
	}
	if !containsKey(out, "MY_VAR") {
		t.Errorf("MY_VAR should pass through, got %v", out)
	}
}

func TestParse_HTTPServer(t *testing.T) {
	yaml := `
version: "1"
servers:
  remote:
    url: https://example.com/mcp
    headers:
      Authorization: "Bearer abc"
`
	c, err := Parse([]byte(yaml), "test")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	s := c.Servers["remote"]
	if s.URL != "https://example.com/mcp" {
		t.Errorf("url: %q", s.URL)
	}
	if s.Headers["Authorization"] != "Bearer abc" {
		t.Errorf("headers: %+v", s.Headers)
	}
}

func TestParse_RejectsBothCommandAndURL(t *testing.T) {
	yaml := `
version: "1"
servers:
  bad:
    command: npx
    url: https://example.com/mcp
`
	_, err := Parse([]byte(yaml), "test")
	if err == nil || !strings.Contains(err.Error(), "cannot set both") {
		t.Errorf("want both-set error, got %v", err)
	}
}

func TestParse_RejectsNeitherCommandNorURL(t *testing.T) {
	yaml := `
version: "1"
servers:
  bad:
    env:
      allow: [X]
`
	_, err := Parse([]byte(yaml), "test")
	if err == nil || !strings.Contains(err.Error(), "one of") {
		t.Errorf("want missing-transport error, got %v", err)
	}
}

func TestParse_RejectsBadURLScheme(t *testing.T) {
	yaml := `
version: "1"
servers:
  bad:
    url: ftp://example.com/mcp
`
	_, err := Parse([]byte(yaml), "test")
	if err == nil || !strings.Contains(err.Error(), "must start with http") {
		t.Errorf("want bad-scheme error, got %v", err)
	}
}

func TestStarter_ParsesAsValidWhenServerAdded(t *testing.T) {
	// The starter file is mostly comments; adding one minimal server should
	// make it parse cleanly. This pins the starter template's syntax.
	yaml := Starter() + "\n  test:\n    command: /bin/true\n"
	_, err := Parse([]byte(yaml), "starter")
	if err != nil {
		t.Errorf("starter + minimal server should parse: %v", err)
	}
}

func containsKey(env []string, name string) bool {
	want := name + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, want) {
			return true
		}
	}
	return false
}
