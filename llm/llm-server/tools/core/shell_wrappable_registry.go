package core

import (
	"log/slog"
	"sort"
	"strings"
	"sync"
)

// shellWrappableRegistry maps a shell command prefix (`kubectl`, `aws`, `gh`,
// ...) to the NBTool name (`kubectl_execute`, `aws_execute`, `github_execute`)
// that owns it. Populated lazily on first LookupShellWrappable call by walking
// every registered system tool's ShellWrappable implementation.
//
// Why lazy rather than a package-level `var x = populateShellWrappableRegistry()`:
// each CLI tool package registers into `nbSystemTools` from its own `init()`,
// which runs AFTER this package's var initializers (Go guarantees imported
// packages initialize their vars first). An eager init here would see an empty
// `nbSystemTools` and cache an empty registry forever. sync.OnceValue defers
// the walk to first-use — which happens at request-serve time, well after all
// tool packages have registered.
//
// After the one-time population, LookupShellWrappable is a single map read
// (O(1)) — cheap enough for the shell_execute hot path.
var lookupShellWrappableRegistry = sync.OnceValue(populateShellWrappableRegistry)

// LookupShellWrappable returns the NBTool name that has registered ownership
// of the given shell command prefix (case-insensitive), or "" if no tool
// owns it.
func LookupShellWrappable(prefix string) string {
	if prefix == "" {
		return ""
	}
	return lookupShellWrappableRegistry()[strings.ToLower(prefix)]
}

// SystemAccountIDForProbe is the sentinel UUID passed to every tool factory
// during shell-wrappable registry population. Exported so tests in the same
// package (and neighboring packages) can align their dummy-account inputs
// with the exact value the registry probes with — otherwise a factory that
// works with an arbitrary test UUID could silently fail with this sentinel
// and drop its prefixes from the registry.
//
// Why this specific value and not empty string: some factories fail-fast on
// an empty accountId. Their prefixes would vanish from the registry and
// their shell commands would silently run WITHOUT the consent gate — a
// silent security regression. The nil UUID satisfies every factory's format
// check (uuid.Parse succeeds) without matching any real account, so any
// per-account lookup the factory tries during instantiation either resolves
// cleanly OR errors loudly (see the slog.Error paths in
// populateShellWrappableRegistry below).
const SystemAccountIDForProbe = "00000000-0000-0000-0000-000000000000"

// populateShellWrappableRegistry walks every registered system tool factory,
// instantiates it (with SystemAccountIDForProbe — ShellCommandPrefixes is a
// stable property that doesn't depend on account state), and records each
// declared prefix.
//
// Prefix collisions are treated as a hard developer error and logged loudly:
// two tools claiming the same prefix would silently downgrade one of them
// (whichever loses the map-iteration race) into ungated territory — a silent
// security regression. First registration wins so subsequent visits don't
// masquerade the collision.
//
// Factory instantiation errors are ALSO logged loudly. If a tool factory
// returns an error for SystemAccountIDForProbe, its prefixes never get
// registered and its shell commands run without a gate. That's a silent
// security regression class — worth an ERROR log so it surfaces in ops, not
// a silent continue.
//
// Called once by sync.OnceValue on the first LookupShellWrappable call.
//
// Iterates `nbSystemTools` in a sorted-key order. Go map iteration is
// non-deterministic, so any resolution rule that depends on "the first tool
// wins" (like the duplicate-prefix collision check below) would otherwise
// give a random winner across runs — making the gate's behavior flaky.
// Sorting once here amortizes to zero on the hot path (this runs exactly
// once per process via sync.OnceValue).
func populateShellWrappableRegistry() map[string]string {
	toolNames := make([]string, 0, len(nbSystemTools))
	for name := range nbSystemTools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	out := make(map[string]string)
	for _, toolName := range toolNames {
		factory := nbSystemTools[toolName]
		tool, err := factory(SystemAccountIDForProbe)
		if err != nil {
			slog.Error("shell-wrappable: factory failed with SystemAccountIDForProbe — tool's prefixes will NOT be registered, its shell commands run without consent gate",
				"tool", toolName, "error", err)
			continue
		}
		if tool == nil {
			slog.Error("shell-wrappable: factory returned nil tool with SystemAccountIDForProbe — tool's prefixes will NOT be registered",
				"tool", toolName)
			continue
		}
		wrappable, ok := tool.(ShellWrappable)
		if !ok {
			continue
		}
		for _, prefix := range wrappable.ShellCommandPrefixes() {
			lower := strings.ToLower(prefix)
			if existing, dup := out[lower]; dup && existing != toolName {
				slog.Error("shell-wrappable prefix registered by multiple tools — first registration wins, later tool won't be gated via shell_execute",
					"prefix", prefix, "kept", existing, "dropped", toolName)
				continue
			}
			out[lower] = toolName
		}
	}
	return out
}

// VisitSystemToolFactories calls fn for each registered system-tool factory,
// in sorted-name order for deterministic iteration. Exists so cross-package
// tests (in the `tools` package where every CLI init() has run and
// nbSystemTools is fully populated) can enumerate the full registry without
// this package taking a reverse import on `tools` — a circular dependency
// that would break the module.
//
// Prod code should NOT use this — resolve individual tools via GetNBTool
// which layers per-account custom / MCP / automation sources on top.
func VisitSystemToolFactories(fn func(name string, factory func(accountId string) (NBTool, error))) {
	names := make([]string, 0, len(nbSystemTools))
	for name := range nbSystemTools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fn(name, nbSystemTools[name])
	}
}
