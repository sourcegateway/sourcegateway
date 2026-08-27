// Source Gateway <https://sourcegateway.dev>
// Copyright The Source Gateway Authors
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sourcegateway/sourcegateway/api/v1alpha1"
	"github.com/sourcegateway/sourcegateway/internal/auth"
	"github.com/sourcegateway/sourcegateway/internal/policy"
	"github.com/sourcegateway/sourcegateway/internal/routing"
)

// gitHandler serves Git's smart HTTP protocol from managed mirrors.
type gitHandler struct {
	registry *routing.Registry
	logger   *slog.Logger
	tmpDir   string
}

// hookContext carries trusted push policy and repository state into the receive hook.
type hookContext struct {
	Repository string                          `json:"repository"`
	Upstream   string                          `json:"upstream"`
	AuthHeader string                          `json:"authHeader"`
	Namespace  string                          `json:"namespace"`
	Name       string                          `json:"name"`
	UID        string                          `json:"uid"`
	Rules      []v1alpha1.GitAuthorizationRule `json:"rules"`
	AuditPath  string                          `json:"auditPath"`
}

// hookAudit records one push authorization decision for the HTTP request log.
type hookAudit struct {
	Allowed bool     `json:"allowed"`
	Reason  string   `json:"reason"`
	Matched []string `json:"matched,omitempty"`
}

// ServeHTTP validates and dispatches a Git smart HTTP request.
func (h *gitHandler) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	setTransferDeadline(response, request, 10*time.Minute)
	suffix, source := h.findSource(request.URL.Path)
	if source == nil {
		http.NotFound(response, request)
		return
	}
	release, acquired := source.Requests.Acquire()
	if !acquired {
		http.Error(response, "repository unavailable", http.StatusServiceUnavailable)
		return
	}
	defer release()
	receiving := request.Method == http.MethodPost && suffix == "/git-receive-pack"
	if receiving {
		source.State.Update.Lock()
		defer source.State.Update.Unlock()
	}
	source.State.Gate.RLock()
	defer source.State.Gate.RUnlock()
	if _, err := os.Stat(filepath.Join(source.MirrorPath, routing.GitRefreshMarker)); err == nil {
		http.Error(response, "repository unavailable", http.StatusServiceUnavailable)
		return
	}
	if time.Since(source.RefreshedAt) > source.MaxStaleness {
		http.Error(response, "repository unavailable", http.StatusServiceUnavailable)
		return
	}
	service := request.URL.Query().Get("service")
	switch {
	case request.Method == http.MethodGet && suffix == "/info/refs" && (service == "git-upload-pack" || service == "git-receive-pack"):
		if !h.readAllowed(response, request, source) {
			return
		}
		h.advertise(response, request, source, service)
	case request.Method == http.MethodPost && (suffix == "/git-upload-pack" || suffix == "/git-receive-pack"):
		service = strings.TrimPrefix(suffix, "/")
		if service == "git-upload-pack" && !h.readAllowed(response, request, source) {
			return
		}
		h.rpc(response, request, source, service)
	default:
		http.Error(response, "unsupported Git protocol request", http.StatusNotFound)
	}
}

// findSource resolves a request path to its registered Git source and protocol suffix.
func (h *gitHandler) findSource(requestPath string) (string, *routing.GitSource) {
	for route, source := range h.registry.Load().Git {
		if strings.HasPrefix(requestPath, route+"/") {
			return strings.TrimPrefix(requestPath, route), source
		}
	}
	return "", nil
}

