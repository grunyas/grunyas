// Package session manages individual client connections to the proxy.
// It handles the lifecycle of a client session, including the initial handshake,
// message routing, and connection teardown.
package session

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grunyas/grunyas/config"
	"github.com/grunyas/grunyas/internal/classifier"
	"github.com/grunyas/grunyas/internal/server/messaging"
	"github.com/grunyas/grunyas/internal/server/types"
	"github.com/jackc/pgx/v5/pgproto3"
	"go.uber.org/zap"
)

// Session represents an active client connection.
// It maintains the state of the connection, including the underlying network connection,
// authentication context, and the associated backend connection lease.
type Session struct {
	id uint64

	downstream types.DownstreamClientInterface
	upstream   types.UpstreamClientInterface
	poolMode   config.PoolMode

	upstreamCh    chan pgproto3.BackendMessage  // delivers upstream messages to the main loop
	upstreamAck   chan struct{}                 // ack: main loop has finished with the upstream message buffer
	downstreamCh  chan pgproto3.FrontendMessage // delivers downstream messages to the main loop
	downstreamAck chan struct{}                 // ack: main loop has finished with the downstream message buffer
	errCh         chan error

	cancel       context.CancelFunc
	closeOnce    sync.Once
	ctx          context.Context
	lastActive   atomic.Int64 // unix nanoseconds; avoids atomic.Value boxing allocation on hot path
	log          *zap.Logger
	loopsStarted bool
	srv          types.ProxyInterface
	startMu      sync.Mutex
	wg           sync.WaitGroup

	upstreamCtx    context.Context
	upstreamCancel context.CancelFunc
	upstreamDone   chan struct{}

	releaseCh chan struct{}

	// M4: compat-port BEGIN buffering.
	// BEGIN itself does not trigger classification — it has no traffic-type
	// signal.  The session defers the lease until the next statement.
	pendingBegin pgproto3.FrontendMessage

	// M4: When a buffered BEGIN is replayed to the upstream, the upstream
	// replies with CommandComplete + ReadyForQuery.  The client already
	// received a synthetic response, so the upstream's reply is swallowed.
	swallowingBeginResponse bool

	// M4 compat-port state (items 3 and 4 from M4.md)
	compatTxState               classifier.TxState
	compatTransactionFirstClass classifier.Type
	compatLeasedNodeID          string
	compatLeasedRole            string
	postWriteWindow             PostWriteWindow
	awaitingRollbackResult      bool
	compatPrepared              map[string]classifier.Class
}

var globalSessionID atomic.Uint64

// Initialize creates a new Session instance for a given client connection.
// It assigns a unique session ID, sets up logging, and prepares the session context.
func Initialize(srv types.ProxyInterface, downstream types.DownstreamClientInterface) *Session {
	id := globalSessionID.Add(1)

	logger := srv.GetLogger().With(zap.Uint64("session_id", id))

	ctx, cancel := context.WithCancel(srv.GetContext())

	s := &Session{
		id:            id,
		cancel:        cancel,
		ctx:           ctx,
		downstream:    downstream,
		downstreamCh:  make(chan pgproto3.FrontendMessage),
		downstreamAck: make(chan struct{}),
		errCh:         make(chan error, 1),
		log:           logger,
		srv:           srv,
		upstreamCh:    make(chan pgproto3.BackendMessage),
		upstreamAck:   make(chan struct{}),
		releaseCh:     make(chan struct{}, 1),
	}

	s.lastActive.Store(time.Now().UnixNano())

	return s
}

