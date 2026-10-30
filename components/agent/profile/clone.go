package profile

import "slices"

// Clone returns a deep copy: the Tools, Skills and InferenceConfig.StopSequences
// slices, every pointer sub-config, the memory config and the sandbox selection
// are copied (nil stays nil), so a caller and a store never share a mutable part
// of a profile. Add a line here when a pointer, slice or map field is added to
// AgentProfile or one of its sub-configs; the clonecheck test fails until you do.
func (p AgentProfile) Clone() AgentProfile {
	p.Tools = slices.Clone(p.Tools)
	p.Skills = slices.Clone(p.Skills)
	if p.InitialContext != nil {
		c := *p.InitialContext
		p.InitialContext = &c
	}
	p.Sandbox = p.Sandbox.Clone()
	p.SkillLearning = p.SkillLearning.Clone()
	if p.Compactor.KeepToolResults != nil {
		v := *p.Compactor.KeepToolResults
		p.Compactor.KeepToolResults = &v
	}
	if p.Compactor.LeanTailTokenBudget != nil {
		v := *p.Compactor.LeanTailTokenBudget
		p.Compactor.LeanTailTokenBudget = &v
	}
	if p.InferenceConfig != nil {
		c := *p.InferenceConfig
		if c.MaxTokens != nil {
			v := *c.MaxTokens
			c.MaxTokens = &v
		}
		if c.Temperature != nil {
			v := *c.Temperature
			c.Temperature = &v
		}
		if c.TopP != nil {
			v := *c.TopP
			c.TopP = &v
		}
		c.StopSequences = slices.Clone(c.StopSequences)
		p.InferenceConfig = &c
	}
	if p.LoopConfig != nil {
		c := *p.LoopConfig
		if c.MaxIterations != nil {
			v := *c.MaxIterations
			c.MaxIterations = &v
		}
		if c.MaxToolRetries != nil {
			v := *c.MaxToolRetries
			c.MaxToolRetries = &v
		}
		if c.RetryBackoffMs != nil {
			v := *c.RetryBackoffMs
			c.RetryBackoffMs = &v
		}
		if c.MaxConcurrentTurns != nil {
			v := *c.MaxConcurrentTurns
			c.MaxConcurrentTurns = &v
		}
		if c.MaxToolResultChars != nil {
			v := *c.MaxToolResultChars
			c.MaxToolResultChars = &v
		}
		p.LoopConfig = &c
	}
	p.Memory = p.Memory.Clone()
	p.Subagents = p.Subagents.Clone()
	p.Secrets = p.Secrets.Clone()
	return p
}
