package messaging

import (
	"context"
	"testing"

	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

type mockUpstream struct{}

func (m *mockUpstream) Send(msgs ...pgproto3.FrontendMessage) error { return nil }
func (m *mockUpstream) TxStatus() byte                              { return 'I' }
func (m *mockUpstream) Release() error                              { return nil }
func (m *mockUpstream) Kill() error                                 { return nil }

func (m *mockUpstream) Receive(ctx context.Context) (pgproto3.BackendMessage, error) {
	return &pgproto3.ReadyForQuery{TxStatus: 'I'}, nil
}

func (m *mockUpstream) SendSimpleQuery(ctx context.Context, query string) (types.ResultReader, error) {
	return nil, nil
}

func TestProcessExtendedProtocolPinning(t *testing.T) {
	upstream := &mockUpstream{}
	logger := zap.NewNop()
	ctx := context.Background()

	tests := []struct {
		name     string
		msg      pgproto3.FrontendMessage
		wantPin  bool
	}{
		// Parse: only named statements pin
		{"parse unnamed", &pgproto3.Parse{Name: "", Query: "SELECT 1"}, false},
		{"parse named", &pgproto3.Parse{Name: "stmt1", Query: "SELECT 1"}, true},

		// Bind: only named prepared statements pin
		{"bind unnamed prepared", &pgproto3.Bind{PreparedStatement: ""}, false},
		{"bind named prepared", &pgproto3.Bind{PreparedStatement: "stmt1"}, true},

		// Describe: only named statements pin (not portals)
		{"describe unnamed statement", &pgproto3.Describe{ObjectType: 'S', Name: ""}, false},
		{"describe named statement", &pgproto3.Describe{ObjectType: 'S', Name: "stmt1"}, true},
		{"describe portal", &pgproto3.Describe{ObjectType: 'P', Name: "portal1"}, false},

		// Execute: never pins
		{"execute", &pgproto3.Execute{Portal: "portal1"}, false},

		// Sync, Flush, Close: never pin
		{"sync", &pgproto3.Sync{}, false},
		{"flush", &pgproto3.Flush{}, false},
		{"close", &pgproto3.Close{ObjectType: 'S', Name: "stmt1"}, false},

		// Simple query: delegates to queryUsesSessionState
		{"simple query no state", &pgproto3.Query{String: "SELECT 1"}, false},
		{"simple query SET", &pgproto3.Query{String: "SET search_path = public"}, true},
		{"simple query SET LOCAL", &pgproto3.Query{String: "SET LOCAL search_path = public"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pin, err := Process(ctx, tt.msg, upstream, logger)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pin != tt.wantPin {
				t.Fatalf("expected pin=%v, got pin=%v", tt.wantPin, pin)
			}
		})
	}
}