// Run starts the main event loop for the session.
// It performs the initial protocol handshake and then continuously receives
// and processes messages from the client until the connection is closed.
func (sess *Session) Run() {
	defer sess.Close()
	defer sess.releaseUpstream()

	// Handle initial connection sequence (SSL and Authentication)
	user, password, err := sess.downstream.Startup(sess.srv.GetAuthMethod())
	if err != nil {
		sess.log.Info("client connection startup failed", zap.Error(err))
		return
	}

	if authErr := sess.authenticate(user, password); authErr != nil {
		code := types.CodeFromErr(authErr, "28P01")

		sess.log.Info("connection setup failed", zap.String("user", user), zap.String("code", code), zap.Error(authErr))
		if err := sess.CloseWithError("FATAL", code, authErr.Error()); err != nil {
			sess.log.Warn("failed to close connection", zap.Error(err))
		}

		return
	}

	sess.poolMode = sess.srv.GetConfig().ServerConfig.PoolMode

	// M3: Use port-specific pool mode when available and explicitly configured
	if portCfg, ok := sess.srv.GetConfig().ServerConfig.Ports[sess.srv.Port()]; ok && portCfg.PoolMode != "" {
		if m, ok := config.PoolModeFromString(portCfg.PoolMode); ok {
			sess.poolMode = m
		}
	}

	// M4: Initialize compat-port state.
	if sess.srv.Port() == "compat" {
		sess.compatPrepared = make(map[string]classifier.Class)
		// Default post-write window is 1000ms per M4.md §4; only an
		// explicit 0 in config disables it.
		sess.postWriteWindow.DurationMs = 1000
		if portCfg, ok := sess.srv.GetConfig().ServerConfig.Ports["compat"]; ok {
			sess.postWriteWindow.DurationMs = portCfg.PostWriteWindowMs
		}
	}

	if sess.poolMode == config.PoolModeSession {
		if err := sess.acquireUpstream(); err != nil {
			code := types.CodeFromErr(err, "53300")
			sess.log.Info("connection setup failed", zap.String("user", user), zap.String("code", code), zap.Error(err))
			if err := sess.CloseWithError("FATAL", code, err.Error()); err != nil {
				sess.log.Warn("failed to close connection", zap.Error(err))
			}
			return
		}
	}

	if err := sess.downstream.Send(&pgproto3.AuthenticationOk{}); err != nil {
		sess.log.Warn("failed to send AuthenticationOk", zap.Error(err))
		return
	}

	if err := sess.downstream.Handshake(); err != nil {
		sess.log.Error("handshake error", zap.Error(err))
		return
	}

	// I didn't like this solution, but couldn't find a better one for now.
	sess.startMu.Lock()

	if sess.ctx.Err() != nil {
		sess.startMu.Unlock()
		return
	}

	sess.loopsStarted = true

	sess.wg.Go(sess.downstreamReadLoop)

	sess.startMu.Unlock()

	sess.log.Debug("session run loop started")
	for {
		select {
		case msg := <-sess.upstreamCh:
			sess.lastActive.Store(time.Now().UnixNano())
			if ce := sess.log.Check(zap.DebugLevel, "upstream message received"); ce != nil {
				ce.Write(zap.Any("message", msg))
			}

			// M4: Swallow upstream BEGIN response when BEGIN was buffered.
			// The client already received a synthetic CommandComplete + RFQ.
			// Swallow everything until the next ReadyForQuery so the client
			// does not see duplicates.  ParameterStatus messages that appear
			// interleaved are also swallowed; they are re-sent on the next
			// statement.
			//
			// The flag clears on ANY ReadyForQuery, not only RFQ{T}.  If the
			// upstream BEGIN failed (RFQ{I} or RFQ{E}), we surface an
			// ErrorResponse to the client because it was told it was in a
			// transaction by the synthetic response.  This lets the client
			// recover with ROLLBACK rather than hanging.
			if sess.swallowingBeginResponse {
				if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
					sess.swallowingBeginResponse = false
					if rfq.TxStatus != 'T' {
						// Upstream BEGIN failed.  The client thinks it is in
						// a transaction; surface an error and reset its view.
						sess.log.Warn("upstream BEGIN failed during compat-port replay",
							zap.Uint8("tx_status", rfq.TxStatus))
						if err := sess.downstream.Send(&pgproto3.ErrorResponse{
							Severity: "ERROR",
							Code:     "25P02",
							Message:  "[grunyas] upstream BEGIN failed; transaction aborted",
						}); err != nil {
							sess.log.Warn("failed to send BEGIN-failed ErrorResponse", zap.Error(err))
						}
						if err := sess.downstream.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'}); err != nil {
							sess.log.Warn("failed to send RFQ after BEGIN-failed", zap.Error(err))
						}
						if err := sess.downstream.Flush(); err != nil {
							sess.log.Warn("failed to flush BEGIN-failed response", zap.Error(err))
							return
						}
					}
				}
				select {
				case sess.upstreamAck <- struct{}{}:
				case <-sess.ctx.Done():
					return
				}
				continue
			}

			if err := sess.downstream.Send(msg); err != nil {
				sess.log.Error("failed to send message", zap.Error(err))
				return
			}

			// Flush the accumulated response batch at protocol boundaries so
			// the client sees complete responses without waiting for the next
			// message. RFQ terminates a Sync response; ErrorResponse surfaces
			// fatal errors immediately. CommandComplete/PortalSuspended end an
			// Execute; NoData/RowDescription end a Describe. These are needed
			// so that Flush responses (which have no RFQ) are visible to the
			// client before the subsequent Sync.
			switch msg.(type) {
			case *pgproto3.ReadyForQuery, *pgproto3.ErrorResponse,
				*pgproto3.CommandComplete, *pgproto3.PortalSuspended,
				*pgproto3.NoData, *pgproto3.RowDescription,
				*pgproto3.CloseComplete, *pgproto3.ParseComplete,
				*pgproto3.BindComplete:
				if err := sess.downstream.Flush(); err != nil {
					sess.log.Error("failed to flush downstream", zap.Error(err))
					return
				}
			}

			// M4: Compat-port transaction-state tracking.
			if sess.srv.Port() == "compat" && sess.poolMode == config.PoolModeTransaction {
				if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok {
					sess.handleCompatReadyForQuery(rfq)
				}
			}

			shouldDetach := false
			if sess.poolMode == config.PoolModeTransaction {
				if rfq, ok := msg.(*pgproto3.ReadyForQuery); ok && rfq.TxStatus == 'I' {
					shouldDetach = true
				}
			}

			if !shouldDetach {
				// Signal read loop that buffer can be reused
				select {
				case sess.upstreamAck <- struct{}{}:
				case <-sess.ctx.Done():
					return
				}
			} else {
				sess.releaseUpstream()
			}

		case msg, ok := <-sess.downstreamCh:
			if !ok {
				sess.log.Debug("downstream channel closed")
				return
			}

			sess.lastActive.Store(time.Now().UnixNano())

			if _, ok := msg.(*pgproto3.Terminate); ok {
				sess.log.Info("client terminated session")
				return
			}

			if ce := sess.log.Check(zap.DebugLevel, "downstream message received"); ce != nil {
				ce.Write(zap.Any("message", msg))
			}

			// Extract SQL for classification (used by read-port enforcement and compat port)
			sql := ""
			if q, ok := msg.(*pgproto3.Query); ok {
				sql = q.String
			} else if p, ok := msg.(*pgproto3.Parse); ok {
				sql = p.Query
			}

			// M3: Read-port contract enforcement
			if sess.srv.Port() == "read" {
				sessState := &classifier.SessionState{Port: "read", TxState: classifier.TxIdle}
				cls := classifier.Classify(classifier.Statement{SQL: sql}, sessState)
				if cls.Type == classifier.TypeWrite {
					sess.log.Info("write attempted on read port, rejecting",
						zap.String("port", sess.srv.Port()))
					if err := sess.downstream.Send(&pgproto3.ErrorResponse{
						Severity: "ERROR",
						Code:     "25006",
						Message:  "[grunyas] write attempted on read port",
					}); err != nil {
						sess.log.Warn("failed to send read-port rejection", zap.Error(err))
					}
					if err := sess.downstream.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'}); err != nil {
						sess.log.Warn("failed to send ReadyForQuery after rejection", zap.Error(err))
					}
					if err := sess.downstream.Flush(); err != nil {
						sess.log.Warn("failed to flush after read-port rejection", zap.Error(err))
						return
					}

					sess.srv.RejectReadPortWrite(sql, string(sess.poolMode))

					select {
					case sess.downstreamAck <- struct{}{}:
					case <-sess.ctx.Done():
						return
					}
					continue
				}
			}

			// M4: Compat port uses SQL-aware upstream acquisition for classification-based routing.
			// BEGIN itself does not trigger classification — it has no traffic-type signal.
			// The session buffers BEGIN and defers the lease until the next statement.
			//
			// Because the client blocks waiting for ReadyForQuery after BEGIN,
			// we synthesize the response immediately.  The buffered BEGIN is
			// replayed to the chosen upstream before the next statement; the
			// upstream's response is swallowed so the client does not see a
			// duplicate RFQ.
			//
			// Only simple-query BEGIN is buffered: a Parse{query: "BEGIN"} via
			// the extended protocol expects ParseComplete (after Sync), not
			// CommandComplete + RFQ, so we cannot synthesize the same response
			// for it.  Extended-protocol BEGIN falls through to the normal
			// path and leases the primary (conservative — see M4.md §2 hook
			// table; Parse-driven BEGIN is rare and the conservative fallback
			// is correct).
			//
			// Multi-statement batches (e.g. "BEGIN; SELECT 1; COMMIT;") also
			// fall through: isTransactionStart only matches a bare BEGIN.  The
			// whole batch is then classified by its first keyword (BEGIN →
			// write) and runs on the primary.  Acceptable for M4; multi-
			// statement BEGIN batches are uncommon in compat-port workloads.
			_, isQuery := msg.(*pgproto3.Query)
			if isQuery && sess.srv.Port() == "compat" && isTransactionStart(sql) && sess.pendingBegin == nil {
				sess.pendingBegin = msg
				if err := sess.downstream.Send(&pgproto3.CommandComplete{CommandTag: []byte("BEGIN")}); err != nil {
					sess.log.Warn("failed to send synthetic CommandComplete for BEGIN", zap.Error(err))
					return
				}
				if err := sess.downstream.Send(&pgproto3.ReadyForQuery{TxStatus: 'T'}); err != nil {
					sess.log.Warn("failed to send synthetic ReadyForQuery for BEGIN", zap.Error(err))
					return
				}
				if err := sess.downstream.Flush(); err != nil {
					sess.log.Warn("failed to flush synthetic BEGIN response", zap.Error(err))
					return
				}
				select {
				case sess.downstreamAck <- struct{}{}:
				case <-sess.ctx.Done():
					return
				}
				continue
			}

			compatHandled := false
			var acquireErr error
			if sess.srv.Port() == "compat" {
				// Handle ALL message types through handleCompatSQL so the
				// failed-tx short-circuit catches Bind/Execute/Sync/Flush
				// in addition to Query and Parse.  M4 §3.
				compatHandled, acquireErr = sess.handleCompatSQL(msg, sql)
			} else {
				acquireErr = sess.acquireUpstream()
			}
			if acquireErr != nil {
				code := types.CodeFromErr(acquireErr, "53300")
				sess.log.Info("failed to acquire upstream", zap.String("code", code), zap.Error(acquireErr))

				// M4: If a BEGIN was buffered, the client already received a
				// synthetic RFQ{T} — it thinks it is in a transaction.  Surface
				// a non-fatal ErrorResponse + RFQ{I} so the client unwinds its
				// view of the transaction cleanly, then drop pendingBegin and
				// continue the session (the next statement may succeed).
				if sess.pendingBegin != nil {
					sess.pendingBegin = nil
					if err := sess.downstream.Send(&pgproto3.ErrorResponse{
						Severity: "ERROR",
						Code:     code,
						Message:  "[grunyas] " + acquireErr.Error(),
					}); err != nil {
						sess.log.Warn("failed to send ErrorResponse after acquire-fail post-BEGIN", zap.Error(err))
					}
					if err := sess.downstream.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'}); err != nil {
						sess.log.Warn("failed to send RFQ after acquire-fail post-BEGIN", zap.Error(err))
					}
					if err := sess.downstream.Flush(); err != nil {
						sess.log.Warn("failed to flush acquire-fail response", zap.Error(err))
						return
					}
					select {
					case sess.downstreamAck <- struct{}{}:
					case <-sess.ctx.Done():
						return
					}
					continue
				}

				if err := sess.CloseWithError("FATAL", code, acquireErr.Error()); err != nil {
					sess.log.Warn("failed to close connection", zap.Error(err))
				}
				return
			}

			if !compatHandled {
				// If a BEGIN was buffered, send it before the current message.
				// The upstream's response will be swallowed in the upstreamCh
				// handler because the client already got a synthetic response.
				if sess.pendingBegin != nil {
					if _, err := messaging.Process(sess.pendingBegin, sess.upstream, sess.log); err != nil {
						sess.log.Error("error processing buffered BEGIN", zap.Error(err))
						return
					}
					sess.swallowingBeginResponse = true
					sess.pendingBegin = nil
				}

				switchMode, err := messaging.Process(msg, sess.upstream, sess.log)
				if err != nil {
					sess.log.Error("error processing message", zap.Error(err))
					return
				}
				if switchMode {
					sess.switchToSessionMode("session state detected")
				}

				// Flush the accumulated request batch at protocol boundaries so
				// the backend starts processing. Extended protocol messages
				// (Parse, Bind, Describe, Execute, Close) are only processed
				// when Sync or Flush is received; simple Query messages trigger
				// immediate processing. CopyDone and CopyFail terminate a
				// COPY IN stream and must flush so the backend can finalize.
				switch msg.(type) {
				case *pgproto3.Query, *pgproto3.Sync, *pgproto3.Flush, *pgproto3.CopyDone, *pgproto3.CopyFail:
					if err := sess.upstream.Flush(); err != nil {
						sess.log.Error("failed to flush upstream", zap.Error(err))
						return
					}
				}
			}

			// Signal read loop that buffer can be reused
			select {
			case sess.downstreamAck <- struct{}{}:
			case <-sess.ctx.Done():
				return
			}

		case <-sess.errCh:
			return

		case <-sess.releaseCh:
			sess.releaseUpstream()

		case <-sess.ctx.Done():
			sess.log.Info("session context closed")
			return
		}
	}
}

