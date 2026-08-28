package tools

import (
	"path/filepath"
	"strings"

	"nudgebee/llm/security"
	"nudgebee/llm/tools/core"

	"github.com/google/shlex"
)

// This file wires shell_execute into the per-tool confirmation-gate framework
// so `shell_execute("kubectl delete deploy X")` requires the same user
// approval as `kubectl_execute("kubectl delete deploy X")`. Without this,
// shell became a bypass channel for every gated CLI (kubectl / aws / gcloud /
// az / gh / glab / helm / argocd / etc.): the LLM could invoke a mutation
// via shell and skip the approval dialog the direct-tool path enforces.
//
// The dispatch is DYNAMIC and interface-driven — a new CLI tool that
// implements ShellWrappable is auto-picked up. The expected-prefix test in
// tool_shell_gate_test.go is the review checklist for newly supported CLIs.
//
// Threat model reminder: workspace pods are already scoped per-account
// (containment layer). This gate is the CONSENT layer — enforcing that the
// user approves each destructive action before it runs. Both are needed:
// containment bounds the blast radius; consent gives the user a veto.

// InferToolRequestType classifies the intent of a shell command by dispatching
// to the wrapped CLI tool's own classifier. The leading command token is
// matched against every registered ShellWrappable tool's declared prefixes;
// the first match owns the classification. Non-CLI-wrapper commands
// (grep, jq, awk, cat, tail, find, unmatched binaries) fall through as
// unclassified — treated as read by the auth gate.
//
// The delegate classifier is called with the CLEANED command (env-var
// assignments and sudo/env wrappers stripped) — not the raw input — so a
// direct-tool classifier written to expect `kubectl ...` still fires
// correctly when the LLM invoked `sudo env KUBECONFIG=/x kubectl ...` via
// shell_execute. Without this, unwrapping the wrapper for lookup but passing
// raw input to the classifier would misclassify or fail-open.
//
// Expressions that cannot be reduced to one registered leading command but
// contain a registered CLI anywhere are conservatively classified as updates.
// This sends compound commands and shell wrappers through confirmation rather
// than allowing an ambiguous expression to fail open.
//
// Static classifier (this method) fires first per auth_agent.go's contract;
// InferToolRequestTypePrompt (below) provides LLM-based fallback for CLIs
// whose classifier is prompt-based rather than heuristic.
func (m ShellTool) InferToolRequestType(ctx *security.RequestContext, toolName, input string) (core.ToolRequestType, error) {
	input = extractCommandFromToolInput(input)
	if hasShellGateControlSyntax(input) {
		readOnly, err := m.isKnownReadOnlyCompound(ctx, input)
		if err != nil {
			return "", err
		}
		if readOnly {
			return core.ToolRequestTypeRead, nil
		}
		if containsExecutableShellWrappable(input) {
			return core.ToolRequestTypeUpdate, nil
		}
	}
	stages := splitShellPipeline(input)
	for _, stage := range stages[1:] {
		lead, _ := extractLeadingShellCommand(stage)
		leadBase := filepath.Base(lead)
		if core.LookupShellWrappable(leadBase) != "" ||
			((lead == "" || isShellExecutionWrapper(leadBase) || !isShellArgumentOnlyUtility(leadBase) ||
				hasShellSubstitution(stage)) && containsShellWrappableCommand(stage)) {
			// A registered CLI later in a pipeline is executable, but classifying
			// it independently could miss state passed through stdin. Fail closed.
			return core.ToolRequestTypeUpdate, nil
		}
	}
	lead, cleaned := extractLeadingShellCommand(stages[0])
	if lead == "" {
		if containsShellWrappableCommand(input) {
			return core.ToolRequestTypeUpdate, nil
		}
		return "", nil
	}
	ownerName := core.LookupShellWrappable(filepath.Base(lead))
	if ownerName == "" {
		leadBase := filepath.Base(lead)
		// Known argument-only utilities may safely contain a registered CLI name
		// as data. Wrappers and other unknown commands fail closed.
		if (isShellExecutionWrapper(leadBase) || !isShellArgumentOnlyUtility(leadBase) ||
			hasShellSubstitution(input)) && containsShellWrappableCommand(input) {
			return core.ToolRequestTypeUpdate, nil
		}
		return "", nil
	}
	tool, ok := core.GetNBTool(m.AccountId, ownerName)
	if !ok {
		return "", nil
	}
	classifier, ok := tool.(core.ToolRequestInference)
	if !ok {
		// Target tool is shell-wrappable but has no static classifier — return
		// "" so auth_agent's LLM-classifier fallback path fires via
		// InferToolRequestTypePrompt below.
		return "", nil
	}
	return classifier.InferToolRequestType(ctx, tool.Name(), cleaned)
}

