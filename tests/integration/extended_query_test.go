//go:build integration
// +build integration

package integration

import (
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

// Extended protocol integration tests.
// These are wire-level expectations that verify message ordering, error state,
// and transaction semantics against a real PostgreSQL backend.
func TestUnnamedExtendedQuerySelect1(t *testing.T) {
	// Baseline extended protocol flow using unnamed statement/portal.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Parse{Name: "", Query: "SELECT 1"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "",
			PreparedStatement: "",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ParseComplete: %v", err)
			}
			expectParseComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive BindComplete: %v", err)
			}
			expectBindComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive execute response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")
		}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestNamedPreparedStatementReusedTwice(t *testing.T) {
	// Named statement reused across multiple binds/executions; portals are transient.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Parse{
			Name:          "S1",
			Query:         "SELECT $1::int + 1",
			ParameterOIDs: []uint32{23},
		}),
		cStep(&pgproto3.Sync{}),
		sStep(expectParseComplete),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// First execution
		cStep(&pgproto3.Bind{
			DestinationPortal: "P1",
			PreparedStatement: "S1",
			ParameterFormatCodes: []int16{
				0,
			},
			Parameters: [][]byte{
				[]byte("41"),
			},
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "P1"}),
		cStep(&pgproto3.Execute{Portal: "P1", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		sStep(expectBindComplete),
		sStep(expectRowDescriptionInt4),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "42")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "SELECT 1")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Statement should still exist.
		cStep(&pgproto3.Describe{ObjectType: 'S', Name: "S1"}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParameterDescriptionInt4),
		sStep(expectRowDescriptionInt4),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Portal should be removed.
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "P1"}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "34000")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Second execution with different param
		cStep(&pgproto3.Bind{
			DestinationPortal: "P2",
			PreparedStatement: "S1",
			ParameterFormatCodes: []int16{
				0,
			},
			Parameters: [][]byte{
				[]byte("99"),
			},
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "P2"}),
		cStep(&pgproto3.Execute{Portal: "P2", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		sStep(expectBindComplete),
		sStep(expectRowDescriptionInt4),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "100")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "SELECT 1")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Statement should still exist.
		cStep(&pgproto3.Describe{ObjectType: 'S', Name: "S1"}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParameterDescriptionInt4),
		sStep(expectRowDescriptionInt4),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Portal should be removed.
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "P2"}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "34000")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestPortalSuspendedWithMaxRows(t *testing.T) {
	// PortalSuspended behavior with partial fetches and continuation.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	runSteps(t, client, []step{
		// Setup table nums with 1..10
		cStep(&pgproto3.Query{String: "CREATE TEMP TABLE nums (n int)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "CREATE TABLE")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),
		cStep(&pgproto3.Query{String: "INSERT INTO nums SELECT generate_series(1,10)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "INSERT 0 10")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		cStep(&pgproto3.Parse{
			Name:          "Snums",
			Query:         "SELECT n FROM nums ORDER BY n",
			ParameterOIDs: nil,
		}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "Pnums",
			PreparedStatement: "Snums",
			Parameters:        nil,
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "Pnums"}),
		cStep(&pgproto3.Execute{Portal: "Pnums", MaxRows: 3}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParseComplete),
		sStep(expectBindComplete),
		sStep(expectRowDescriptionInt4),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "1")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "2")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "3")
		}),
		sStep(expectPortalSuspended),

		// Portal should still exist.
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "Pnums"}),
		cStep(&pgproto3.Flush{}),
		sStep(expectRowDescriptionInt4),

		// fetch next 3
		cStep(&pgproto3.Execute{Portal: "Pnums", MaxRows: 3}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "4")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "5")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "6")
		}),
		sStep(expectPortalSuspended),

		// Portal should still exist.
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "Pnums"}),
		cStep(&pgproto3.Flush{}),
		sStep(expectRowDescriptionInt4),

		// fetch rest (0 = no limit)
		cStep(&pgproto3.Execute{Portal: "Pnums", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "7")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "8")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "9")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectDataRowValues(t, msg, "10")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "SELECT 4")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),
	})
}