// ID returns the unique identifier for this session.
func (sess *Session) ID() uint64 {
	return sess.id
}

// LastActive returns the time of the most recent activity in this session.
// This is used by the idle sweeper to determine if the session should be terminated.
func (sess *Session) LastActive() time.Time {
	ns := sess.lastActive.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Close gracefully terminates the session.
// It releases any held backend resources and closes the client connection.
// It is safe to call Close multiple times.
func (sess *Session) Close() {
	sess.closeOnce.Do(func() {
		select {
		case sess.releaseCh <- struct{}{}:
		default:
		}

		sess.log.Debug("cancelling session context")
		sess.cancel()

		sess.log.Info("closing client connection")
		_ = sess.downstream.Close()

		sess.startMu.Lock()

		started := sess.loopsStarted

		sess.startMu.Unlock()

		if started {
			sess.wg.Wait()
		}
	})
}

// CloseWithError sends a PostgreSQL ErrorResponse to the client and then closes the session.
// This is used to terminate sessions due to fatal errors (e.g. idle timeout, shutting down).
func (sess *Session) CloseWithError(severity, code, message string) error {
	sess.log.Debug("closing session with error",
		zap.String("severity", severity),
		zap.String("code", code),
		zap.String("message", message))

	// Ensure we close the session even if sending the error fails.
	defer sess.Close()

	// Watchdog to prevent hanging forever if the client stops reading.
	timer := time.AfterFunc(5*time.Second, func() {
		sess.log.Warn("CloseWithError timed out, forcing session closure")
		sess.Close()
	})
	defer timer.Stop()

	if err := sess.downstream.Send(&pgproto3.ErrorResponse{
		Severity: severity,
		Code:     code,
		Message:  message,
	}); err != nil {
		sess.log.Warn("failed to buffer error message before closing", zap.Error(err))
	}
	// Flush synchronously — CloseWithError is the last thing the client will
	// see on this connection, so we must push the error out of the buffer
	// before Close() tears down the socket.
	if err := sess.downstream.Flush(); err != nil {
		sess.log.Warn("failed to flush error message before closing", zap.Error(err))
	}

	return nil
}

func (sess *Session) authenticate(user, password string) error {
	switch sess.srv.GetAuthMethod() {
	case types.AuthMD5:
		salt := sess.downstream.MD5Salt()
		return sess.srv.AuthenticateMD5(user, password, salt)
	case types.AuthScramSHA256:
		scramSession, err := sess.srv.NewSCRAMSession()
		if err != nil {
			return &types.ProxyError{Code: "28P01", Message: err.Error()}
		}
		return sess.downstream.SASLExchange(scramSession.Step)
	default:
		return sess.srv.Authenticate(user, password)
	}
}

func (sess *Session) acquireUpstream() error {
	if sess.upstream != nil {
		return nil
	}
	upstream, err := sess.srv.AcquireUpstream()
	if err != nil {
		return err
	}

	upstreamCtx, upstreamCancel := context.WithCancel(sess.ctx)
	sess.upstream = upstream
	sess.upstreamCtx = upstreamCtx
	sess.upstreamCancel = upstreamCancel
	done := make(chan struct{})
	sess.upstreamDone = done

	sess.wg.Go(func() {
		sess.upstreamReadLoop(upstreamCtx, upstream)
		close(done)
	})

	return nil
}

func (sess *Session) releaseUpstream() {
	upstream := sess.upstream
	cancel := sess.upstreamCancel
	done := sess.upstreamDone
	if upstream == nil {
		return
	}
	sess.upstream = nil
	sess.upstreamCancel = nil
	sess.upstreamCtx = nil
	sess.upstreamDone = nil

	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}

	sess.log.Info("releasing connection back to pool")
	if err := upstream.Release(); err != nil {
		sess.log.Error("failed to release connection", zap.Error(err))
		sess.log.Info("killing connection")
		if err := upstream.Kill(); err != nil {
			sess.log.Error("failed to kill connection", zap.Error(err))
		}
	}
}

