package coverage

import "github.com/ahrzb/terraform-provider-pmcp/internal/pmcp"

// This file converts a recorded call's decoded-JSON argument map back into the pmcp row types,
// so each scenario's fake hub can answer a create/update with a row shaped the way the real hub
// would — reusing the wire's own field names rather than a second, hand-maintained mapping.

func str(args map[string]any, key string) string {
	if v, ok := args[key]; ok {
		if s, ok2 := v.(string); ok2 {
			return s
		}
	}
	return ""
}

func strDefault(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok2 := v.(string); ok2 {
			return s
		}
	}
	return def
}

func boolDefault(args map[string]any, key string, def bool) bool {
	if v, ok := args[key]; ok {
		if b, ok2 := v.(bool); ok2 {
			return b
		}
	}
	return def
}

func strSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		out[i], _ = e.(string)
	}
	return out
}

func mapListFromAny(v any) map[string][]string {
	raw, ok := v.(map[string]any)
	if !ok {
		return map[string][]string{}
	}
	out := make(map[string][]string, len(raw))
	for k, list := range raw {
		out[k] = strSlice(list)
	}
	return out
}

func mapListFromArgs(args map[string]any, key string) map[string][]string {
	v, ok := args[key]
	if !ok {
		return map[string][]string{}
	}
	return mapListFromAny(v)
}

func rolesFromAny(v any) map[string]pmcp.RoleFamilies {
	raw, ok := v.(map[string]any)
	if !ok {
		return map[string]pmcp.RoleFamilies{}
	}
	out := make(map[string]pmcp.RoleFamilies, len(raw))
	for name, val := range raw {
		obj, _ := val.(map[string]any)
		out[name] = pmcp.RoleFamilies{
			Tools:     strSlice(obj["tools"]),
			Prompts:   strSlice(obj["prompts"]),
			Resources: strSlice(obj["resources"]),
		}
	}
	return out
}

func rolesFromArgs(args map[string]any, key string) map[string]pmcp.RoleFamilies {
	v, ok := args[key]
	if !ok {
		return map[string]pmcp.RoleFamilies{}
	}
	return rolesFromAny(v)
}
