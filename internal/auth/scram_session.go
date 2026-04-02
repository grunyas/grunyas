package auth

import "github.com/xdg-go/scram"

// ScramSession holds the state of a single SCRAM authentication exchange between the client and server.
// It wraps the underlying xdg-go/scram conversation and implements types.SCRAMSession.
type ScramSession struct {
	conv *scram.ServerConversation
}

// Step processes the next message in the SCRAM handshake.
// It takes the client's message, advances the conversation state, and returns the server's response.
// It returns an error if the exchange fails or violates the protocol.
func (s *ScramSession) Step(msg string) (string, error) {
	return s.conv.Step(msg)
}