func (m ShellTool) isKnownReadOnlyCompound(ctx *security.RequestContext, input string) (bool, error) {
	for _, segment := range splitShellCommandSegments(input) {
		for _, stage := range splitShellPipeline(segment) {
			stage = strings.TrimSpace(stage)
			if stage == "" {
				continue
			}
			if hasShellSubstitution(stage) && containsShellWrappableCommand(stage) {
				return false, nil
			}
			lead, cleaned := extractLeadingShellCommand(stripShellRedirections(stage))
			leadBase := filepath.Base(lead)
			if lead == "" {
				tokens, err := shlex.Split(stage)
				if err == nil && len(tokens) == 1 && tokens[0] == "env" {
					continue
				}
				if containsShellWrappableCommand(stage) {
					return false, nil
				}
				continue
			}
			ownerName := core.LookupShellWrappable(leadBase)
			if ownerName == "" {
				if leadBase == "curl" && !isKnownReadOnlyFallbackCommand(cleaned) {
					return false, nil
				}
				if (isShellExecutionWrapper(leadBase) || hasShellSubstitution(stage) ||
					!isShellArgumentOnlyUtility(leadBase)) && containsShellWrappableCommand(stage) {
					return false, nil
				}
				continue
			}
			tool, ok := core.GetNBTool(m.AccountId, ownerName)
			if !ok {
				return false, nil
			}
			if classifier, ok := tool.(core.ToolRequestInference); ok {
				requestType, err := classifier.InferToolRequestType(ctx, tool.Name(), cleaned)
				if err != nil {
					return false, err
				}
				if requestType != core.ToolRequestTypeRead {
					return false, nil
				}
				continue
			}
			if leadBase != "gh" || !isKnownReadOnlyGHCommand(cleaned) {
				return false, nil
			}
		}
	}
	return true, nil
}

func stripShellRedirections(input string) string {
	var singleQuoted, doubleQuoted, escaped bool
	lastOperatorIdx := -1
	for i := 0; i < len(input); i++ {
		char := input[i]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if char == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if char == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if !singleQuoted && !doubleQuoted && (char == '>' || char == '<') {
			lastOperatorIdx = i
		}
	}
	if lastOperatorIdx >= 0 {
		if !shellRedirectionEndsStage(input, lastOperatorIdx) {
			return input
		}
		end := lastOperatorIdx
		if end > 0 && input[end-1] == '&' {
			end--
		}
		if end > 0 && input[end-1] >= '0' && input[end-1] <= '9' {
			end--
		}
		return stripShellRedirections(strings.TrimSpace(input[:end]))
	}
	return input
}

func shellRedirectionEndsStage(input string, operatorIdx int) bool {
	i := operatorIdx + 1
	if i < len(input) && input[i] == '>' {
		i++
	}
	for i < len(input) && (input[i] == ' ' || input[i] == '\t') {
		i++
	}
	if i < len(input) && input[i] == '&' {
		i++
		for i < len(input) && ((input[i] >= '0' && input[i] <= '9') || input[i] == '-') {
			i++
		}
	} else {
		var quote byte
		escaped := false
		for i < len(input) {
			char := input[i]
			if escaped {
				escaped = false
				i++
				continue
			}
			if char == '\\' && quote != '\'' {
				escaped = true
				i++
				continue
			}
			if quote != 0 {
				if char == quote {
					quote = 0
				}
				i++
				continue
			}
			if char == '\'' || char == '"' {
				quote = char
				i++
				continue
			}
			if char == ' ' || char == '\t' {
				break
			}
			i++
		}
	}
	for i < len(input) && (input[i] == ' ' || input[i] == '\t') {
		i++
	}
	return i == len(input)
}

