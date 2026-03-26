package agent

import (
	"context"
	"io"
)

// Provider is the common interface for conversational AI agent backends.
type Provider interface {
	Start(ctx context.Context, reader io.Reader, writer io.Writer,
		apiKey string, opts Options, cb Callbacks) error
	Stop()
	Running() bool
	ConversationID() string
}

// Options configures the agent session.
type Options struct {
	AgentID          string
	Language         string
	FirstMessage     string
	DynamicVariables map[string]string
}

// Callbacks receives events from the agent session.
type Callbacks struct {
	OnConnected      func(conversationID string)
	OnDisconnected   func()
	OnUserTranscript func(text string)
	OnAgentResponse  func(text string)
}
