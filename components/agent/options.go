package agent

import (
	"time"

	"github.com/Phong95/aec-agent-go/components/agent/agentloop"
	"github.com/Phong95/aec-agent-go/components/agent/memorysystem"
	"github.com/Phong95/aec-agent-go/components/agent/skilllearning"
)

// options are the settings of New that are not dependencies.
type options struct {
	loopOpts   []agentloop.Option
	memoryOpts []memorysystem.Option
	// learningOpts (A-166) configure the skill learner (its clock, its logger).
	learningOpts []skilllearning.Option
	// memoryWrap is applied to the memory system before the loop sees it. Only the
	// test hook of export_test.go sets it, so a test can watch the recalls of a turn
	// (the Go form of vi.spyOn(memorySystem, "recallForTurn")).
	memoryWrap func(agentloop.Memory) agentloop.Memory
	// jobLocation is the location cron expressions of scheduled jobs are read in; nil
	// means time.Local.
	jobLocation *time.Location
	// promptJobClock is the clock of the prompt-job scheduler; nil means time.Now. Only
	// the test hook of export_test.go sets it.
	promptJobClock func() time.Time
}

// Option changes how New builds an Agent. Options are applied in order.
type Option func(*options)

// WithLoopOptions passes options to agentloop.New, for example a persist timeout
// or the parallel tool bound. They are applied after the logger of Deps.
func WithLoopOptions(opts ...agentloop.Option) Option {
	return func(o *options) {
		o.loopOpts = append(o.loopOpts, opts...)
	}
}

// WithMemoryOptions passes options to memorysystem.New, for example a pinned clock
// and id function so that the records a test consolidates are reproducible. The
// logger of Deps is applied first, so a WithLogger here wins.
func WithMemoryOptions(opts ...memorysystem.Option) Option {
	return func(o *options) {
		o.memoryOpts = append(o.memoryOpts, opts...)
	}
}

// WithSkillLearningOptions (A-166) passes options to the agent's skill learner, for
// example its clock. The logger of Deps is applied first, so a WithLogger here wins.
func WithSkillLearningOptions(opts ...skilllearning.Option) Option {
	return func(o *options) {
		o.learningOpts = append(o.learningOpts, opts...)
	}
}

// WithJobLocation sets the location in which the cron expressions of the agent's
// scheduled jobs are read: the minute, hour, day, month and weekday fields compare
// with the clock of this location. The default is time.Local; tests pin
// time.UTC. A nil location keeps the default. It is the default only: a job
// definition that names a Timezone is read in that zone instead.
func WithJobLocation(loc *time.Location) Option {
	return func(o *options) {
		o.jobLocation = loc
	}
}
