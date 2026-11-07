/*
Copyright 2026 lburgazzoli.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package cluster

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// runBounded executes a context-less provider operation under an explicit
// deadline. This is the gate-recorded practical method for the pinned kind
// library: kind's Provider.Create/Delete take no context,
// so a timeout cannot cancel the underlying work. The wrapper therefore:
//
//   - returns the operation's own error when it completes inside the bound;
//   - on caller-context cancellation or deadline, reports the cancellation
//     without claiming the operation stopped;
//   - on bound expiry, reports a timeout and states that the operation may
//     still be running and must be cleaned up explicitly (by cluster name).
//
// Callers must treat any non-nil result as "cluster state unknown" and
// still attempt Delete during cleanup.
func runBounded(ctx context.Context, bound time.Duration, op string, fn func() error) error {
	if fn == nil {
		return fmt.Errorf("%s: no operation provided", op)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: not started: %w", op, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- fn()
	}()

	timer := time.NewTimer(bound)
	defer timer.Stop()

	select {
	case err := <-done:
		if err != nil {
			return fmt.Errorf("%s: %w", op, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: caller context done (%w): kind's provider API takes no context, "+
			"the operation may still be running and must be cleaned up by cluster name", op, ctx.Err())
	case <-timer.C:
		return fmt.Errorf("%s: timed out after %s: kind's provider API takes no context, "+
			"the operation could not be cancelled and may still be running; "+
			"Stop/delete must still be attempted by cluster name", op, bound)
	}
}

// errUnsupportedRuntime is separated so both the options validation and the
// provider construction report it uniformly.
func errUnsupportedRuntime(runtime Runtime) error {
	return errors.New("unsupported runtime: " + string(runtime))
}