func (sess *Session) switchToSessionMode(reason string) {
	if sess.poolMode == config.PoolModeSession {
		return
	}
	sess.poolMode = config.PoolModeSession
	sess.log.Info("switching to session mode", zap.String("reason", reason))
}

func (sess *Session) upstreamReadLoop(ctx context.Context, upstream types.UpstreamClientInterface) {
	for {
		msg, err := upstream.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil && sess.ctx.Err() == nil {
				sess.log.Error("upstream receive error", zap.Error(err))
				select {
				case sess.errCh <- err:
				default:
				}
			}

			return
		}

		select {
		case sess.upstreamCh <- msg:
		case <-ctx.Done():
			return
		}

		// Wait for main loop to signal it's done with the buffer
		select {
		case <-sess.upstreamAck:
		case <-ctx.Done():
			return
		}
	}
}

// downstreamReadLoop reads messages from the downstream connection (client) and sends them to the session's downstream channel.
func (sess *Session) downstreamReadLoop() {
	defer close(sess.downstreamCh)

	for {
		msg, err := sess.downstream.Receive()
		if err != nil {
			if sess.ctx.Err() == nil {
				sess.log.Error("downstream receive error", zap.Error(err))
				select {
				case sess.errCh <- err:
				default:
				}
			}

			return
		}

		select {
		case sess.downstreamCh <- msg:
		case <-sess.ctx.Done():
			return
		}

		// Wait for main loop to signal it's done with the buffer
		select {
		case <-sess.downstreamAck:
		case <-sess.ctx.Done():
			return
		}
	}
}

