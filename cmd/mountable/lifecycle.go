package main

// What ends a mount, and how the mount ends for each.

type event int

const (
	// eventUnmounted: the mount went away underneath (mountable unmount,
	// umount); serving has stopped.
	eventUnmounted event = iota + 1
	// eventSignal: SIGINT or SIGTERM.
	eventSignal
	// eventRevoked: the API refused the session (a refused renewal, or a
	// gateway rejection the API confirmed).
	eventRevoked
	// eventExpired: the certificate ran out without a renewal; the session
	// has ended.
	eventExpired
)

type ending int

const (
	// endFinish: wait (bounded) for pending writes, drop caches, exit.
	endFinish ending = iota + 1
	// endUnmount: unmount cleanly, then finish; abort if the mount is busy.
	endUnmount
	// endAbort: abort the connection at once, without flushing: the gateway
	// no longer accepts the writes, and cached data must stop being served.
	endAbort
)

func endingFor(e event) ending {
	switch e {
	case eventUnmounted:
		return endFinish
	case eventSignal:
		return endUnmount
	default:
		return endAbort
	}
}