// containsExecutableShellWrappable inspects executable positions in compound
// expressions. Unlike containsShellWrappableCommand, it does not treat command
// arguments (such as a grep regex containing "clickhouse") as executables.
func containsExecutableShellWrappable(input string) bool {
	for _, segment := range splitShellCommandSegments(input) {
		for _, stage := range splitShellPipeline(segment) {
			lead, _ := extractLeadingShellCommand(stage)
			if core.LookupShellWrappable(filepath.Base(lead)) != "" {
				return true
			}
			leadBase := filepath.Base(lead)
			if lead == "" || isShellExecutionWrapper(leadBase) ||
				!isShellArgumentOnlyUtility(leadBase) || hasShellSubstitution(stage) {
				// Preserve fail-closed handling for shell wrappers, substitutions,
				// groups, and executable-valued variable assignments.
				if containsShellWrappableCommand(stage) {
					return true
				}
			}
		}
	}
	return false
}

// These utilities treat following tokens as data rather than executable
// commands. Keep this deliberately narrow: awk, sed, find, xargs, shells, and
// similar tools can execute arguments and therefore must use the fail-closed
// fallback when a registered CLI appears later in the stage.
var shellArgumentOnlyUtilities = map[string]struct{}{
	"cat": {}, "cut": {}, "echo": {}, "egrep": {}, "fgrep": {}, "grep": {},
	"head": {}, "jq": {}, "od": {}, "printf": {}, "rgrep": {}, "sort": {},
	"tail": {}, "tr": {}, "uniq": {}, "wc": {}, "xxd": {},
}

func isShellArgumentOnlyUtility(command string) bool {
	_, ok := shellArgumentOnlyUtilities[command]
	return ok
}

func isShellExecutionWrapper(command string) bool {
	switch command {
	case "command", "exec", "nohup", "nice", "time", "timeout", "watch", "xargs":
		return true
	default:
		return false
	}
}

func hasShellSubstitution(input string) bool {
	return strings.Contains(input, "$(") || strings.Contains(input, "`") ||
		strings.Contains(input, "<(") || strings.Contains(input, ">(")
}

func splitShellCommandSegments(input string) []string {
	var parts []string
	start := 0
	var singleQuoted, doubleQuoted, escaped bool
	for i := 0; i < len(input); i++ {
		char := input[i]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if char == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if char == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if singleQuoted || doubleQuoted {
			continue
		}
		separatorLen := 0
		switch char {
		case ';', '\n', '\r':
			separatorLen = 1
		case '&':
			if i+1 < len(input) && input[i+1] == '&' {
				separatorLen = 2
			} else if !isShellRedirectionAmpersand(input, i) {
				// Do not split descriptor or combined-output redirections.
				separatorLen = 1
			}
		case '|':
			if i+1 < len(input) && input[i+1] == '|' {
				separatorLen = 2
			}
		}
		if separatorLen > 0 {
			parts = append(parts, strings.TrimSpace(input[start:i]))
			i += separatorLen - 1
			start = i + 1
		}
	}
	if singleQuoted || doubleQuoted || escaped {
		return []string{input}
	}
	parts = append(parts, strings.TrimSpace(input[start:]))
	return parts
}

func isShellRedirectionAmpersand(input string, i int) bool {
	return (i > 0 && (input[i-1] == '>' || input[i-1] == '<')) ||
		(i+1 < len(input) && input[i+1] == '>')
}

// hasShellGateControlSyntax reports syntax that can execute more than one
// command or hide a nested command. Delegating only the leading verb in these
// cases can classify a benign first segment as read while a later segment
// mutates state.
func hasShellGateControlSyntax(input string) bool {
	var singleQuoted, doubleQuoted, escaped bool
	for i := 0; i < len(input); i++ {
		char := input[i]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if char == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if char == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if singleQuoted {
			continue
		}
		if char == '`' || (char == '$' && i+1 < len(input) && input[i+1] == '(') {
			return true
		}
		if !doubleQuoted && char == '|' && i+1 < len(input) && input[i+1] == '|' {
			return true
		}
		if !doubleQuoted && char == '&' {
			if !isShellRedirectionAmpersand(input, i) {
				return true
			}
		} else if !doubleQuoted && strings.ContainsRune(";\n\r(){}", rune(char)) {
			return true
		}
	}
	return singleQuoted || doubleQuoted || escaped
}

