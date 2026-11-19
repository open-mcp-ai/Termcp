// Package daemon manages a detached service across separate CLI invocations.
// The application defines how to recognize and stop its instance; the manager
// owns spawning, readiness deadlines, log redirection and lifecycle locking.
// The directory holding its control files must be private to the caller.
package daemon
