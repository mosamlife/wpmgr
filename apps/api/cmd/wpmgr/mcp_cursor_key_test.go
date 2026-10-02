package main

import (
	"strings"
	"testing"
)

func TestCheckMCPCursorKey(t *testing.T) {
	err := checkMCPCursorKey("")
	if err == nil {
		t.Fatal("empty key must refuse")
	}
	for _, want := range []string{"WPMGR_MCP_CURSOR_KEY", "at least 32 random characters", "openssl rand -hex 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message %q lacks %q", err, want)
		}
	}
	short := strings.Repeat("s", 31)
	if e := checkMCPCursorKey(short); e == nil {
		t.Fatal("31 chars must refuse")
	} else if strings.Contains(e.Error(), short) {
		t.Fatal("message must not echo the key")
	}
	if err := checkMCPCursorKey(strings.Repeat("k", 32)); err != nil {
		t.Fatalf("32 chars must pass: %v", err)
	}
}
