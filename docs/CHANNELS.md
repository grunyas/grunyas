# Channel Architecture

This document describes the channel-based message routing in the session layer.

## Overview

The proxy uses a full-duplex channel architecture to handle bidirectional PostgreSQL protocol messages. Each session has dedicated read loops for upstream (database) and downstream (client) connections.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              Session                                        │
│                                                                             │
│  PostgreSQL ──(BackendMessage)──► upstreamReadLoop() ──► upstreamCh ──┐    │
│       ▲                                    │                          │    │
│       │                              upstreamAck ◄────────────────────┤    │
│       │                                                               ▼    │
│       │                                                          Main Loop │
│       │                                                               │    │
│       │                              downstreamAck ◄──────────────────┤    │
│       │                                    ▲                          │    │
│  Client ◄──(BackendMessage)─────────────┬──┴──────────── downstreamCh ◄┘    │
│       │                                 │                                   │
│       └──(FrontendMessage)──► downstreamReadLoop() ──────────────────►     │
└─────────────────────────────────────────────────────────────────────────────┘
```

## The Buffer Reuse Problem

The `pgproto3` library reuses internal buffers when decoding messages. When `Receive()` returns a message, the byte slices within that message (e.g., `DataRow.Values`, `CommandComplete.CommandTag`) point to a shared buffer that gets overwritten on the next `Receive()` call.

This creates a data race when messages are passed between goroutines via channels:

1. Read loop calls `Receive()` → gets message with data in shared buffer
2. Read loop sends message to channel
3. Read loop calls `Receive()` again → **overwrites the buffer**
4. Main loop receives message → **data is corrupted**

## Solution: Ack Channel Synchronization

Instead of cloning messages (which has CPU and allocation overhead), we use **ack channels** to synchronize access to the shared buffer.

### How It Works

```go
// Read loop (upstream or downstream)
for {
    msg, err := conn.Receive()  // Decode into shared buffer
    if err != nil { return }
    
    // Send message to main loop
    select {
    case ch <- msg:
    case <-ctx.Done():
        return
    }
    
    // WAIT for main loop to signal it's done with the buffer
    select {
    case <-ackCh:
    case <-ctx.Done():
        return
    }
    // Now safe to call Receive() again
}

// Main loop
for {
    select {
    case msg := <-upstreamCh:
        downstream.Send(msg)  // Use the message
        upstreamAck <- struct{}{}  // Signal: buffer can be reused
        
    case msg := <-downstreamCh:
        process(msg)  // Use the message
        downstreamAck <- struct{}{}  // Signal: buffer can be reused
    }
}
```

### Why Unbuffered Channels

With ack synchronization, only one message can be "in flight" at a time. The read loop is blocked waiting for the ack before it can read the next message. Therefore:

- Message channels are **unbuffered** (no need for buffering)
- Ack channels are **unbuffered** (simple signal)

### Performance Characteristics

| Aspect | Impact |
|--------|--------|
| **Allocations** | Zero per message (no cloning) |
| **CPU overhead** | Minimal (empty struct channel send) |
| **Throughput** | Slightly reduced pipelining |
| **Latency** | Negligible for I/O-bound proxy |

The reduced pipelining is acceptable because:
1. We're I/O-bound (network latency dominates)
2. Kernel TCP buffers (128KB+) absorb any micro-delays
3. The ack overhead is just an empty struct send

## Alternative Approaches Considered

### 1. Message Cloning (Original)
```go
clone, _ := cloneBackendMessage(msg)
ch <- clone
```
- **Pros**: Simple
- **Cons**: Allocation + CPU overhead for encode/decode

### 2. Manual Deep Copy
```go
switch m := msg.(type) {
case *pgproto3.DataRow:
    clone := &pgproto3.DataRow{Values: copySlices(m.Values)}
}
```
- **Pros**: Faster than encode/decode
- **Cons**: Maintenance burden, must update for new message types

### 3. sync.Cond
- **Cons**: Doesn't integrate with `select`, can't combine with `ctx.Done()`

The ack channel approach was chosen for its simplicity, zero allocations, and clean integration with Go's concurrency patterns.