// isTransactionStart returns true for SQL statements that open a new
// transaction boundary (BEGIN, START TRANSACTION).  These do not carry
// a traffic-type signal and must not trigger lease acquisition on the
// compat port.  M4.md §2.
func isTransactionStart(sql string) bool {
	s := strings.TrimSpace(strings.ToLower(sql))
	if s == "begin" || strings.HasPrefix(s, "begin ") {
		return true
	}
	if s == "start transaction" || strings.HasPrefix(s, "start transaction ") {
		return true
	}
	return false
}

// isCommit returns true for a top-level COMMIT statement.
func isCommit(sql string) bool {
	s := strings.TrimSpace(strings.ToLower(sql))
	return s == "commit" || strings.HasPrefix(s, "commit ")
}

// isRollback returns true for a top-level ROLLBACK statement.
func isRollback(sql string) bool {
	s := strings.TrimSpace(strings.ToLower(sql))
	return s == "rollback" || strings.HasPrefix(s, "rollback ")
}

// PostWriteWindow is a per-connection timer that opens at the COMMIT of any
// write-classified transaction on a compat-port connection.
type PostWriteWindow struct {
	OpenedAt   time.Time
	DurationMs int
}

// Active reports whether the window is currently open.
func (w *PostWriteWindow) Active(now time.Time) bool {
	return w.DurationMs > 0 &&
		!w.OpenedAt.IsZero() &&
		now.Sub(w.OpenedAt) < time.Duration(w.DurationMs)*time.Millisecond
}