// readAllowed authorizes and audits repository reads.
func (h *gitHandler) readAllowed(response http.ResponseWriter, request *http.Request, source *routing.GitSource) bool {
	identity, _ := auth.IdentityFromContext(request.Context())
	allowed, matched, err := source.Policy.Authorize(policy.ServiceAccount{Namespace: identity.Namespace, Name: identity.Name, UID: identity.UID}, policy.Request{Operation: "git.read"})
	h.audit(identity, source, "git.read", allowed && err == nil, matched, err)
	if err != nil || !allowed {
		http.Error(response, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// advertise writes the requested Git service's reference advertisement.
func (h *gitHandler) advertise(response http.ResponseWriter, request *http.Request, source *routing.GitSource, service string) {
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, service, "--stateless-rpc", "--advertise-refs", source.MirrorPath) // #nosec G204,G702 -- service is allowlisted above and mirror paths are controller-owned.
	ConfigureGitCommand(command, gitProtocolEnvironment(request)...)
	output, err := command.Output()
	if err != nil {
		http.Error(response, "repository unavailable", http.StatusServiceUnavailable)
		return
	}
	response.Header().Set("Content-Type", "application/x-"+service+"-advertisement")
	response.Header().Set("Cache-Control", "no-cache")
	serviceAnnouncement := "# service=" + service + "\n"
	_, _ = fmt.Fprintf(response, "%04x%s0000", len(serviceAnnouncement)+4, serviceAnnouncement) // #nosec G705 -- this is a Git protocol packet, not HTML.
	_, _ = response.Write(output)                                                               // #nosec G705 -- git-upload-pack/receive-pack emits the required binary protocol response.
}

// rpc runs an allowlisted stateless Git service for one request.
func (h *gitHandler) rpc(response http.ResponseWriter, request *http.Request, source *routing.GitSource, service string) {
	if request.Header.Get("Content-Type") != "application/x-"+service+"-request" {
		http.Error(response, "invalid content type", http.StatusUnsupportedMediaType)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, service, "--stateless-rpc", source.MirrorPath) // #nosec G204,G702 -- service is allowlisted above and mirror paths are controller-owned.
	commandEnvironment := gitProtocolEnvironment(request)
	command.Stdin = request.Body
	var stderr bytes.Buffer
	command.Stderr = &stderr
	auditPath := ""
	identity := auth.Identity{}
	if service == "git-receive-pack" {
		// Pass policy to the hook through an isolated environment and return audits by file.
		auditFile, err := os.CreateTemp(h.tmpDir, "git-audit-*")
		if err != nil {
			http.Error(response, "internal error", http.StatusInternalServerError)
			return
		}
		auditPath = auditFile.Name()
		_ = auditFile.Close()
		_ = os.Remove(auditPath)
		defer func() { _ = os.Remove(auditPath) }()
		identity, _ = auth.IdentityFromContext(request.Context())
		encoded, err := json.Marshal(hookContext{
			Repository: source.MirrorPath, Upstream: source.UpstreamURL, AuthHeader: source.AuthHeader,
			Namespace: identity.Namespace, Name: identity.Name, UID: identity.UID, Rules: source.Rules, AuditPath: auditPath,
		})
		if err != nil {
			http.Error(response, "internal error", http.StatusInternalServerError)
			return
		}
		commandEnvironment = append(commandEnvironment, "SOURCEGATEWAY_HOOK_CONTEXT="+base64.RawStdEncoding.EncodeToString(encoded))
	}
	ConfigureGitCommand(command, commandEnvironment...)
	response.Header().Set("Content-Type", "application/x-"+service+"-result")
	response.Header().Set("Cache-Control", "no-cache")
	command.Stdout = response
	if err := command.Run(); err != nil {
		h.logger.Warn("Git RPC failed", "service", service, "class", "upstream_or_protocol")
	}
	if service == "git-receive-pack" {
		h.logPushAudits(identity, source, auditPath)
		if _, err := os.Stat(filepath.Join(source.MirrorPath, routing.GitRefreshMarker)); err == nil { // #nosec G703 -- the controller owns and validates mirror paths.
			h.registry.RequestGitRefresh()
		}
	}
}

// InstallProcReceiveHook configures a bare repository to delegate all ref updates to this executable.
func InstallProcReceiveHook(repository string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	configuration := exec.Command("git", "-C", repository, "config", "--replace-all", "receive.procReceiveRefs", "refs/") // #nosec G204 -- repository is controller-owned.
	ConfigureGitCommand(configuration)
	if err := configuration.Run(); err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(repository, "hooks", "pre-receive"))
	hook := filepath.Join(repository, "hooks", "proc-receive")
	_ = os.Remove(hook)
	return os.Symlink(executable, hook)
}

// RunProcReceiveHook authorizes an atomic push before forwarding it upstream.
func RunProcReceiveHook(ctx context.Context, input io.Reader, output io.Writer) error {
	encoded := os.Getenv("SOURCEGATEWAY_HOOK_CONTEXT")
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("missing hook context")
	}
	var hook hookContext
	if err := json.Unmarshal(raw, &hook); err != nil {
		return errors.New("invalid hook context")
	}
	compiled, err := policy.CompileGit(hook.Rules)
	if err != nil {
		return errors.New("policy unavailable")
	}
	if err := negotiateProcReceive(input, output); err != nil {
		return err
	}
	commands, err := readRefCommands(input)
	if err != nil {
		return err
	}

	// Validate the complete command set before making any upstream request.
	audits := make([]hookAudit, len(commands))
	valid := true
	for index := range commands {
		matched, reason, validationErr := validateRefCommand(ctx, hook, compiled, &commands[index])
		audits[index] = hookAudit{Allowed: validationErr == nil, Reason: reason, Matched: matched}
		valid = valid && validationErr == nil
	}
	writeHookAudits(hook.AuditPath, audits)
	if !valid {
		return writeRefResults(output, commands, refRejected)
	}
	if err := pushRefCommands(ctx, hook, commands); err != nil {
		matched, reconcileErr := upstreamRefsMatch(ctx, hook, commands)
		if reconcileErr != nil || !matched {
			_ = os.WriteFile(filepath.Join(hook.Repository, routing.GitRefreshMarker), []byte("refresh required\n"), 0o600)
			return writeRefResults(output, commands, refUnknown)
		}
	}
	if err := updateLocalRefs(ctx, hook.Repository, commands); err != nil {
		_ = os.WriteFile(filepath.Join(hook.Repository, routing.GitRefreshMarker), []byte("refresh required\n"), 0o600)
	}
	return writeRefResults(output, commands, refAccepted)
}