// InferToolRequestTypePrompt is the LLM-classifier fallback used by
// auth_agent.go when InferToolRequestType returned "" (either the shell
// command was non-CLI, or the matched CLI has no static classifier).
// Dispatches to the matched CLI tool's LLM classifier so shell classification
// exactly matches direct-tool classification.
//
// Passes the CLEANED command (not the raw input) for the same reason as the
// static classifier above — see InferToolRequestType's doc.
//
// If no match, returns "" (empty prompt) — auth_agent treats this as
// "unclassifiable", the action runs without a confirmation gate. That matches
// the pre-fix behavior for the "not a CLI wrap" case (grep, jq, etc.) — no
// regression on those.
func (m ShellTool) InferToolRequestTypePrompt(ctx *security.RequestContext, toolName, input string) (string, error) {
	input = extractCommandFromToolInput(input)
	if hasShellGateControlSyntax(input) {
		readOnly, err := m.isKnownReadOnlyCompound(ctx, input)
		if err != nil {
			return "", err
		}
		if readOnly {
			input = firstRegisteredShellStage(input)
		}
	}
	lead, cleaned := extractLeadingShellCommand(splitShellPipeline(input)[0])
	if lead == "" {
		return "", nil
	}
	ownerName := core.LookupShellWrappable(filepath.Base(lead))
	if ownerName == "" {
		return "", nil
	}
	tool, ok := core.GetNBTool(m.AccountId, ownerName)
	if !ok {
		return "", nil
	}
	classifier, ok := tool.(core.ToolRequestInferencePrompt)
	if !ok {
		return "", nil
	}
	return classifier.InferToolRequestTypePrompt(ctx, tool.Name(), cleaned)
}

func firstRegisteredShellStage(input string) string {
	for _, segment := range splitShellCommandSegments(input) {
		for _, stage := range splitShellPipeline(segment) {
			lead, _ := extractLeadingShellCommand(stage)
			if core.LookupShellWrappable(filepath.Base(lead)) != "" {
				return stage
			}
		}
	}
	return input
}

// splitShellFallbacks separates unquoted logical-OR fallback commands. A
// fallback chain is treated specially only when every branch is a narrowly
// recognized read operation; all other compound expressions still fail closed.
func splitShellFallbacks(input string) []string {
	var parts []string
	start := 0
	var singleQuoted, doubleQuoted, escaped bool
	for i := 0; i < len(input); i++ {
		char := input[i]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if char == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if char == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if char == '|' && i+1 < len(input) && input[i+1] == '|' && !singleQuoted && !doubleQuoted {
			parts = append(parts, strings.TrimSpace(input[start:i]))
			i++
			start = i + 1
		}
	}
	if singleQuoted || doubleQuoted || escaped {
		return []string{input}
	}
	parts = append(parts, strings.TrimSpace(input[start:]))
	return parts
}

func isKnownReadOnlyFallbackChain(parts []string) bool {
	for _, part := range parts {
		if !isKnownReadOnlyFallbackCommand(part) {
			return false
		}
	}
	return true
}

func isKnownReadOnlyFallbackCommand(input string) bool {
	tokens, err := shlex.Split(input)
	if err != nil || len(tokens) == 0 {
		return false
	}
	switch filepath.Base(tokens[0]) {
	case "echo":
		return true
	case "curl":
		for _, token := range tokens[1:] {
			if hasShellMutationFlag(token, "-d", "--data", "--data-raw", "--data-binary",
				"-F", "--form", "-T", "--upload-file", "-X", "--request") {
				return false
			}
		}
		return true
	case "gh":
		return isKnownReadOnlyGHCommand(input)
	default:
		return false
	}
}

func isKnownReadOnlyGHCommand(input string) bool {
	tokens, err := shlex.Split(input)
	if err != nil || len(tokens) < 2 || filepath.Base(tokens[0]) != "gh" {
		return false
	}
	if len(tokens) >= 3 && tokens[1] == "run" && tokens[2] == "view" {
		return true
	}
	if tokens[1] != "api" {
		return false
	}
	for _, token := range tokens[2:] {
		if hasShellMutationFlag(token, "-X", "--method", "-f", "--raw-field",
			"-F", "--field", "--input") {
			return false
		}
	}
	return true
}

