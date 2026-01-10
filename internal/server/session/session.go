// Package session manages individual client connections to the proxy.
// It handles the lifecycle of a client session, including the initial handshake,
// message routing, and connection teardown.
package session

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

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

	upstreamCh    chan pgproto3.BackendMessage  // delivers upstream messages to the main loop
	upstreamAck   chan struct{}                 // ack: main loop has finished with the upstream message buffer
	downstreamCh  chan pgproto3.FrontendMessage // delivers downstream messages to the main loop
	downstreamAck chan struct{}                 // ack: main loop has finished with the downstream message buffer
	errCh         chan error

	cancel       context.CancelFunc
	closeOnce    sync.Once
	ctx          context.Context
	lastActive   atomic.Value // time.Time
	log          *zap.Logger
	loopsStarted bool
	srv          types.ProxyInterface
	startMu      sync.Mutex
	wg           sync.WaitGroup
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
	}

	s.lastActive.Store(time.Now())

	return s
}

// Run starts the main event loop for the session.
// It performs the initial protocol handshake and then continuously receives
// and processes messages from the client until the connection is closed.
func (sess *Session) Run() {
	defer sess.Close()

	// Handle initial connection sequence (SSL and Authentication)
	user, password, err := sess.downstream.Startup()
	if err != nil {
		sess.log.Info("client connection startup failed", zap.Error(err))
		return
	}

	upstream, err := sess.srv.Authenticate(user, password)
	if err != nil {
		code := "28P01" // Default: invalid_password
		if perr, ok := err.(*types.ProxyError); ok {
			code = perr.Code
		}

		sess.log.Info("connection setup failed", zap.String("user", user), zap.String("code", code), zap.Error(err))
		if err := sess.CloseWithError("FATAL", code, err.Error()); err != nil {
			sess.log.Warn("failed to close connection", zap.Error(err))
		}

		return
	}
	sess.upstream = upstream

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

	sess.wg.Go(sess.upstreamReadLoop)
	sess.wg.Go(sess.downstreamReadLoop)

	sess.startMu.Unlock()

	sess.log.Debug("session run loop started")
	for {
		select {
		case msg, ok := <-sess.upstreamCh:
			if !ok {
				sess.log.Info("upstream connection closed")
				return
			}

			sess.lastActive.Store(time.Now())
			sess.log.Debug("upstream message received", zap.Any("message", msg))

			if err := sess.downstream.Send(msg); err != nil {
				sess.log.Error("failed to send message", zap.Error(err))
				return
			}

			// Signal read loop that buffer can be reused
			select {
			case sess.upstreamAck <- struct{}{}:
			case <-sess.ctx.Done():
				return
			}

		case msg, ok := <-sess.downstreamCh:
			if !ok {
				sess.log.Info("downstream connection closed")
				return
			}

			sess.lastActive.Store(time.Now())
			if _, ok := msg.(*pgproto3.Terminate); ok {
				sess.log.Info("client terminated session")
				return
			}

			if err := messaging.Process(sess.ctx, msg, sess.upstream, sess.downstream, sess.log); err != nil {
				sess.log.Error("error processing message", zap.Error(err))
				return
			}

			sess.log.Debug("downstream message received", zap.Any("message", msg))

			// Signal read loop that buffer can be reused
			select {
			case sess.downstreamAck <- struct{}{}:
			case <-sess.ctx.Done():
				return
			}

		case <-sess.errCh:
			return

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
	v := sess.lastActive.Load()

	if t, ok := v.(time.Time); ok {
		return t
	}

	return time.Time{}
}

// Close gracefully terminates the session.
// It releases any held backend resources and closes the client connection.
// It is safe to call Close multiple times.
func (sess *Session) Close() {
	sess.closeOnce.Do(func() {
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

		if sess.upstream != nil {
			sess.log.Info("releasing connection back to pool")
			err := sess.upstream.Release()
			if err != nil {
				sess.log.Error("failed to release connection", zap.Error(err))

				sess.log.Info("killing connection")
				err := sess.upstream.Kill()
				if err != nil {
					sess.log.Error("failed to kill connection", zap.Error(err))
				}
			}
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
		sess.log.Warn("failed to flush error message before closing", zap.Error(err))
	}

	return nil
}

// upstreamReadLoop reads messages from the upstream connection (backend) and sends them to the session's upstream channel.
func (sess *Session) upstreamReadLoop() {
	defer close(sess.upstreamCh)

	for {
		msg, err := sess.upstream.Receive(sess.ctx)
		if err != nil {
			if sess.ctx.Err() == nil {
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
		case <-sess.ctx.Done():
			return
		}

		// Wait for main loop to signal it's done with the buffer
		select {
		case <-sess.upstreamAck:
		case <-sess.ctx.Done():
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
