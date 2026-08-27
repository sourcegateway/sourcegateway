// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestConfigureGitCommandIsolatesAmbientConfiguration checks security-sensitive environment filtering.
func TestConfigureGitCommandIsolatesAmbientConfiguration(t *testing.T) {
	t.Setenv("GIT_SSL_NO_VERIFY", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "http.sslVerify")
	t.Setenv("GIT_CONFIG_VALUE_0", "false")
	t.Setenv("GIT_PROTOCOL", "ambient")
	t.Setenv("SOURCEGATEWAY_HOOK_CONTEXT", "ambient")
	t.Setenv("GIT_QUARANTINE_PATH", "/quarantine")

	command := exec.Command("git", "version")
	ConfigureGitCommand(command, "GIT_PROTOCOL=version=2")
	environment := environmentMap(command.Env)

	for _, name := range []string{"GIT_SSL_NO_VERIFY", "GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0", "SOURCEGATEWAY_HOOK_CONTEXT"} {
		if _, found := environment[name]; found {
			t.Errorf("%s leaked into Git environment", name)
		}
	}
	if got := environment["GIT_PROTOCOL"]; got != "version=2" {
		t.Errorf("GIT_PROTOCOL = %q", got)
	}
	if got := environment["GIT_QUARANTINE_PATH"]; got != "/quarantine" {
		t.Errorf("GIT_QUARANTINE_PATH = %q", got)
	}
	if environment["GIT_TERMINAL_PROMPT"] != "0" || environment["GCM_INTERACTIVE"] != "never" {
		t.Error("Git credential prompting was not disabled")
	}
}

// environmentMap indexes environment entries by variable name for assertions.
func environmentMap(values []string) map[string]string {
	environment := make(map[string]string, len(values))
	for _, value := range values {
		name, value, _ := strings.Cut(value, "=")
		environment[name] = value
	}
	return environment
}

// TestConfigureGitCommandPreservesOrdinaryEnvironment checks benign environment preservation.
func TestConfigureGitCommandPreservesOrdinaryEnvironment(t *testing.T) {
	t.Setenv("SOURCEGATEWAY_TEST_ENVIRONMENT", "present")
	command := exec.Command("git", "version")
	ConfigureGitCommand(command)
	if environmentMap(command.Env)["SOURCEGATEWAY_TEST_ENVIRONMENT"] != "present" {
		t.Fatal("ordinary environment variable was removed")
	}
}

// TestRefCommandsMatchRequiresCompleteAtomicResult checks failed-push reconciliation.
func TestRefCommandsMatchRequiresCompleteAtomicResult(t *testing.T) {
	zero := strings.Repeat("0", 40)
	first := strings.Repeat("1", 40)
	second := strings.Repeat("2", 40)
	commands := []refCommand{{new: first, ref: "refs/heads/first"}, {new: zero, ref: "refs/heads/deleted"}}
	if !refCommandsMatch(commands, first+"\trefs/heads/first\n") {
		t.Fatal("complete post-push state did not match")
	}
	if refCommandsMatch(commands, second+"\trefs/heads/first\n") {
		t.Fatal("different upstream ref matched")
	}
	if refCommandsMatch(commands, first+"\trefs/heads/first\n"+second+"\trefs/heads/deleted\n") {
		t.Fatal("undeleted upstream ref matched")
	}
}

// TestInstallProcReceiveHookDoesNotRequireShell checks direct executable hook installation.
func TestInstallProcReceiveHookDoesNotRequireShell(t *testing.T) {
	repository := filepath.Join(t.TempDir(), "repository.git")
	command := exec.Command("git", "init", "--bare", repository) // #nosec G204 -- repository is created by this test.
	ConfigureGitCommand(command)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	if err := InstallProcReceiveHook(repository); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(repository, "hooks", "proc-receive")
	target, err := os.Readlink(hook)
	if err != nil {
		t.Fatalf("proc-receive hook is not a symbolic link: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if target != executable {
		t.Fatalf("proc-receive target = %q, want %q", target, executable)
	}
}