func TestDescribeStatementThenBindExecute(t *testing.T) {
	// Describe a statement, then Bind/Execute; accept optional RowDescription on Execute.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		// Setup users table
		cStep(&pgproto3.Query{String: "CREATE TEMP TABLE users (id int, name text)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "CREATE TABLE")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),
		cStep(&pgproto3.Query{String: "INSERT INTO users (id, name) VALUES (123, 'alice')"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "INSERT 0 1")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		cStep(&pgproto3.Parse{
			Name:          "Suser",
			Query:         "SELECT id, name FROM users WHERE id = $1",
			ParameterOIDs: []uint32{23},
		}),
		cStep(&pgproto3.Describe{ObjectType: 'S', Name: "Suser"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "Puser1",
			PreparedStatement: "Suser",
			ParameterFormatCodes: []int16{
				0,
			},
			Parameters: [][]byte{
				[]byte("123"),
			},
			ResultFormatCodes: []int16{0, 0},
		}),
		cStep(&pgproto3.Execute{Portal: "Puser1", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ParseComplete: %v", err)
			}
			expectParseComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ParameterDescription: %v", err)
			}
			expectParameterDescriptionInt4(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive RowDescription: %v", err)
			}
			expectRowDescriptionInt4Text(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive BindComplete: %v", err)
			}
			expectBindComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive execute response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4Text(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "123", "alice")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestDescribeNonexistentPortal(t *testing.T) {
	// Describe error puts server into "discard until Sync" mode.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: "nope"}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "34000")
		}),

		// In error state: server should ignore until Sync.
		cStep(&pgproto3.Execute{Portal: "nope", MaxRows: 0}),
		cStep(&pgproto3.Close{ObjectType: 'P', Name: "nope"}),
		cStep(&pgproto3.Flush{}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestParseErrorSetsErrorStateUntilSync(t *testing.T) {
	// Parse error should produce ErrorResponse and clear only after Sync.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Parse{Name: "", Query: "SELEC 1"}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "42601")
		}),

		// Keep sending messages; server should discard until Sync.
		cStep(&pgproto3.Bind{
			DestinationPortal: "",
			PreparedStatement: "",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Describe{ObjectType: 'P', Name: ""}),
		cStep(&pgproto3.Execute{Portal: "", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestBindMissingStatement(t *testing.T) {
	// Bind to missing prepared statement returns an error and resets after Sync.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Bind{
			DestinationPortal: "P1",
			PreparedStatement: "NoSuchStmt",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "26000")
		}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestExecuteRuntimeError(t *testing.T) {
	// Runtime error during Execute should error and clear after Sync.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		// Setup table with unique constraint and seed a row.
		cStep(&pgproto3.Query{String: "CREATE TEMP TABLE t_unique (id int PRIMARY KEY)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "CREATE TABLE")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),
		cStep(&pgproto3.Query{String: "INSERT INTO t_unique (id) VALUES (1)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "INSERT 0 1")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		cStep(&pgproto3.Parse{
			Name:          "Sins",
			Query:         "INSERT INTO t_unique(id) VALUES($1)",
			ParameterOIDs: []uint32{23},
		}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "P",
			PreparedStatement: "Sins",
			ParameterFormatCodes: []int16{
				0,
			},
			Parameters: [][]byte{
				[]byte("1"),
			},
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "P", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParseComplete),
		sStep(expectBindComplete),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "23505")
		}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestErrorMidPipeline(t *testing.T) {
	// Pipeline with an error in the middle; server discards remaining messages.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	runSteps(t, client, []step{
		// Pipeline without flush; Sync will flush the batch.
		cStep(&pgproto3.Parse{Name: "Sok", Query: "SELECT 1"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "Pok",
			PreparedStatement: "Sok",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "Pok", MaxRows: 0}),

		cStep(&pgproto3.Parse{Name: "Sbad", Query: "INSRT INTO t VALUES(1)"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "Pbad",
			PreparedStatement: "Sbad",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "Pbad", MaxRows: 0}),

		cStep(&pgproto3.Parse{Name: "Snever", Query: "SELECT 2"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "Pnever",
			PreparedStatement: "Snever",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "Pnever", MaxRows: 0}),

		cStep(&pgproto3.Sync{}),

		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ParseComplete: %v", err)
			}
			expectParseComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive BindComplete: %v", err)
			}
			expectBindComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive query response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ErrorResponse: %v", err)
			}
			expectErrorResponseCode(t, msg, "42601")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),
	})
}

