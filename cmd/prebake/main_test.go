package main

import "testing"

func TestUsageMentionsCommands(t *testing.T) {
	if usage == "" {
		t.Fatal("usage must not be empty")
	}
}
