package llmhub

import "slices"

// Clone returns a deep copy: the modality slices, the capabilities and pricing
// structs and every pointer inside them are copied (nil stays nil), so a caller
// and a provider store never share a mutable part of a model record. Add a line
// here when a pointer, slice or map field is added to ModelRecord,
// ModelCapabilities or ModelPricing; the clonecheck test fails until you do.
func (m ModelRecord) Clone() ModelRecord {
	if m.ContextWindowTokens != nil {
		v := *m.ContextWindowTokens
		m.ContextWindowTokens = &v
	}
	if m.MaxContextWindowTokens != nil {
		v := *m.MaxContextWindowTokens
		m.MaxContextWindowTokens = &v
	}
	if m.MaxOutputTokens != nil {
		v := *m.MaxOutputTokens
		m.MaxOutputTokens = &v
	}
	m.InputModalities = slices.Clone(m.InputModalities)
	m.OutputModalities = slices.Clone(m.OutputModalities)
	m.ReasoningEfforts = slices.Clone(m.ReasoningEfforts)
	if m.Capabilities != nil {
		c := *m.Capabilities
		for _, p := range []**bool{&c.Vision, &c.PDFInput, &c.ToolCalling, &c.Reasoning, &c.StructuredOutputs, &c.Streaming} {
			if *p != nil {
				v := **p
				*p = &v
			}
		}
		m.Capabilities = &c
	}
	if m.Pricing != nil {
		p := *m.Pricing
		if p.InputUSDPerMillionTokens != nil {
			v := *p.InputUSDPerMillionTokens
			p.InputUSDPerMillionTokens = &v
		}
		if p.OutputUSDPerMillionTokens != nil {
			v := *p.OutputUSDPerMillionTokens
			p.OutputUSDPerMillionTokens = &v
		}
		m.Pricing = &p
	}
	return m
}