func TestSimpleQueryClearsUnnamedExtendedState(t *testing.T) {
	// Simple Query invalidates unnamed extended statement/portal.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		// First create unnamed statement/portal via extended protocol.
		cStep(&pgproto3.Parse{Name: "", Query: "SELECT 1"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "",
			PreparedStatement: "",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ParseComplete: %v", err)
			}
			expectParseComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive BindComplete: %v", err)
			}
			expectBindComplete(t, msg)

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive execute response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Simple Query should invalidate unnamed statement/portal.
		cStep(&pgproto3.Query{String: "SELECT 42"}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive simple query response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "42")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),

		// Bind unnamed without parse should now fail.
		cStep(&pgproto3.Bind{
			DestinationPortal: "",
			PreparedStatement: "",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Flush{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "26000")
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestExtendedImplicitTransactionCommit(t *testing.T) {
	// Extended protocol in implicit transaction auto-commits on Sync.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		// Setup table
		cStep(&pgproto3.Query{String: "CREATE TEMP TABLE t_values (v int)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "CREATE TABLE")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		cStep(&pgproto3.Parse{
			Name:          "Sins",
			Query:         "INSERT INTO t_values(v) VALUES ($1)",
			ParameterOIDs: []uint32{23},
		}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "P1",
			PreparedStatement: "Sins",
			ParameterFormatCodes: []int16{
				0,
			},
			Parameters: [][]byte{
				[]byte("10"),
			},
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "P1", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParseComplete),
		sStep(expectBindComplete),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "INSERT 0 1")
		}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		// Verify data committed.
		cStep(&pgproto3.Query{String: "SELECT v FROM t_values"}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive select response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "10")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestExplicitTransactionErrorStateAndRollback(t *testing.T) {
	// Explicit transaction enters failed state on error and clears on rollback.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var readyAfterBegin byte
	var readyAfterError byte
	var readyAfterParseInFailedTx byte
	var readyAfterRollback byte
	var haveReadyAfterBegin bool
	var haveReadyAfterError bool
	var haveReadyAfterParseInFailedTx bool
	var haveReadyAfterRollback bool

	runSteps(t, client, []step{
		// Setup table for transaction test.
		cStep(&pgproto3.Query{String: "CREATE TEMP TABLE t_values (v int)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "CREATE TABLE")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),

		// Begin explicit transaction.
		cStep(&pgproto3.Query{String: "BEGIN"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "BEGIN")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			readyAfterBegin = expectReadyForQuery(t, msg, 'T').TxStatus
			haveReadyAfterBegin = true
		}),

		// Use extended protocol inside transaction.
		cStep(&pgproto3.Parse{
			Name:          "Sins",
			Query:         "INSERT INTO t_values(v) VALUES ($1)",
			ParameterOIDs: []uint32{23},
		}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "P1",
			PreparedStatement: "Sins",
			ParameterFormatCodes: []int16{
				0,
			},
			Parameters: [][]byte{
				[]byte("100"),
			},
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Execute{Portal: "P1", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParseComplete),
		sStep(expectBindComplete),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "INSERT 0 1")
		}),

		// Trigger syntax error in same transaction.
		cStep(&pgproto3.Parse{
			Name:          "Sbad",
			Query:         "INSRT INTO t_values(v) VALUES ($1)",
			ParameterOIDs: []uint32{23},
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "42601")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			readyAfterError = expectReadyForQuery(t, msg, 'E').TxStatus
			haveReadyAfterError = true
		}),

		// In failed transaction, further commands should error.
		cStep(&pgproto3.Parse{
			Name:  "Sx",
			Query: "SELECT 1",
		}),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectErrorResponseCode(t, msg, "25P02")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			readyAfterParseInFailedTx = expectReadyForQuery(t, msg, 'E').TxStatus
			haveReadyAfterParseInFailedTx = true
		}),

		// Rollback clears error state.
		cStep(&pgproto3.Query{String: "ROLLBACK"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "ROLLBACK")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			readyAfterRollback = expectReadyForQuery(t, msg, 'I').TxStatus
			haveReadyAfterRollback = true
		}),

		// Verify insert was rolled back.
		cStep(&pgproto3.Query{String: "SELECT v FROM t_values WHERE v = 100"}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive select response: %v", err)
			}
			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive next message: %v", err)
				}
			}

			if dr, ok := msg.(*pgproto3.DataRow); ok {
				expectDataRowValues(t, dr, "100")
				t.Fatalf("expected no rows after rollback")
			}

			expectCommandComplete(t, msg, "SELECT 0")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if !haveReadyAfterBegin || !haveReadyAfterError || !haveReadyAfterParseInFailedTx || !haveReadyAfterRollback {
				t.Fatalf("missing ReadyForQuery")
			}
			if readyAfterBegin != 'T' {
				t.Fatalf("expected TxStatus T after BEGIN, got %q", readyAfterBegin)
			}
			if readyAfterError != 'E' {
				t.Fatalf("expected TxStatus E after error, got %q", readyAfterError)
			}
			if readyAfterParseInFailedTx != 'E' {
				t.Fatalf("expected TxStatus E in failed tx, got %q", readyAfterParseInFailedTx)
			}
			if readyAfterRollback != 'I' {
				t.Fatalf("expected TxStatus I after rollback, got %q", readyAfterRollback)
			}
		}),
	})
}