func hasShellMutationFlag(token string, flags ...string) bool {
	for _, flag := range flags {
		if token == flag || strings.HasPrefix(token, flag+"=") ||
			(len(flag) == 2 && strings.HasPrefix(token, flag) && len(token) > len(flag)) {
			return true
		}
	}
	return false
}

// splitShellPipeline separates unquoted pipeline stages. Unlike compound
// operators, a pipe does not make a read-only leading CLI destructive; common
// inspection commands such as `kubectl get ... | grep ...` should retain the
// leading CLI's classification. Quoted pipes and logical OR (`||`) are not
// separators. Malformed quoting is left intact so the existing conservative
// bailout path handles it.
func splitShellPipeline(input string) []string {
	var stages []string
	start := 0
	var singleQuoted, doubleQuoted, escaped bool
	for i := 0; i < len(input); i++ {
		char := input[i]
		if escaped {
			escaped = false
			continue
		}
		if char == '\\' && !singleQuoted {
			escaped = true
			continue
		}
		if char == '\'' && !doubleQuoted {
			singleQuoted = !singleQuoted
			continue
		}
		if char == '"' && !singleQuoted {
			doubleQuoted = !doubleQuoted
			continue
		}
		if char == '|' && !singleQuoted && !doubleQuoted &&
			(i == 0 || input[i-1] != '|') && (i+1 == len(input) || input[i+1] != '|') {
			stages = append(stages, strings.TrimSpace(input[start:i]))
			start = i + 1
		}
	}
	if singleQuoted || doubleQuoted || escaped {
		return []string{input}
	}
	stages = append(stages, strings.TrimSpace(input[start:]))
	return stages
}

// containsShellWrappableCommand conservatively detects a registered CLI
// anywhere in a shell expression. It is used only when the expression cannot
// be reduced to one leading command. In that case the exact mutation cannot
// be classified safely, so the caller routes the action through confirmation
// as an update rather than treating it as a read.
func containsShellWrappableCommand(input string) bool {
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || strings.ContainsRune(";&|(){}$`'\"", r)
	})
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if isEnvAssignment(part) {
			// Shell variables can hold an executable and be invoked later in the
			// same expression: `k=kubectl; $k delete ...`. Inspecting only `$k`
			// loses the value and would let the registered CLI evade the gate.
			if _, value, ok := strings.Cut(part, "="); ok && core.LookupShellWrappable(filepath.Base(value)) != "" {
				return true
			}
			continue
		}
		// Absolute/relative executable paths are equivalent to their basename
		// for registry ownership: /usr/local/bin/kubectl is still kubectl.
		if core.LookupShellWrappable(filepath.Base(part)) != "" {
			return true
		}
	}
	return false
}

