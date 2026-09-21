package kiro

import (
	"context"
	"time"

	"github.com/sortie-ai/sortie/internal/agent/agentcore"
)

func ListWorkspaceConversationsForTest(ctx context.Context, target agentcore.LaunchTarget, timeout time.Duration, stopGraceMS int) ([]kiroSessionListing, error) {
	return listWorkspaceConversations(ctx, target, timeout, stopGraceMS)
}

func VerificationConversationFoundForTest(before, after []kiroSessionListing) bool {
	_, outcome := findVerificationConversation(before, after)
	return outcome == verificationConversationFound
}

type SessionListingForTest = kiroSessionListing