func TestCloseStatementCleansPortals(t *testing.T) {
	// Close statement, then execute portal; accept error or success depending on backend.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Parse{Name: "S1", Query: "SELECT 1"}),
		cStep(&pgproto3.Bind{
			DestinationPortal: "P1",
			PreparedStatement: "S1",
			ResultFormatCodes: []int16{0},
		}),
		cStep(&pgproto3.Flush{}),
		sStep(expectParseComplete),
		sStep(expectBindComplete),

		cStep(&pgproto3.Close{ObjectType: 'S', Name: "S1"}),
		cStep(&pgproto3.Flush{}),
		sStep(expectCloseComplete),

		// Executing the portal may error (if portals are cleaned up) or still succeed
		// (Postgres keeps existing portals even after statement close).
		cStep(&pgproto3.Execute{Portal: "P1", MaxRows: 0}),
		cStep(&pgproto3.Flush{}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive execute response: %v", err)
			}
			if errResp, ok := msg.(*pgproto3.ErrorResponse); ok {
				if errResp.Code != "26000" {
					t.Fatalf("expected error code 26000, got %q", errResp.Code)
				}
				return
			}

			if rd, ok := msg.(*pgproto3.RowDescription); ok {
				expectRowDescriptionInt4(t, rd)
				msg, err = client.fe.Receive()
				if err != nil {
					t.Fatalf("failed to receive DataRow: %v", err)
				}
			}
			expectDataRowValues(t, msg, "1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")
		}),

		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestCloseNonexistentPortal(t *testing.T) {
	// Closing an unknown portal still returns CloseComplete.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	client := newTestClient(t, addr, env)
	defer client.close()

	var finalReady *pgproto3.ReadyForQuery

	runSteps(t, client, []step{
		cStep(&pgproto3.Close{ObjectType: 'P', Name: "ghost"}),
		cStep(&pgproto3.Flush{}),
		sStep(expectCloseComplete),
		cStep(&pgproto3.Sync{}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			finalReady = expectReadyForQuery(t, msg, 'I')
		}),

		assertStep(func(t *testing.T, _ *testClient) {
			if finalReady == nil {
				t.Fatalf("missing ReadyForQuery")
			}
			if finalReady.TxStatus == 'E' {
				t.Fatalf("expected InError=false, got TxStatus=%q", finalReady.TxStatus)
			}
		}),
	})
}

