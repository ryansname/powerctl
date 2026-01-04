package sankey

// GeneratedConfigs holds both generated YAML outputs
type GeneratedConfigs struct {
	SankeyConfig string
	Templates    string
}

// Generate produces both YAML configurations from the default config
func Generate() GeneratedConfigs {
	cfg := DefaultConfig()
	return GeneratedConfigs{
		SankeyConfig: GenerateSankeyYAML(cfg),
		Templates:    GenerateTemplatesYAML(cfg),
	}
}
