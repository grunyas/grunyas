# Idle Sweeper

This proxy closes client sessions that stay idle longer than `server.client_idle_timeout` (seconds). The default is 300s; set it to `0` to disable idle enforcement entirely.

## Lifecycle
- `Proxy.Initialize` builds an `idleSweeper` with the configured timeout.
- Any resource implementing the `types.Expirable` interface can be tracked. Currently, every accepted session is registered via `idle.Track(sess)` and unregistered on exit with `idle.Untrack(sess)`.
- A ticker in `Proxy.idleSweeper` wakes every second and asks the sweeper for expired entries.
- **Parallel Cleanup**: For each expired entry, the proxy spawns a separate goroutine to perform the cleanup. This ensures that if one connection hangs during the "Goodbye" message, it doesn't block the sweeper from closing other idle sessions.

## Data structure and algorithm
- The sweeper keeps a min-heap of `idleEntry` values keyed by a deadline and an `entries` map for O(1) lookups/removals.
- `Track` sets an initial deadline of `now + timeout`, pushes it onto the heap, and skips work entirely when the timeout is `<= 0` (feature off).
- `Expire` (called by the ticker) repeatedly looks at the earliest deadline. For each candidate:
  - If the stored deadline is still in the future, the sweep stops (heap is ordered).
  - Otherwise, it recomputes a fresh deadline from the entry’s `LastActive()` to avoid closing resources that were active after the entry was enqueued.
  - If the refreshed deadline is in the future, it updates the heap entry in place; if it is still past due, the entry is returned for closure and removed from both heap and map.
- `Untrack` removes the entry when a resource is closed normally, keeping the heap clean.

## Activity tracking and Robustness
- `session.Session` records `lastActive` at creation and updates it on every read from the client.
- **Watchdog Timer**: When the sweeper calls `CloseWithError`, a 5-second watchdog timer is started. If the network write for the error response hangs (e.g., client stops reading), the timer force-closes the session, terminating the socket and unblocking the cleanup goroutine.
- Because `Expire` revalidates against `LastActive()`, sporadic activity is respected even if the entry’s prior deadline was stale.
