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

// GenerateSankeyYAML generates the Lovelace sankey chart card YAML
func GenerateSankeyYAML(cfg Config) string {
	w := newIndentWriter()

	w.writeLine("sections:")
	w.indent()

	// Iterate through all sections in order
	for section := SectionPowerhouseIn; section <= SectionHouseMainsOut; section++ {
		w.writeLine("- sort_group_by_parent: true")
		w.indent()
		w.writeLine("entities:")

		for _, group := range cfg.Groups {
			if group.Section != section {
				continue
			}

			// Write regular sensors
			for _, sensor := range group.Sensors {
				w.writeLine("- type: entity")
				w.indent()
				w.writeLine(fmt.Sprintf("entity_id: %s", sensor.Name))
				if sensor.Label != "" {
					w.writeLine(fmt.Sprintf("name: %s", sensor.Label))
				}

				if len(group.Children) > 0 {
					writeChildren(w, cfg, group.Children)
				}
				w.unindent()
			}

			// Write remainder entity if present
			if group.Other != nil {
				w.writeLine(fmt.Sprintf("- type: %s", group.Other.Type.String()))
				w.indent()
				w.writeLine(fmt.Sprintf("entity_id: %s", group.Other.Key))
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

				if len(group.Children) > 0 {
					writeChildren(w, cfg, group.Children)
				}
				w.unindent()
			}
		}

		w.unindent()
	}

	w.unindent()

	// Write fixed configuration
	w.writeRaw(`type: custom:sankey-chart
min_state: 10
show_names: true
wide: false
grid_options:
  columns: full
  rows: 4
static_scale: 0
sort_by: state
throttle: 5000
layout: horizontal
height: 200
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

// writeChildren writes the children list for a group
func writeChildren(w *indentWriter, cfg Config, childNames []string) {
	w.writeLine("children:")
	w.indent()
	for _, childName := range childNames {
		for _, child := range cfg.Groups {
			if child.Name != childName {
				continue
			}
			for _, childSensor := range child.Sensors {
				w.writeLine(fmt.Sprintf("- %s", childSensor.Name))
			}
			if child.Other != nil {
				w.writeLine(fmt.Sprintf("- %s", child.Other.Key))
			}
			break
		}
	}
	w.unindent()
}
