package tools

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"nudgebee/llm/tools/core"

	"github.com/google/shlex"
)

// staticCLIArgs returns shell-parsed arguments for a single CLI invocation.
// Compound commands deliberately fall through to the existing LLM classifier:
// classifying only the first command could approve a later mutation hidden
// behind a separator, substitution, or pipeline.
func staticCLIArgs(input string, allowBare bool, executables ...string) ([]string, bool) {
	command := strings.TrimSpace(extractCommandFromToolInput(input))
	if command == "" || hasUnquotedShellSyntax(command) {
		return nil, false
	}
	tokens, err := shlex.Split(command)
	if err != nil || len(tokens) == 0 {
		return nil, false
	}
	matchedExecutable := false
	for _, executable := range executables {
		if strings.EqualFold(filepath.Base(tokens[0]), executable) {
			tokens = tokens[1:]
			matchedExecutable = true
			break
		}
	}
	if (!matchedExecutable && !allowBare) || len(tokens) == 0 {
		return nil, false
	}
	return tokens, true
}

func helpOrVersion(tokens []string) bool {
	if len(tokens) == 0 {
		return false
	}
	for _, token := range tokens {
		switch strings.ToLower(token) {
		case "-h", "--help":
			return true
		}
	}
	first := strings.ToLower(tokens[0])
	return first == "help" || first == "version" ||
		(len(tokens) == 1 && (first == "-v" || first == "--version"))
}

func classifyAction(action string, reads, creates, updates, deletes map[string]struct{}) core.ToolRequestType {
	action = strings.ToLower(action)
	if _, ok := reads[action]; ok {
		return core.ToolRequestTypeRead
	}
	if _, ok := creates[action]; ok {
		return core.ToolRequestTypeCreate
	}
	if _, ok := updates[action]; ok {
		return core.ToolRequestTypeUpdate
	}
	if _, ok := deletes[action]; ok {
		return core.ToolRequestTypeDelete
	}
	return ""
}

func wordSet(words ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(words))
	for _, word := range words {
		result[word] = struct{}{}
	}
	return result
}

var (
	helmReadActions   = wordSet("env", "get", "history", "inspect", "lint", "list", "ls", "search", "show", "status", "template", "verify")
	helmCreateActions = wordSet("install")
	helmUpdateActions = wordSet("rollback", "upgrade")
	helmDeleteActions = wordSet("uninstall")
	helmRepoReads     = wordSet("list")
	helmRepoCreates   = wordSet("add")
	helmRepoUpdates   = wordSet("update")
	helmRepoDeletes   = wordSet("remove")

	githubReadActions   = wordSet("checks", "diff", "download", "list", "status", "view")
	githubCreateActions = wordSet("clone", "create", "fork")
	githubUpdateActions = wordSet("cancel", "close", "comment", "disable", "edit", "enable", "merge", "reopen", "rerun", "run", "set")
	githubDeleteActions = wordSet("delete")

	gitlabReadActions   = wordSet("list", "status", "trace", "view")
	gitlabCreateActions = wordSet("clone", "create", "fork")
	gitlabUpdateActions = wordSet("cancel", "close", "merge", "note", "reopen", "retry", "run", "set", "update")
	gitlabDeleteActions = wordSet("delete")

	argoReadActions   = wordSet("diff", "get", "history", "list")
	argoCreateActions = wordSet("add", "create")
	argoUpdateActions = wordSet("patch", "rollback", "set", "sync", "unset")
	argoDeleteActions = wordSet("delete", "rm", "terminate-op")

	redisReadActions   = wordSet("command", "dbsize", "exists", "get", "hget", "hgetall", "hlen", "hmget", "info", "llen", "lrange", "mget", "ping", "pttl", "scan", "scard", "sismember", "smembers", "ttl", "type", "zrange", "zscore")
	redisCreateActions = wordSet("hset", "lpush", "mset", "rpush", "sadd", "set", "setnx")
	redisUpdateActions = wordSet("append", "decr", "decrby", "expire", "incr", "incrby", "lset", "persist", "pexpire", "rename", "trim", "zadd")
	redisDeleteActions = wordSet("del", "flushall", "flushdb", "hdel", "lpop", "rpop", "srem", "unlink", "zrem")

	rabbitReadActions   = wordSet("get", "list", "overview", "show")
	rabbitCreateActions = wordSet("declare", "publish")
	rabbitUpdateActions = wordSet("purge", "set_permission")
	rabbitDeleteActions = wordSet("close_connection", "delete")
)

