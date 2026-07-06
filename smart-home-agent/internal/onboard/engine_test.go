package onboard

import (
	"context"
	"errors"
	"testing"
)

// captureReporter records the engine's progress calls so tests can assert on the
// exact sequence (started/skipped/completed/failed).
type captureReporter struct {
	started   []string
	skipped   []string
	completed []string
	failed    []string
}

func (c *captureReporter) StepStarted(name string)         { c.started = append(c.started, name) }
func (c *captureReporter) StepSkipped(name, _ string)      { c.skipped = append(c.skipped, name) }
func (c *captureReporter) StepCompleted(name string)       { c.completed = append(c.completed, name) }
func (c *captureReporter) StepFailed(name string, _ error) { c.failed = append(c.failed, name) }
func (c *captureReporter) Info(string)                     {}

func TestEngineRunsStepsInOrder(t *testing.T) {
	var order []string
	step := func(name string) Step {
		return Step{
			Name: name,
			Act: func(context.Context, *State) error {
				order = append(order, name)

				return nil
			},
		}
	}

	rep := &captureReporter{}
	eng := NewEngine([]Step{step("a"), step("b"), step("c")}, rep)

	if err := eng.Run(context.Background(), &State{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := order; len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Errorf("steps ran out of order: %v", got)
	}
	if len(rep.completed) != 3 {
		t.Errorf("expected 3 completed, got %v", rep.completed)
	}
}

// A step whose Check reports the goal already holds must skip Act and Verify —
// this is the mechanism the whole resumable design rests on.
func TestCheckShortCircuitsActAndVerify(t *testing.T) {
	actRan, verifyRan := false, false

	rep := &captureReporter{}
	eng := NewEngine([]Step{{
		Name:  "already-done",
		Check: func(context.Context, *State) (bool, error) { return true, nil },
		Act: func(context.Context, *State) error {
			actRan = true

			return nil
		},
		Verify: func(context.Context, *State) error {
			verifyRan = true

			return nil
		},
	}}, rep)

	if err := eng.Run(context.Background(), &State{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if actRan || verifyRan {
		t.Errorf("Check=true must skip Act (%v) and Verify (%v)", actRan, verifyRan)
	}
	if len(rep.skipped) != 1 || rep.skipped[0] != "already-done" {
		t.Errorf("expected the step reported as skipped, got %v", rep.skipped)
	}
}

// A failing Verify stops the run at that step, wraps the cause in *StepError, and
// never advances to later steps.
func TestVerifyFailureStopsRun(t *testing.T) {
	sentinel := errors.New("boom")
	laterRan := false

	rep := &captureReporter{}
	eng := NewEngine([]Step{
		{
			Name:   "bad",
			Act:    func(context.Context, *State) error { return nil },
			Verify: func(context.Context, *State) error { return sentinel },
		},
		{
			Name: "later",
			Act: func(context.Context, *State) error {
				laterRan = true

				return nil
			},
		},
	}, rep)

	err := eng.Run(context.Background(), &State{})
	if err == nil {
		t.Fatal("expected an error")
	}

	var stepErr *StepError
	if !errors.As(err, &stepErr) || stepErr.Step != "bad" {
		t.Fatalf("expected *StepError for step \"bad\", got %v", err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("StepError should wrap the underlying cause")
	}
	if laterRan {
		t.Error("a later step must not run after a failure")
	}
	if len(rep.failed) != 1 || rep.failed[0] != "bad" {
		t.Errorf("expected the failed step reported once, got %v", rep.failed)
	}
}

// A failing Act likewise stops the run and names the step.
func TestActFailureStopsRun(t *testing.T) {
	eng := NewEngine([]Step{{
		Name: "acts-badly",
		Act:  func(context.Context, *State) error { return errors.New("nope") },
	}}, nil)

	err := eng.Run(context.Background(), &State{})

	var stepErr *StepError
	if !errors.As(err, &stepErr) || stepErr.Step != "acts-badly" {
		t.Fatalf("expected *StepError for \"acts-badly\", got %v", err)
	}
}

// The headline guarantee: a run that dies partway is recovered by re-running.
// The first run fails in step "b"; on the second run, "a" and "b" report done
// via Check (so their Act does not run again) and the run advances to "c".
func TestResumeFromPartialRun(t *testing.T) {
	// device models the gateway state that Check queries fresh each run.
	device := struct {
		aDone bool
		bDone bool
	}{}

	var aActs, bActs, cActs int
	bShouldFail := true

	steps := []Step{
		{
			Name:  "a",
			Check: func(context.Context, *State) (bool, error) { return device.aDone, nil },
			Act: func(context.Context, *State) error {
				aActs++
				device.aDone = true

				return nil
			},
		},
		{
			Name:  "b",
			Check: func(context.Context, *State) (bool, error) { return device.bDone, nil },
			Act: func(context.Context, *State) error {
				bActs++
				if bShouldFail {
					return errors.New("transient failure")
				}
				device.bDone = true

				return nil
			},
		},
		{
			Name:  "c",
			Check: func(context.Context, *State) (bool, error) { return false, nil },
			Act: func(context.Context, *State) error {
				cActs++

				return nil
			},
		},
	}

	// First run: a succeeds, b fails, c never runs.
	eng := NewEngine(steps, nil)
	if err := eng.Run(context.Background(), &State{}); err == nil {
		t.Fatal("expected first run to fail at b")
	}
	if aActs != 1 || bActs != 1 || cActs != 0 {
		t.Fatalf("after first run: aActs=%d bActs=%d cActs=%d", aActs, bActs, cActs)
	}

	// Operator "fixes" whatever made b fail, then re-runs the same command.
	bShouldFail = false
	eng = NewEngine(steps, nil)
	if err := eng.Run(context.Background(), &State{}); err != nil {
		t.Fatalf("resume run: %v", err)
	}

	if aActs != 1 {
		t.Errorf("step a acted again on resume (aActs=%d); Check should have skipped it", aActs)
	}
	if bActs != 2 {
		t.Errorf("step b should have acted once more on resume (bActs=%d)", bActs)
	}
	if cActs != 1 {
		t.Errorf("step c should have run on resume (cActs=%d)", cActs)
	}
}

// A Check error aborts the run at that step (not treated as "not done").
func TestCheckErrorStopsRun(t *testing.T) {
	acted := false
	eng := NewEngine([]Step{{
		Name:  "checks-badly",
		Check: func(context.Context, *State) (bool, error) { return false, errors.New("cannot reach device") },
		Act: func(context.Context, *State) error {
			acted = true

			return nil
		},
	}}, nil)

	if err := eng.Run(context.Background(), &State{}); err == nil {
		t.Fatal("expected a check error to stop the run")
	}
	if acted {
		t.Error("Act must not run when Check errors")
	}
}

// A cancelled context aborts before running any step.
func TestContextCancellationAborts(t *testing.T) {
	ran := false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	eng := NewEngine([]Step{{
		Name: "should-not-run",
		Act: func(context.Context, *State) error {
			ran = true

			return nil
		},
	}}, nil)

	if err := eng.Run(ctx, &State{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if ran {
		t.Error("no step should run under a cancelled context")
	}
}