// negotiateProcReceive establishes the supported Git proc-receive protocol version.
func negotiateProcReceive(input io.Reader, output io.Writer) error {
	version, flush, err := readPacket(input)
	if err != nil || flush || !strings.HasPrefix(string(version), "version=1") {
		return errors.New("unsupported proc-receive protocol")
	}
	if _, flush, err = readPacket(input); err != nil || !flush {
		return errors.New("malformed proc-receive negotiation")
	}
	if err := writePacket(output, []byte("version=1")); err != nil {
		return err
	}
	if _, err := io.WriteString(output, "0000"); err != nil {
		return err
	}
	return nil
}

// readRefCommands buffers one complete atomic set of reference transitions.
func readRefCommands(input io.Reader) ([]refCommand, error) {
	commands := make([]refCommand, 0)
	for {
		packet, packetFlush, packetErr := readPacket(input)
		if packetErr != nil {
			return nil, packetErr
		}
		if packetFlush {
			break
		}
		fields := strings.Fields(string(packet))
		if len(fields) != 3 {
			return nil, errors.New("malformed ref command")
		}
		commands = append(commands, refCommand{old: fields[0], new: fields[1], ref: fields[2]})
	}
	if len(commands) == 0 {
		return nil, errors.New("no valid ref commands")
	}
	return commands, nil
}

// pushRefCommands atomically forwards validated reference transitions upstream.
func pushRefCommands(ctx context.Context, hook hookContext, commands []refCommand) error {
	// Pin transport security settings and avoid all ambient credential helpers.
	arguments := gitTransportArguments(hook)
	arguments = append(arguments, "push", "--atomic")
	for _, command := range commands {
		expected := command.old
		if isZeroOID(expected) {
			expected = ""
		}
		arguments = append(arguments, "--force-with-lease="+command.ref+":"+expected)
	}
	arguments = append(arguments, hook.Upstream)
	for _, command := range commands {
		if isZeroOID(command.new) {
			arguments = append(arguments, ":"+command.ref)
		} else {
			arguments = append(arguments, command.new+":"+command.ref)
		}
	}
	push := exec.CommandContext(ctx, "git", arguments...) // #nosec G204 -- validated object IDs/refs and controller-owned upstream are passed without a shell.
	ConfigureGitCommand(push)
	push.Stdout, push.Stderr = io.Discard, io.Discard
	return push.Run()
}

// upstreamRefsMatch checks whether every requested post-push ref state is visible upstream.
func upstreamRefsMatch(ctx context.Context, hook hookContext, commands []refCommand) (bool, error) {
	arguments := append(gitTransportArguments(hook), "ls-remote", "--refs", hook.Upstream)
	for _, command := range commands {
		arguments = append(arguments, command.ref)
	}
	query := exec.CommandContext(ctx, "git", arguments...) // #nosec G204 -- refs are validated and the upstream is controller-owned.
	ConfigureGitCommand(query)
	output, err := query.Output()
	if err != nil {
		return false, err
	}
	return refCommandsMatch(commands, string(output)), nil
}

// refCommandsMatch compares ls-remote output with one atomic requested result.
func refCommandsMatch(commands []refCommand, output string) bool {
	upstream := make(map[string]string, len(commands))
	for line := range strings.Lines(output) {
		oid, ref, found := strings.Cut(strings.TrimSpace(line), "\t")
		if found {
			upstream[ref] = oid
		}
	}
	for _, command := range commands {
		if isZeroOID(command.new) {
			if upstream[command.ref] != "" {
				return false
			}
		} else if upstream[command.ref] != command.new {
			return false
		}
	}
	return true
}

