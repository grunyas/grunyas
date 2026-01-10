package auth

import "github.com/xdg-go/scram"

// ScramSession holds the state of a single SCRAM authentication exchange between the client and server.
// It wraps the underlying xdg-go/scram conversation.
type ScramSession struct {
	conv *scram.ServerConversation
}

// Continue processes the next message in the SCRAM handshake.
// It takes the client's message, advances the conversation state, and returns the server's response.
// It returns an error if the exchange fails or violates the protocol.
func (s *ScramSession) Continue(msg string) (string, error) {
	return s.conv.Step(msg)
}

// Finish processes the final message from the client to complete the SCRAM authentication.
// It verifies the client's proof and returns nil if authentication is successful.
func (s *ScramSession) Finish(msg string) error {
	_, err := s.conv.Step(msg)
	return err
}
