package tools

import (
	"fmt"
	"nudgebee/llm/security"
	"nudgebee/llm/tools/core"
	"nudgebee/llm/workspace"
	"strings"
)

// ShellConfigToolName uses the existing detector; script/AST detection is separate work.
func ShellConfigToolName(input string) string {
	_, owner := detectCloudCLI(extractCommandFromToolInput(input))
	return owner
}

// ShellTargetConfigs filters cached tenant inventory for the current caller, on every use.
func ShellTargetConfigs(ctx *security.RequestContext, accountID string, tool core.NBTool) ([]core.ToolConfig, error) {
	configs, err := core.ListToolConfigs(ctx, accountID, tool)
	if err != nil {
		return nil, err
	}
	allowed := make([]core.ToolConfig, 0, len(configs))
	for _, c := range configs {
		for _, v := range c.Values {
			if v.Name == "id" && ctx.GetSecurityContext().HasAccountAccess(v.Value, security.SecurityAccessTypeRead) {
				allowed = append(allowed, c)
				break
			}
		}
	}
	return allowed, nil
}

// ResolveShellTarget never substitutes another account for an invalid saved selection.
func ResolveShellTarget(ctx core.NbToolContext, owner string) (core.ToolConfig, error) {
	tool, ok := core.GetNBTool(ctx.AccountId, owner)
	if !ok {
		return core.ToolConfig{}, fmt.Errorf("shell: target tool %s unavailable", owner)
	}
	configs, err := ShellTargetConfigs(ctx.Ctx, ctx.AccountId, tool)
	if err != nil {
		return core.ToolConfig{}, fmt.Errorf("shell: target lookup: %w", err)
	}
	hint := ctx.QueryConfig.ToolConfigs[owner]
	if hint != "" {
		for _, c := range configs {
			if strings.EqualFold(c.Name, hint) {
				return c, nil
			}
		}
		return core.ToolConfig{}, fmt.Errorf("shell: selected %s configuration is unavailable or unauthorized", owner)
	}
	if len(configs) == 1 {
		return configs[0], nil
	}
	return core.ToolConfig{}, fmt.Errorf("shell: resolve %s account selection before executing this command", owner)
}

// CheckShellTargetWriteAccess keeps source-workspace permissions from authorizing target writes.
func CheckShellTargetWriteAccess(ctx *security.RequestContext, selected core.ToolConfig) error {
	for _, v := range selected.Values {
		if v.Name == "id" && ctx.GetSecurityContext().HasAccountAccess(v.Value, security.SecurityAccessTypeCreate) {
			return nil
		}
	}
	return fmt.Errorf("shell: write access to the selected target account is required")
}

// KubernetesTargetEnv binds the shim's authority to the selected target, keeping
// its workspace account and files unchanged.
func KubernetesTargetEnv(ctx core.NbToolContext, selected core.ToolConfig) (map[string]string, error) {
	for _, v := range selected.Values {
		if v.Name == "id" && v.Value != "" {
			token, err := workspace.KubernetesTargetToken(ctx.Ctx, ctx.AccountId, v.Value)
			if err != nil {
				return nil, err
			}
			return map[string]string{workspace.ENV_NB_TOOL_CONFIG_NAME: selected.Name, workspace.ENV_NB_WORKSPACE_TOKEN: token}, nil
		}
	}
	return nil, fmt.Errorf("selected cluster has no account id")
}
