// Package skilllearning is the skill learner of an agent (A-166): when the agent finally
// finishes a task after many failed tool calls and the user confirms it worked, the
// learner writes the winning path and the dead ends down as a normal SKILL.md - a draft
// for review, or saved at once in auto mode behind a verification replay (A-174) - so
// the next similar request follows the skill instead of figuring it out again. Learned
// skills that stop working are patched or retired (A-172). See System.
package skilllearning