// gitTransportArguments returns pinned transport configuration without a subcommand.
func gitTransportArguments(hook hookContext) []string {
	arguments := []string{"-C", hook.Repository, "-c", "http.followRedirects=false", "-c", "http.sslVerify=true", "-c", "credential.helper="}
	if hook.AuthHeader != "" {
		arguments = append(arguments, "-c", "http.extraHeader=Authorization: "+hook.AuthHeader)
	}
	return arguments
}

// refCommand describes one proposed reference transition.
type refCommand struct{ old, new, ref string }

// refResult describes what Source Gateway knows about an upstream ref mutation.
type refResult string

const (
	refAccepted refResult = "accepted"
	refRejected refResult = "rejected"
	refUnknown  refResult = "unknown"
)

// validateRefCommand validates object identity, update shape, and push policy.
func validateRefCommand(ctx context.Context, hook hookContext, compiled *policy.Policy, command *refCommand) ([]string, string, error) {
	if !strings.HasPrefix(command.ref, "refs/heads/") || strings.TrimPrefix(command.ref, "refs/heads/") == "" {
		return nil, "invalid push", errors.New("only branches may be pushed")
	}
	if len(command.old) != 40 && len(command.old) != 64 || len(command.new) != len(command.old) {
		return nil, "invalid push", errors.New("malformed object ID")
	}
	if _, err := hex.DecodeString(command.old); err != nil {
		return nil, "invalid push", errors.New("malformed object ID")
	}
	if _, err := hex.DecodeString(command.new); err != nil {
		return nil, "invalid push", errors.New("malformed object ID")
	}
	create, deleteRef := isZeroOID(command.old), isZeroOID(command.new)
	if !create {
		current, err := gitOutput(ctx, hook.Repository, "rev-parse", command.ref)
		if err != nil || current != command.old {
			return nil, "invalid push", errors.New("stale old object ID")
		}
	}
	force := false
	if !deleteRef {
		kind, err := gitOutput(ctx, hook.Repository, "cat-file", "-t", command.new)
		if err != nil || kind != "commit" {
			return nil, "invalid push", errors.New("branch target must be a commit")
		}
		if !create {
			ancestry := exec.CommandContext(ctx, "git", "-C", hook.Repository, "merge-base", "--is-ancestor", command.old, command.new) // #nosec G204 -- repository is controller-owned and object IDs are hex validated.
			ConfigureGitCommand(ancestry)
			force = ancestry.Run() != nil
		}
	}
	allowed, matched, err := compiled.Authorize(policy.ServiceAccount{Namespace: hook.Namespace, Name: hook.Name, UID: hook.UID}, policy.Request{
		Operation: "git.push", Branch: strings.TrimPrefix(command.ref, "refs/heads/"), Create: create, Delete: deleteRef, Force: force,
	})
	if err != nil {
		return matched, "evaluation failure", errors.New("policy evaluation failed")
	}
	if !allowed {
		return matched, "no matching rule", errors.New("policy denied push")
	}
	return matched, "matched", nil
}

// readPacket reads one Git pkt-line and reports flush packets separately.
func readPacket(input io.Reader) ([]byte, bool, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(input, header); err != nil {
		return nil, false, err
	}
	if string(header) == "0000" {
		return nil, true, nil
	}
	length, err := strconv.ParseUint(string(header), 16, 16)
	if err != nil || length < 4 || length > 65520 {
		return nil, false, errors.New("invalid packet length")
	}
	payload := make([]byte, int(length)-4)
	if _, err := io.ReadFull(input, payload); err != nil {
		return nil, false, err
	}
	return payload, false, nil
}

// writePacket writes one length-prefixed Git pkt-line.
func writePacket(output io.Writer, payload []byte) error {
	if len(payload)+4 > 65520 {
		return errors.New("packet too large")
	}
	_, err := fmt.Fprintf(output, "%04x%s", len(payload)+4, payload)
	return err
}

// writeRefResults reports a uniform result for each command and terminates the response.
func writeRefResults(output io.Writer, commands []refCommand, result refResult) error {
	for _, command := range commands {
		message := "ng " + command.ref + " push rejected"
		switch result {
		case refAccepted:
			message = "ok " + command.ref
		case refUnknown:
			message = "ng " + command.ref + " push outcome unknown; inspect upstream before retrying"
		}
		if err := writePacket(output, []byte(message)); err != nil {
			return err
		}
	}
	_, err := io.WriteString(output, "0000")
	return err
}