// extractLeadingShellCommand returns two values:
//   - leading: the first executable command name after skipping leading
//     whitespace, inline environment-variable assignments (`FOO=bar cmd`),
//     and privilege/env wrappers (`sudo cmd`, `env FOO=bar cmd`).
//   - cleaned: the same command with those prefixes stripped, suitable for
//     passing to the downstream classifier which expects the input to start
//     with its own executable name. `sudo env FOO=bar kubectl delete X`
//     yields leading="kubectl", cleaned="kubectl delete X".
//
// Both return "" if the input is empty, all-whitespace, or opens with a
// construct that can't be reliably reduced to a single command
// (`sh -c '...'`, backtick, `$(...)`, `cd && ...`). The caller separately
// detects registered CLIs in those expressions and routes them to approval.
//
// Not a full shell parser: pipes (`|`), redirects (`>`), and compound
// operators (`&&`, `;`, `||`) after the leading command are kept in `cleaned`
// as-is — the downstream classifier's job to handle. See InferToolRequestType's
// doc for the full rationale.
//
// Note on quoting fidelity: `cleaned` is reconstructed by joining shlex tokens
// with single spaces, re-quoting any token that contains whitespace, quotes,
// or backslashes (see reQuoteShellToken). This preserves argument boundaries:
// `-l "app in (foo, bar)"` round-trips back to `-l "app in (foo, bar)"`
// rather than being re-split into `-l`, `app`, `in`, ... by a downstream
// shlex parse. Exact byte-for-byte formatting (which quote style, extra
// whitespace) is NOT preserved.
func extractLeadingShellCommand(input string) (leading, cleaned string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return "", ""
	}
	// Reject shapes we can't safely reduce.
	//   - Subshell/eval wrappers: `sh -c '...'`, `bash -c '...'`, `eval '...'`
	//   - Command substitution: leading `$(...)` or backtick
	//   - Subshell / group commands: leading `(...)` or `{...}` — the real
	//     command lives inside; treating the paren/brace as a token would
	//     misclassify or bypass. Safe default: don't classify.
	//   - Prefix chains we don't try to unwind: `cd ... && ...`
	// The caller applies a conservative registered-CLI scan to these shapes.
	if strings.HasPrefix(input, "$(") || strings.HasPrefix(input, "`") ||
		strings.HasPrefix(input, "(") || strings.HasPrefix(input, "{") ||
		strings.HasPrefix(input, "cd ") || strings.HasPrefix(input, "cd\t") ||
		strings.HasPrefix(input, "eval ") || strings.HasPrefix(input, "eval\t") {
		return "", ""
	}
	// Tokenize via shlex — the same POSIX-compliant tokenizer already used by
	// tool_shell.go, tool_cloud_gcp.go, tool_cloud_azure.go, common.go — so
	// quoted env-var values with spaces (`KUBECONFIG="/etc/kube config" kubectl`)
	// stay a single token rather than being torn apart by strings.Fields.
	// Malformed quoting → return unclassified (bailout — the safest default,
	// since the LLM likely got the input wrong anyway).
	tokens, err := shlex.Split(input)
	if err != nil {
		return "", ""
	}
	// Walk tokens: skip env-var assignments, unwrap sudo/env wrappers
	// (including their flags + flag values), bail on subshell wrappers.
	// Uses an index-based loop (not range) because sudo/env flag handling
	// needs to consume a variable number of tokens per wrapper.
	//
	// An env-var assignment matches `[A-Za-z_][A-Za-z0-9_]*=...` with the `=`
	// occurring BEFORE any non-identifier char (see isEnvAssignment in
	// shell_suspicious.go — same rule as the suspicious-command detector).
	leadingIdx := -1
	i := 0
	for i < len(tokens) {
		tok := tokens[i]
		if isEnvAssignment(tok) {
			i++
			continue
		}
		// Unwrap privilege / env wrappers AND their flags. Without this,
		// `sudo -u admin kubectl delete X` would stop at `-u` and treat it as
		// the leading command → no registry match → no gate → BYPASS.
		// consumeSudoEnvFlags advances past the wrapper's `-flags` and any
		// value-taking flags' argument tokens (`-u admin`, `--user=admin`).
		if tok == "sudo" || tok == "env" {
			i = consumeSudoEnvFlags(tok, tokens, i+1)
			continue
		}
		if tok == "sh" || tok == "bash" || tok == "zsh" {
			// bare shell interpreters — the arg is a quoted script we'd have
			// to re-parse (`sh -c 'kubectl delete X'`). Safe default: don't
			// classify.
			return "", ""
		}
		leadingIdx = i
		break
	}
	if leadingIdx == -1 {
		return "", ""
	}
	leading = tokens[leadingIdx]
	rebuilt := make([]string, 0, len(tokens)-leadingIdx)
	for _, tok := range tokens[leadingIdx:] {
		rebuilt = append(rebuilt, reQuoteShellToken(tok))
	}
	cleaned = strings.Join(rebuilt, " ")
	return leading, cleaned
}

