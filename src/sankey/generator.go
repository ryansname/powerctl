package sankey

import (
	"fmt"
	"strings"
)

// indentWriter helps produce properly indented YAML output
type indentWriter struct {
	builder *strings.Builder
	depth   int
}

func newIndentWriter() *indentWriter {
	return &indentWriter{builder: &strings.Builder{}}
}

func (w *indentWriter) indent() {
	w.depth++
}

func (w *indentWriter) unindent() {
	w.depth--
}

func (w *indentWriter) writeLine(line string) {
	for i := 0; i < w.depth*2; i++ {
		w.builder.WriteByte(' ')
	}
	w.builder.WriteString(line)
	w.builder.WriteByte('\n')
}

func (w *indentWriter) writeRaw(s string) {
	w.builder.WriteString(s)
}

func (w *indentWriter) String() string {
	return w.builder.String()
}

// GenerateTemplatesYAML generates the Home Assistant template sensors YAML
func GenerateTemplatesYAML(cfg Config) string {
	w := newIndentWriter()

	for _, sensor := range cfg.Sensors {
		w.writeLine(fmt.Sprintf("- name: \"%s\"", sensor.Name))
		w.writeLine(fmt.Sprintf("  unique_id: %s", sensor.Name))
		w.writeLine("  device_class: power")
		w.writeLine("  unit_of_measurement: W")

		var stateExpr string
		switch sensor.Type {
		case TemplateFormula:
			stateExpr = sensor.Formula
		case TemplateSum:
			quoted := make([]string, len(sensor.Entities))
			for i, e := range sensor.Entities {
				quoted[i] = fmt.Sprintf("'%s'", e)
			}
			stateExpr = fmt.Sprintf("[%s] | map('states') | map('float') | sum", strings.Join(quoted, ", "))
		}
		w.writeLine(fmt.Sprintf("  state: \"{{ %s }}\"", stateExpr))
		w.writeLine("")
	}

	return w.String()
}

// GenerateSankeyYAML generates the Lovelace sankey chart card YAML for ha-sankey-chart v4+.
func GenerateSankeyYAML(cfg Config) string {
	w := newIndentWriter()

	w.writeLine("type: custom:sankey-chart")

	w.writeLine("sections:")
	w.indent()
	for s := SectionPowerhouseIn; s <= SectionHouseMainsOut; s++ {
		w.writeLine("- sort_group_by_parent: true")
	}
	w.unindent()

	w.writeLine("nodes:")
	w.indent()
	for section := SectionPowerhouseIn; section <= SectionHouseMainsOut; section++ {
		for _, group := range cfg.Groups {
			if group.Section != section {
				continue
			}
			for _, sensor := range group.Sensors {
				w.writeLine(fmt.Sprintf("- id: %s", sensor.Name))
				w.indent()
				w.writeLine("type: entity")
				w.writeLine(fmt.Sprintf("section: %d", section))
				if sensor.Label != "" {
					w.writeLine(fmt.Sprintf("name: %s", sensor.Label))
				}
				w.unindent()
			}
			if group.Other != nil {
				w.writeLine(fmt.Sprintf("- id: %s", group.Other.Key))
				w.indent()
				w.writeLine(fmt.Sprintf("type: %s", group.Other.Type.String()))
				w.writeLine(fmt.Sprintf("section: %d", section))
				w.writeLine(fmt.Sprintf("name: %s", group.Other.Label))
				if group.Other.ChildrenSum != nil {
					w.writeLine("children_sum:")
					w.indent()
					w.writeLine(fmt.Sprintf("should_be: %s", group.Other.ChildrenSum.ShouldBe.String()))
					w.writeLine(fmt.Sprintf("reconcile_to: %s", group.Other.ChildrenSum.ReconcileTo.String()))
					w.unindent()
				}
				if group.Other.ParentsSum != nil {
					w.writeLine("parents_sum:")
					w.indent()
					w.writeLine(fmt.Sprintf("should_be: %s", group.Other.ParentsSum.ShouldBe.String()))
					w.writeLine(fmt.Sprintf("reconcile_to: %s", group.Other.ParentsSum.ReconcileTo.String()))
					w.unindent()
				}
				w.unindent()
			}
		}
	}
	w.unindent()

	w.writeLine("links:")
	w.indent()
	for _, group := range cfg.Groups {
		if len(group.Children) == 0 {
			continue
		}
		fromIDs := groupEntityIDs(group)
		if len(fromIDs) == 0 {
			continue
		}
		for _, childName := range group.Children {
			child, ok := findGroup(cfg, childName)
			if !ok {
				continue
			}
			toIDs := groupEntityIDs(child)
			for _, from := range fromIDs {
				for _, to := range toIDs {
					w.writeLine(fmt.Sprintf("- source: %s", from))
					w.indent()
					w.writeLine(fmt.Sprintf("target: %s", to))
					w.unindent()
				}
			}
		}
	}
	w.unindent()

	w.writeRaw(`min_state: 10
show_names: true
wide: false
grid_options:
  columns: full
  rows: 4
static_scale: 0
sort_by: state
throttle: 5000
layout: horizontal
height: 215
unit_prefix: ""
round: 0
convert_units_to: "W"
min_box_size: 10
min_box_distance: 3
show_states: true
show_units: true
`)

	return w.String()
}

// groupEntityIDs returns the node ids contributed by a group: each sensor name plus the remainder key if present.
func groupEntityIDs(g Group) []string {
	ids := make([]string, 0, len(g.Sensors)+1)
	for _, s := range g.Sensors {
		ids = append(ids, s.Name)
	}
	if g.Other != nil {
		ids = append(ids, g.Other.Key)
	}
	return ids
}

func findGroup(cfg Config, name string) (Group, bool) {
	for _, g := range cfg.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return Group{}, false
}
