// Package onboard drives a freshly flashed Home Assistant OS gateway from first
// boot to "agent provisioned, claim code in hand" over Home Assistant's stable
// public APIs (the onboarding + auth REST API and the Supervisor API, reached
// through Core's authenticated "supervisor/api" WebSocket command) — never by
// writing HA internal .storage files.
//
// The unit of work is a Step with a check -> act -> verify shape, run by the
// Engine in order. The design goal is robustness through resumability: the
// source of truth for "is this step done?" is the DEVICE, queried fresh at the
// start of every step, not a local progress file. So a run that dies halfway
// (network blip, killed process, a stuck add-on install) is recovered by simply
// re-running the exact same command: each already-satisfied step's Check short-
// circuits it, and the run resumes at the first unfinished step.
package onboard

import (
	"context"
	"fmt"
)

// Step is one idempotent unit of onboarding.
//
//   - Check reports whether the step's goal already holds (queried fresh from the
//     device). When it returns true the engine skips Act/Verify — this is what
//     makes the whole run resumable after a failure. A nil Check means "always
//     act" (rare; used for pure output steps).
//   - Act performs the mutation to reach the goal.
//   - Verify confirms the goal holds after Act, from a fresh device read, so a
//     step never reports success on a silent no-op. A nil Verify trusts Act.
type Step struct {
	Name   string
	Check  func(ctx context.Context, st *State) (done bool, err error)
	Act    func(ctx context.Context, st *State) error
	Verify func(ctx context.Context, st *State) error
}

// Reporter receives progress as the engine runs, so the CLI can render a live,
// human-friendly transcript and tests can assert on the sequence. All methods
// must tolerate being called from a single goroutine in step order.
type Reporter interface {
	// StepStarted fires before a step's Check runs.
	StepStarted(name string)
	// StepSkipped fires when Check reports the goal already holds (resume path).
	StepSkipped(name, reason string)
	// StepCompleted fires after a step acts + verifies successfully.
	StepCompleted(name string)
	// StepFailed fires once with the error that stopped the run at this step.
	StepFailed(name string, err error)
	// Info surfaces incidental progress within a step (e.g. "waiting for Core").
	Info(msg string)
}

// Engine runs a fixed, ordered list of steps against a shared State.
type Engine struct {
	steps    []Step
	reporter Reporter
}

// NewEngine builds an engine. A nil reporter is replaced with a no-op so callers
// (and tests) never have to supply one.
func NewEngine(steps []Step, reporter Reporter) *Engine {
	if reporter == nil {
		reporter = nopReporter{}
	}

	return &Engine{steps: steps, reporter: reporter}
}

// Run executes each step in order, stopping at the first failure. A step failure
// is wrapped in *StepError so the caller can name the failed step and tell the
// operator to re-run to resume. Context cancellation aborts the run promptly.
func (e *Engine) Run(ctx context.Context, st *State) error {
	for _, step := range e.steps {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := e.runStep(ctx, st, step); err != nil {
			stepErr := &StepError{Step: step.Name, Err: err}
			e.reporter.StepFailed(step.Name, err)

			return stepErr
		}
	}

	return nil
}

func (e *Engine) runStep(ctx context.Context, st *State, step Step) error {
	e.reporter.StepStarted(step.Name)

	if step.Check != nil {
		done, err := step.Check(ctx, st)
		if err != nil {
			return fmt.Errorf("check: %w", err)
		}

		if done {
			e.reporter.StepSkipped(step.Name, "already satisfied")

			return nil
		}
	}

	if step.Act != nil {
		if err := step.Act(ctx, st); err != nil {
			return fmt.Errorf("act: %w", err)
		}
	}

	if step.Verify != nil {
		if err := step.Verify(ctx, st); err != nil {
			return fmt.Errorf("verify: %w", err)
		}
	}

	e.reporter.StepCompleted(step.Name)

	return nil
}

// StepError names the step that stopped a run and wraps its underlying cause, so
// the CLI can print "step X failed: ... — re-run to resume".
type StepError struct {
	Step string
	Err  error
}

func (e *StepError) Error() string {
	return fmt.Sprintf("step %q failed: %v", e.Step, e.Err)
}

func (e *StepError) Unwrap() error {
	return e.Err
}

// nopReporter is the default when a caller passes nil.
type nopReporter struct{}

func (nopReporter) StepStarted(string)         {}
func (nopReporter) StepSkipped(string, string) {}
func (nopReporter) StepCompleted(string)       {}
func (nopReporter) StepFailed(string, error)   {}
func (nopReporter) Info(string)                {}