func TestSessionStateIsolation(t *testing.T) {
	// Verify that session state set by one client does not leak to another.
	// This tests that DISCARD ALL is executed when connections are released.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	// Client A: Set a session parameter
	clientA := newTestClient(t, addr, env)
	defer clientA.close()
	runSteps(t, clientA, []step{
		cStep(&pgproto3.Query{String: "SET statement_timeout = '1234ms'"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "SET")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'I')
		}),
	})

	// Client B: Should get a fresh connection with default statement_timeout
	clientB := newTestClient(t, addr, env)
	defer clientB.close()

	runSteps(t, clientB, []step{
		cStep(&pgproto3.Query{String: "SHOW statement_timeout"}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive RowDescription: %v", err)
			}
			if _, ok := msg.(*pgproto3.RowDescription); !ok {
				t.Fatalf("expected RowDescription, got %T", msg)
			}

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive DataRow: %v", err)
			}
			dr, ok := msg.(*pgproto3.DataRow)
			if !ok {
				t.Fatalf("expected DataRow, got %T", msg)
			}

			// The value should be the default (0 = no timeout), not "1234ms"
			val := string(dr.Values[0])
			if val == "1234ms" {
				t.Fatalf("session state leaked: statement_timeout is still %q, expected default (0)", val)
			}

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SHOW")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),
	})
}

func TestSessionResetAfterOpenTransaction(t *testing.T) {
	// Verify that an open transaction and temp objects do not leak to the next client.
	env := loadTestEnv(t)
	addr, stopProxy := startProxy(t, env)
	defer stopProxy()

	clientA := newTestClient(t, addr, env)
	defer clientA.close()

	runSteps(t, clientA, []step{
		cStep(&pgproto3.Query{String: "BEGIN"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "BEGIN")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'T')
		}),
		cStep(&pgproto3.Query{String: "SET LOCAL statement_timeout = '1234ms'"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "SET")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'T')
		}),
		cStep(&pgproto3.Query{String: "CREATE TEMP TABLE temp_reset_test(x int)"}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectCommandComplete(t, msg, "CREATE TABLE")
		}),
		sStep(func(t *testing.T, msg pgproto3.BackendMessage) {
			expectReadyForQuery(t, msg, 'T')
		}),
	})

	clientB := newTestClient(t, addr, env)
	defer clientB.close()

	runSteps(t, clientB, []step{
		cStep(&pgproto3.Query{String: "SHOW statement_timeout"}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive RowDescription: %v", err)
			}
			if _, ok := msg.(*pgproto3.RowDescription); !ok {
				t.Fatalf("expected RowDescription, got %T", msg)
			}

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive DataRow: %v", err)
			}
			dr, ok := msg.(*pgproto3.DataRow)
			if !ok {
				t.Fatalf("expected DataRow, got %T", msg)
			}

			val := string(dr.Values[0])
			if val == "1234ms" {
				t.Fatalf("session state leaked: statement_timeout is still %q, expected default (0)", val)
			}

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SHOW")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),
		cStep(&pgproto3.Query{String: "SELECT to_regclass('pg_temp.temp_reset_test')"}),
		assertStep(func(t *testing.T, client *testClient) {
			msg, err := client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive RowDescription: %v", err)
			}
			if _, ok := msg.(*pgproto3.RowDescription); !ok {
				t.Fatalf("expected RowDescription, got %T", msg)
			}

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive DataRow: %v", err)
			}
			dr, ok := msg.(*pgproto3.DataRow)
			if !ok {
				t.Fatalf("expected DataRow, got %T", msg)
			}
			if dr.Values[0] != nil {
				t.Fatalf("temp table leaked: expected NULL, got %q", string(dr.Values[0]))
			}

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive CommandComplete: %v", err)
			}
			expectCommandComplete(t, msg, "SELECT 1")

			msg, err = client.fe.Receive()
			if err != nil {
				t.Fatalf("failed to receive ReadyForQuery: %v", err)
			}
			expectReadyForQuery(t, msg, 'I')
		}),
	})
}
