package conformance_test

import (
	"testing"

	"github.com/Phong95/aec-agent-go/components/persistence"
	"github.com/Phong95/aec-agent-go/conformance"
)

// TestSkillStoreDefault runs the SkillStore suite on the persistence default.
func TestSkillStoreDefault(t *testing.T) {
	conformance.SkillStore(t, func(_ *testing.T) persistence.SkillStore {
		return persistence.NewMemorySkillStore()
	})
}

// TestArtifactStoreDefault runs the ArtifactStore suite on the persistence default.
func TestArtifactStoreDefault(t *testing.T) {
	conformance.ArtifactStore(t, func(_ *testing.T) persistence.ArtifactStore {
		return persistence.NewMemoryArtifactStore()
	})
}

// TestChannelStoreDefault runs the ChannelStore suite on the persistence default.
func TestChannelStoreDefault(t *testing.T) {
	conformance.ChannelStore(t, func(_ *testing.T) persistence.ChannelStore {
		return persistence.NewMemoryChannelStore()
	})
}

// TestTriggerStoreDefault runs the TriggerStore suite on the persistence default (A-70).
// Update (A-126): TestJobStoreDefault - the JobStore suite.
func TestJobStoreDefault(t *testing.T) {
	conformance.JobStore(t, func(_ *testing.T) persistence.JobStore {
		return persistence.NewMemoryJobStore()
	})
}

// TestMessageStoreDefault runs the MessageStore suite on the persistence default.
func TestMessageStoreDefault(t *testing.T) {
	conformance.MessageStore(t, func(_ *testing.T) persistence.MessageStore {
		return persistence.NewMemoryMessageStore()
	})
}

// TestCompactedMessageStoreDefault runs the MessageStore suite on the compacted
// message slot of the hub (the eighth store slot).
func TestCompactedMessageStoreDefault(t *testing.T) {
	conformance.MessageStore(t, func(_ *testing.T) persistence.MessageStore {
		return persistence.NewPersistenceHub().CompactedMessages
	})
}

// TestMemoryStoreDefault runs the MemoryStore suite on the persistence default.
func TestMemoryStoreDefault(t *testing.T) {
	conformance.MemoryStore(t, func(_ *testing.T) persistence.MemoryStore {
		return persistence.NewMemoryMemoryStore()
	})
}

// TestAgentStoreDefault runs the AgentStore suite on the persistence default.
func TestAgentStoreDefault(t *testing.T) {
	conformance.AgentStore(t, func(_ *testing.T) persistence.AgentStore {
		return persistence.NewMemoryAgentStore()
	})
}

// TestProviderStoreDefault runs the ProviderStore suite on the persistence default.
func TestProviderStoreDefault(t *testing.T) {
	conformance.ProviderStore(t, func(_ *testing.T) persistence.ProviderStore {
		return persistence.NewMemoryProviderStore()
	})
}

// TestSubagentRunStoreDefault runs the SubagentRunStore suite on the persistence default
// (A-105).
func TestSubagentRunStoreDefault(t *testing.T) {
	conformance.SubagentRunStore(t, func(_ *testing.T) persistence.SubagentRunStore {
		return persistence.NewMemorySubagentRunStore()
	})
}

// TestSecretStoreDefault runs the SecretStore suite on the persistence default (A-101).
func TestSecretStoreDefault(t *testing.T) {
	conformance.SecretStore(t, func(_ *testing.T) persistence.SecretStore {
		return persistence.NewMemorySecretStore()
	})
}

// TestGroupStoreDefault runs the GroupStore suite on the persistence default (A-106).
func TestGroupStoreDefault(t *testing.T) {
	conformance.GroupStore(t, func(_ *testing.T) persistence.GroupStore {
		return persistence.NewMemoryGroupStore()
	})
}

// TestGroupMessageStoreDefault runs the GroupMessageStore suite on the persistence
// default (A-106).
func TestGroupMessageStoreDefault(t *testing.T) {
	conformance.GroupMessageStore(t, func(_ *testing.T) persistence.GroupMessageStore {
		return persistence.NewMemoryGroupMessageStore()
	})
}

// TestSkillProposalStoreDefault runs the SkillProposalStore suite on the persistence
// default (A-166).
func TestSkillProposalStoreDefault(t *testing.T) {
	conformance.SkillProposalStore(t, func(_ *testing.T) persistence.SkillProposalStore {
		return persistence.NewMemorySkillProposalStore()
	})
}
