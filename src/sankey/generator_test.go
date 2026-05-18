package sankey

import (
	"strings"
	"testing"
)

// collectNodeIDs returns every node id the generator will emit for cfg,
// in the same order GenerateSankeyYAML emits them.
func collectNodeIDs(cfg Config) []string {
	var ids []string
	for _, g := range cfg.Groups {
		for _, s := range g.Sensors {
			ids = append(ids, s.Name)
		}
		if g.Other != nil {
			ids = append(ids, g.Other.Key)
		}
	}
	return ids
}

func TestDefaultConfigNodeIDsAreUnique(t *testing.T) {
	seen := map[string]int{}
	for _, id := range collectNodeIDs(DefaultConfig()) {
		seen[id]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("node id %q appears %d times; v4 requires unique ids", id, n)
		}
	}
}

func TestDefaultConfigChildrenReferencesResolve(t *testing.T) {
	cfg := DefaultConfig()
	groupNames := map[string]bool{}
	for _, g := range cfg.Groups {
		groupNames[g.Name] = true
	}
	for _, g := range cfg.Groups {
		for _, child := range g.Children {
			if !groupNames[child] {
				t.Errorf("group %q references unknown child group %q", g.Name, child)
			}
		}
	}
}

func TestDefaultConfigSectionsInRange(t *testing.T) {
	for _, g := range DefaultConfig().Groups {
		if g.Section < SectionPowerhouseIn || g.Section > SectionHouseMainsOut {
			t.Errorf("group %q has section %d outside [%d,%d]", g.Name, g.Section, SectionPowerhouseIn, SectionHouseMainsOut)
		}
	}
}

func TestGeneratedSankeyLinksReferenceKnownNodes(t *testing.T) {
	cfg := DefaultConfig()
	known := map[string]bool{}
	for _, id := range collectNodeIDs(cfg) {
		known[id] = true
	}

	yaml := GenerateSankeyYAML(cfg)
	for i, line := range strings.Split(yaml, "\n") {
		trimmed := strings.TrimSpace(line)
		var prefix, id string
		switch {
		case strings.HasPrefix(trimmed, "- source:"):
			prefix = "source"
			id = strings.TrimSpace(strings.TrimPrefix(trimmed, "- source:"))
		case strings.HasPrefix(trimmed, "target:"):
			prefix = "target"
			id = strings.TrimSpace(strings.TrimPrefix(trimmed, "target:"))
		default:
			continue
		}
		if !known[id] {
			t.Errorf("line %d: link %s %q does not match any generated node id", i+1, prefix, id)
		}
	}
}

func TestGeneratedSankeyHasV4TopLevelKeys(t *testing.T) {
	yaml := GenerateSankeyYAML(DefaultConfig())
	for _, want := range []string{
		"type: custom:sankey-chart\n",
		"\nsections:\n",
		"\nnodes:\n",
		"\nlinks:\n",
	} {
		if !strings.Contains(yaml, want) {
			t.Errorf("generated YAML is missing v4 marker %q", want)
		}
	}
	if strings.Contains(yaml, "entity_id:") {
		t.Error("generated YAML still contains v3 entity_id: field; should be id:")
	}
}
