// Package safe contains small concurrency helpers for production Go services.
//
// The package has no third-party dependencies. Its Locked[T] type provides
// mutex-protected values with closure-based compound mutations.
//
// Panics are never recovered. A panic in any goroutine crashes the process.
// This follows Google's Go style guidance: recovering panics to avoid crashes
// masks corrupted state. The orchestrator handles restart.
package safe