// RemainingMs returns the remaining window duration in milliseconds.
func (w *PostWriteWindow) RemainingMs(now time.Time) int {
	if !w.Active(now) {
		return 0
	}
	elapsed := now.Sub(w.OpenedAt)
	return w.DurationMs - int(elapsed/time.Millisecond)
}

// handleCompatSQL implements M4 items 3 and 4 for the compat port.
// It classifies the statement, checks for mid-transaction reclassification,
// manages the post-write window, and acquires upstream leases.
// Returns (handled, error). If handled is true, messaging.Process should be skipped.
func (sess *Session) handleCompatSQL(msg pgproto3.FrontendMessage, sql string) (bool, error) {
	// Failed-transaction short-circuit (M4 §3).
	// Checked before the type switch so it catches ALL message types
	// including Bind, Execute, Sync, Flush, Close, Describe.
	if sess.compatTxState == classifier.TxInFailedTransaction {
		if q, ok := msg.(*pgproto3.Query); ok && isRollback(q.String) {
			// Forward ROLLBACK; state resets when RFQ 'I' arrives.
			sess.awaitingRollbackResult = true
			return false, nil
		}
		if _, ok := msg.(*pgproto3.Query); ok {
			// Non-ROLLBACK Query: full Query-style ErrorResponse + RFQ{E}.
			if sess.sendCompatError("25P02", "current transaction is aborted, commands ignored until end of transaction block", 'E') {
				return false, nil
			}
			return true, nil
		}
		if _, ok := msg.(*pgproto3.Sync); ok {
			// Sync: RFQ{E} only.  ErrorResponse was already sent at the
			// reclassification rejection (Parse or Query) that triggered
			// the failed-tx state.
			if err := sess.downstream.Send(&pgproto3.ReadyForQuery{TxStatus: 'E'}); err != nil {
				sess.log.Warn("failed to send RFQ in failed-tx Sync", zap.Error(err))
			}
			if err := sess.downstream.Flush(); err != nil {
				sess.log.Warn("failed to flush failed-tx Sync response", zap.Error(err))
				return false, nil
			}
			return true, nil
		}
		if _, ok := msg.(*pgproto3.Terminate); ok {
			// Let Terminate fall through to close the session.
			return false, nil
		}
		// Bind, Execute, Describe, Close, Flush, Parse, CopyData:
		// silently consume per Postgres extended-protocol behavior.
		// The ErrorResponse was already sent at the rejection point;
		// every message between the error and Sync is swallowed.
		return true, nil
	}

	// Build classifier session state (non-failed transactions only).
	cs := &classifier.SessionState{
		Port:     "compat",
		TxState:  sess.compatTxState,
		Prepared: sess.compatPrepared,
	}

	var stmt classifier.Statement
	switch m := msg.(type) {
	case *pgproto3.Query:
		stmt = classifier.Statement{SQL: sql}
	case *pgproto3.Parse:
		stmt = classifier.Statement{Name: m.Name, SQL: sql}
	default:
		// Bind, Execute, Sync, etc. have no SQL text — no special compat
		// handling.  Acquire an upstream if one does not exist yet so the
		// main loop does not panic on nil upstream (M4 §3.g).
		if sess.upstream == nil {
			now := time.Now()
			pwwActive := sess.postWriteWindow.Active(now)
			pwwRemaining := sess.postWriteWindow.RemainingMs(now)
			lease, err := sess.srv.AcquireUpstreamLease("", pwwActive, pwwRemaining)
			if err != nil {
				return false, err
			}
			sess.setUpstreamLease(lease)
			sess.compatLeasedNodeID = lease.NodeID
			sess.compatLeasedRole = lease.Role
		}
		return false, nil
	}

	cls := classifier.Classify(stmt, cs)

	// Parse-time classification: cache and check reclassification.
	if _, isParse := msg.(*pgproto3.Parse); isParse {
		if sess.upstream != nil && sess.compatLeasedRole == "replica" && cls.Type == classifier.TypeWrite && !isCommit(sql) && !isRollback(sql) {
			sess.srv.RejectCompatReclassification(sql, string(sess.poolMode))
			sess.compatTxState = classifier.TxInFailedTransaction
			// Parse-style rejection: ErrorResponse only, no RFQ.
			// Extended-protocol clients expect RFQ after Sync, not after Parse.
			if err := sess.downstream.Send(&pgproto3.ErrorResponse{
				Severity: "ERROR",
				Code:     "25006",
				Message:  "[grunyas] write attempted in transaction leased to replica (compat port)",
			}); err != nil {
				sess.log.Warn("failed to send Parse-time reclassification error", zap.Error(err))
			}
			if err := sess.downstream.Flush(); err != nil {
				sess.log.Warn("failed to flush Parse-time reclassification error", zap.Error(err))
				return false, nil
			}
			return true, nil
		}
		if sess.upstream == nil {
			now := time.Now()
			pwwActive := sess.postWriteWindow.Active(now)
			pwwRemaining := sess.postWriteWindow.RemainingMs(now)
			lease, err := sess.srv.AcquireUpstreamLease(sql, pwwActive, pwwRemaining)
			if err != nil {
				return false, err
			}
			sess.setUpstreamLease(lease)
			sess.compatLeasedNodeID = lease.NodeID
			sess.compatLeasedRole = lease.Role
			sess.compatTransactionFirstClass = cls.Type
			if cls.Type == classifier.TypeWrite {
				sess.compatTxState = classifier.TxInTransactionWrite
			} else {
				sess.compatTxState = classifier.TxInTransactionRead
			}
			if isRollback(sql) {
				sess.awaitingRollbackResult = true
			}
		} else {
			// Parse with existing upstream: update tx state for
			// transaction-state inheritance (M4 §3.k).
			if cls.Type == classifier.TypeWrite {
				sess.compatTxState = classifier.TxInTransactionWrite
			} else if sess.compatTxState == classifier.TxIdle {
				sess.compatTxState = classifier.TxInTransactionRead
			}
			if isRollback(sql) {
				sess.awaitingRollbackResult = true
			}
		}
		return false, nil
	}

	// In transaction: check mid-transaction reclassification (M4 §3).
	if sess.upstream != nil {
		if sess.compatLeasedRole == "replica" && cls.Type == classifier.TypeWrite && !isCommit(sql) && !isRollback(sql) {
			sess.srv.RejectCompatReclassification(sql, string(sess.poolMode))
			sess.compatTxState = classifier.TxInFailedTransaction
			if sess.sendCompatError("25006", "write attempted in transaction leased to replica (compat port)", 'E') {
				return false, nil
			}
			return true, nil
		}
		// Update tx state for transaction-state inheritance.
		if cls.Type == classifier.TypeWrite {
			sess.compatTxState = classifier.TxInTransactionWrite
		} else if sess.compatTxState == classifier.TxIdle {
			sess.compatTxState = classifier.TxInTransactionRead
		}
		// Track ROLLBACK for post-write window lifecycle.
		if isRollback(sql) {
			sess.awaitingRollbackResult = true
		}
		return false, nil
	}

	// Need to acquire upstream for this statement.
	now := time.Now()
	pwwActive := sess.postWriteWindow.Active(now)
	pwwRemaining := sess.postWriteWindow.RemainingMs(now)

	lease, err := sess.srv.AcquireUpstreamLease(sql, pwwActive, pwwRemaining)
	if err != nil {
		return false, err
	}
	sess.setUpstreamLease(lease)
	sess.compatLeasedNodeID = lease.NodeID
	sess.compatLeasedRole = lease.Role
	sess.compatTransactionFirstClass = cls.Type
	if cls.Type == classifier.TypeWrite {
		sess.compatTxState = classifier.TxInTransactionWrite
	} else {
		sess.compatTxState = classifier.TxInTransactionRead
	}

	// Track ROLLBACK for window lifecycle.
	if isRollback(sql) {
		sess.awaitingRollbackResult = true
	}

	return false, nil
}

