package main

import "testing"

func TestDispatchUnknownCommand(t *testing.T) {
	if code := dispatch([]string{"bogus"}); code != exitError {
		t.Fatalf("dispatch(bogus) = %d, want %d", code, exitError)
	}
	if code := dispatch(nil); code != exitError {
		t.Fatalf("dispatch(nil) = %d, want %d", code, exitError)
	}
}

func TestDispatchVersion(t *testing.T) {
	if code := dispatch([]string{"version"}); code != exitPass {
		t.Fatalf("dispatch(version) = %d, want %d", code, exitPass)
	}
}