// reQuoteShellToken wraps a shlex-stripped token in double quotes if it
// contains characters that would cause a downstream shlex parse to split it
// (whitespace) or misparse (quote / backslash). Backslashes and embedded
// double quotes get escaped. Bare tokens without any of these chars pass
// through unchanged so we don't quote every atom needlessly.
//
// Used by extractLeadingShellCommand when rebuilding the cleaned command so
// the downstream classifier sees the same argument boundaries as the LLM
// wrote — e.g. `-l "app in (foo, bar)"` stays one argument, not five.
func reQuoteShellToken(tok string) string {
	if tok == "" {
		return `""`
	}
	if !strings.ContainsAny(tok, " \t\n'\"\\") {
		return tok
	}
	escaped := strings.ReplaceAll(tok, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// flagTakesValue reports whether the given short/long flag is one that
// consumes the NEXT positional token as its argument, disambiguating by
// wrapper because sudo and env don't share a flag namespace. Notably:
//   - `env -S` (`--split-string`) TAKES a value.
//   - `sudo -S` (`--stdin`) does NOT — it's a flag-only "read password from
//     stdin" toggle. Lumping the two together would misparse `sudo -S kubectl`
//     as "skip kubectl too".
//
// Flags that embed their value with `=` (`--user=admin`) are handled by the
// caller and don't need to appear here.
//
// The list covers the flags realistically emitted by an LLM through
// shell_execute — sudo's user/group/prompt/host/role/type/etc. and env's
// unset/chdir/split. Not every possible flag; obscure ones default to
// "flag-only, no value" — worst case, the classifier sees the flag's
// intended value as the leading command and returns unclassified. The
// workspace-pod containment layer still applies to that miss.
func flagTakesValue(wrapper, flag string) bool {
	switch wrapper {
	case "sudo":
		switch flag {
		case "-u", "-U", "-g", "-p", "-C", "-h", "-r", "-t", "-T", "-D", "-R":
			return true
		case "--user", "--group", "--prompt", "--close-from", "--host", "--role", "--type", "--other-user",
			"--command-timeout", "--timeout", "--chdir", "--chroot":
			return true
		}
	case "env":
		switch flag {
		case "-u", "-C", "-S":
			return true
		case "--unset", "--chdir", "--split-string":
			return true
		}
	}
	return false
}

// consumeSudoEnvFlags advances the token index past sudo/env's flags. Called
// with `start` pointing at the first token AFTER the sudo/env wrapper itself;
// `wrapper` is either "sudo" or "env" so flag-value semantics disambiguate
// (see flagTakesValue). Skips: option flags (`-x`, `--long`), embedded-value
// flags (`--user=admin`), value-taking flag arguments (`-u admin`), grouped
// short flags where the trailing char takes a value (`-Eu admin`), inline
// env-var assignments (`env FOO=bar CMD`), and honors the POSIX end-of-options
// delimiter (`--`) after which everything is treated as the command.
// Returns the index of the first token that looks like the real command.
func consumeSudoEnvFlags(wrapper string, tokens []string, start int) int {
	i := start
	for i < len(tokens) {
		tok := tokens[i]
		if isEnvAssignment(tok) {
			i++
			continue
		}
		if !strings.HasPrefix(tok, "-") {
			return i
		}
		// End-of-options delimiter: `sudo -E -- kubectl delete X`. Everything
		// after `--` is positional; the next token is the real command.
		if tok == "--" {
			return i + 1
		}
		i++
		// Determine whether this flag takes a value. Two shapes to consider:
		//   - Exact match: `-u`, `--user`, etc. → flagTakesValue.
		//   - Grouped short flags: `-Eu` where the LAST char (`u`) is the
		//     value-taker and the earlier ones (`E`) are boolean toggles. Only
		//     applies when there's no `=` (embedded value) and no long-form
		//     prefix (`--`) — long options don't group. Checking last char is
		//     conservative: if the grouped form isn't actually value-taking
		//     (e.g. `-Eh` where `-h` is a help flag we don't list), we won't
		//     consume the next token — worst case, classifier gets the flag's
		//     intended value as leading and returns unclassified.
		takesValue := false
		if !strings.Contains(tok, "=") {
			if flagTakesValue(wrapper, tok) {
				takesValue = true
			} else if !strings.HasPrefix(tok, "--") && len(tok) > 2 {
				last := "-" + tok[len(tok)-1:]
				if flagTakesValue(wrapper, last) {
					takesValue = true
				}
			}
		}
		if takesValue && i < len(tokens) {
			i++
		}
	}
	return i
}