func inferHelmRequestType(input string) core.ToolRequestType {
	tokens, ok := staticCLIArgs(input, false, "helm")
	if !ok {
		return ""
	}
	if helpOrVersion(tokens) {
		return core.ToolRequestTypeRead
	}
	if strings.EqualFold(tokens[0], "repo") {
		if len(tokens) < 2 {
			return ""
		}
		return classifyAction(tokens[1], helmRepoReads, helmRepoCreates, helmRepoUpdates, helmRepoDeletes)
	}
	return classifyAction(tokens[0], helmReadActions, helmCreateActions, helmUpdateActions, helmDeleteActions)
}

func inferNestedCLIRequestType(
	input string,
	executable string,
	reads, creates, updates, deletes map[string]struct{},
) core.ToolRequestType {
	tokens, ok := staticCLIArgs(input, false, executable)
	if !ok {
		return ""
	}
	if helpOrVersion(tokens) {
		return core.ToolRequestTypeRead
	}
	if len(tokens) < 2 || strings.HasPrefix(tokens[0], "-") {
		return ""
	}
	if strings.EqualFold(tokens[0], "api") {
		return inferAPIRequestType(tokens[1:])
	}
	return classifyAction(tokens[1], reads, creates, updates, deletes)
}

// inferAPIRequestType classifies the shared `gh api` / `glab api` surface.
// Both default to GET unless a method or input/field flag requests a write.
func inferAPIRequestType(tokens []string) core.ToolRequestType {
	if len(tokens) == 0 {
		return ""
	}
	method := "GET"
	explicitMethod := false
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		key, value, hasValue := strings.Cut(token, "=")
		switch strings.ToLower(key) {
		case "-x", "--method":
			if hasValue {
				if value == "" {
					return ""
				}
				method = strings.ToUpper(value)
			} else {
				if i+1 >= len(tokens) {
					return ""
				}
				i++
				method = strings.ToUpper(tokens[i])
			}
			explicitMethod = true
		case "-f", "--field", "--raw-field", "--form", "--input":
			// gh/glab switch the default method to POST when request data is supplied.
			if !explicitMethod {
				method = "POST"
			}
		}
	}
	return classifyHTTPMethod(method)
}

func inferRedisRequestType(input string) core.ToolRequestType {
	tokens, ok := staticCLIArgs(input, true, "redis-cli", "redis")
	if !ok {
		return ""
	}
	// Connection flags can take arbitrary values that happen to look like Redis
	// verbs. Defer those forms rather than risk classifying a later mutation by
	// an option value (for example, `-h get SET key value`).
	if strings.HasPrefix(tokens[0], "-") {
		if strings.EqualFold(tokens[0], "--help") || strings.EqualFold(tokens[0], "--version") {
			return core.ToolRequestTypeRead
		}
		return ""
	}
	if helpOrVersion(tokens) {
		return core.ToolRequestTypeRead
	}
	return classifyAction(tokens[0], redisReadActions, redisCreateActions, redisUpdateActions, redisDeleteActions)
}

func inferRabbitRequestType(input string) core.ToolRequestType {
	command := extractRabbitCommand(input)
	tokens, ok := staticCLIArgs(command, false, "rabbitmqadmin", "rabbitmq-api", "rabbitmq")
	if !ok {
		return ""
	}
	if helpOrVersion(tokens) {
		return core.ToolRequestTypeRead
	}
	if strings.HasPrefix(tokens[0], "-") {
		return ""
	}
	commandTokens, err := shlex.Split(command)
	if err == nil && len(commandTokens) > 0 && strings.EqualFold(filepath.Base(commandTokens[0]), "rabbitmq-api") {
		return classifyHTTPMethod(tokens[0])
	}
	return classifyAction(tokens[0], rabbitReadActions, rabbitCreateActions, rabbitUpdateActions, rabbitDeleteActions)
}

func extractRabbitCommand(input string) string {
	var command rabbitmqCommand
	if err := json.Unmarshal([]byte(input), &command); err != nil || command.Args == "" {
		return extractCommandFromToolInput(input)
	}
	args := strings.TrimSpace(command.Args)
	if args == "rabbitmq-api" || strings.HasPrefix(args, "rabbitmq-api ") {
		return args
	}
	if command.Command == "" {
		command.Command = "rabbitmqadmin"
	}
	return strings.TrimSpace(command.Command + " " + args)
}

func classifyHTTPMethod(method string) core.ToolRequestType {
	switch strings.ToUpper(method) {
	case "GET", "HEAD", "OPTIONS":
		return core.ToolRequestTypeRead
	case "POST":
		return core.ToolRequestTypeCreate
	case "PUT", "PATCH":
		return core.ToolRequestTypeUpdate
	case "DELETE":
		return core.ToolRequestTypeDelete
	default:
		return ""
	}
}