// setUpstreamLease sets the active upstream and starts its read loop.
func (sess *Session) setUpstreamLease(lease *types.UpstreamLease) {
	upstreamCtx, upstreamCancel := context.WithCancel(sess.ctx)
	sess.upstream = lease.Client
	sess.upstreamCtx = upstreamCtx
	sess.upstreamCancel = upstreamCancel
	done := make(chan struct{})
	sess.upstreamDone = done
	sess.wg.Go(func() {
		sess.upstreamReadLoop(upstreamCtx, lease.Client)
		close(done)
	})
}

// handleCompatReadyForQuery updates compat state when a ReadyForQuery arrives
// on a compat-port connection in transaction mode.
func (sess *Session) handleCompatReadyForQuery(rfq *pgproto3.ReadyForQuery) {
	switch rfq.TxStatus {
	case 'I':
		// Transaction ended. Open post-write window for successful write commits.
		if !sess.awaitingRollbackResult &&
			sess.compatTransactionFirstClass == classifier.TypeWrite &&
			sess.compatLeasedRole == "primary" {
			sess.postWriteWindow.OpenedAt = time.Now()
		}
		sess.resetCompatState()
	case 'T':
		// In transaction. State was set when the statement was sent.
	case 'E':
		// Failed transaction.  Reset the first-class so the post-write
		// window does not open when the client eventually sends ROLLBACK.
		sess.compatTxState = classifier.TxInFailedTransaction
		sess.compatTransactionFirstClass = classifier.TypeUnknown
	}
}

// sendCompatError sends an ErrorResponse to the downstream, followed by a
// ReadyForQuery with the given txStatus, and flushes. On flush failure it
// returns true (caller should return/abort). On success it returns false.
func (sess *Session) sendCompatError(code, message string, txStatus byte) bool {
	if err := sess.downstream.Send(&pgproto3.ErrorResponse{
		Severity: "ERROR",
		Code:     code,
		Message:  "[grunyas] " + message,
	}); err != nil {
		sess.log.Warn("failed to send compat error response", zap.Error(err))
	}
	if err := sess.downstream.Send(&pgproto3.ReadyForQuery{TxStatus: txStatus}); err != nil {
		sess.log.Warn("failed to send RFQ after compat error", zap.Error(err))
	}
	if err := sess.downstream.Flush(); err != nil {
		sess.log.Warn("failed to flush compat error response", zap.Error(err))
		return true
	}
	return false
}

// resetCompatState clears all per-transaction compat state after a transaction ends.
func (sess *Session) resetCompatState() {
	sess.compatTxState = classifier.TxIdle
	sess.compatTransactionFirstClass = classifier.TypeUnknown
	sess.compatLeasedNodeID = ""
	sess.compatLeasedRole = ""
	sess.awaitingRollbackResult = false
}