// updateLocalRefs atomically brings the mirror's refs in line with a successful push.
func updateLocalRefs(ctx context.Context, repository string, commands []refCommand) error {
	var input strings.Builder
	for _, command := range commands {
		if isZeroOID(command.new) {
			_, _ = fmt.Fprintf(&input, "delete %s %s\n", command.ref, command.old)
		} else {
			_, _ = fmt.Fprintf(&input, "update %s %s %s\n", command.ref, command.new, command.old)
		}
	}
	update := exec.CommandContext(ctx, "git", "-C", repository, "update-ref", "--stdin") // #nosec G204 -- repository and validated ref commands are controller-owned.
	ConfigureGitCommand(update)
	update.Stdin = strings.NewReader(input.String())
	update.Stdout, update.Stderr = io.Discard, io.Discard
	return update.Run()
}

// writeHookAudits returns hook decisions to the serving process through its private file.
func writeHookAudits(path string, audits []hookAudit) {
	encoded, err := json.Marshal(audits)
	if err == nil && path != "" {
		_ = os.WriteFile(path, encoded, 0o600)
	}
}

// logPushAudits emits bounded, structured authorization records from a receive hook.
func (h *gitHandler) logPushAudits(identity auth.Identity, source *routing.GitSource, path string) {
	encoded, err := os.ReadFile(path) // #nosec G304 -- path is created by this handler in its private temporary directory.
	if err != nil || len(encoded) > 1<<20 {
		return
	}
	var audits []hookAudit
	if json.Unmarshal(encoded, &audits) != nil {
		return
	}
	for _, audit := range audits {
		h.logger.Info("authorization decision", "service_account_namespace", identity.Namespace, "service_account_name", identity.Name,
			"service_account_uid", identity.UID, "source_namespace", source.Namespace, "source_name", source.Name, "operation", "git.push",
			"policy_revision", source.Policy.Revision(), "allowed", audit.Allowed, "reason", audit.Reason, "matched_rules", audit.Matched)
	}
}

// gitOutput runs a non-interactive Git query and trims its output.
func gitOutput(ctx context.Context, repository string, arguments ...string) (string, error) {
	args := append([]string{"-C", repository}, arguments...)
	command := exec.CommandContext(ctx, "git", args...) // #nosec G204 -- callers provide fixed Git subcommands with validated object IDs/refs.
	ConfigureGitCommand(command)
	output, err := command.Output()
	return strings.TrimSpace(string(output)), err
}

// isZeroOID reports whether an object ID is Git's all-zero sentinel.
func isZeroOID(oid string) bool { return strings.Trim(oid, "0") == "" }

// gitProtocolEnvironment forwards an explicitly requested Git protocol version.
func gitProtocolEnvironment(request *http.Request) []string {
	if protocol := request.Header.Get("Git-Protocol"); protocol != "" {
		return []string{"GIT_PROTOCOL=" + protocol}
	}
	return nil
}

// ConfigureGitCommand prevents ambient process configuration from changing
// Git's authentication or TLS behavior while preserving receive-pack's
// quarantine environment for hook subprocesses.
func ConfigureGitCommand(command *exec.Cmd, extra ...string) {
	environment := make([]string, 0, len(os.Environ())+len(extra)+2)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if blockedGitEnvironment(name) {
			continue
		}
		environment = append(environment, value)
	}
	environment = append(environment, "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never")
	command.Env = append(environment, extra...)
}

// blockedGitEnvironment reports whether ambient state could alter Git transport security.
func blockedGitEnvironment(name string) bool {
	if strings.HasPrefix(name, "GIT_CONFIG_") {
		return true
	}
	switch name {
	case "GCM_INTERACTIVE", "GIT_ASKPASS", "GIT_PROTOCOL", "GIT_PROXY_COMMAND", "GIT_SSH", "GIT_SSH_COMMAND",
		"GIT_SSL_NO_VERIFY", "GIT_TERMINAL_PROMPT", "SOURCEGATEWAY_HOOK_CONTEXT", "SSH_ASKPASS":
		return true
	default:
		return false
	}
}

// audit logs a redacted Git authorization decision.
func (h *gitHandler) audit(identity auth.Identity, source *routing.GitSource, operation string, allowed bool, matched []string, evaluationError error) {
	reason := "no matching rule"
	if evaluationError != nil {
		reason = "evaluation failure"
	} else if allowed {
		reason = "matched"
	}
	h.logger.Info("authorization decision", "service_account_namespace", identity.Namespace, "service_account_name", identity.Name,
		"service_account_uid", identity.UID, "source_namespace", source.Namespace, "source_name", source.Name, "operation", operation,
		"policy_revision", source.Policy.Revision(), "allowed", allowed, "reason", reason, "matched_rules", matched)
}
