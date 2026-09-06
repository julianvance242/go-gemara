package gemara

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"time"

	"github.com/gemaraproj/go-gemara/internal/codec"
)

// AssessmentStep is a function type that inspects the provided targetData and returns a Result with a message and confidence level.
// The message may be an error string or other descriptive text.
type AssessmentStep func(payload interface{}) (Result, string, ConfidenceLevel)

// stepNameProbe is the sentinel payload that asks a decoded step for its name.
type stepNameProbe struct{}

// decodedStep is the step a log decodes into. AssessmentStep is a function type,
// so the recorded name has nowhere to live but inside the function itself: the
// closure captures it and returns it when probed.
//
// A function is not recoverable from its name, so a decoded step reports Unknown
// rather than pretending to assess anything.
func decodedStep(name string) AssessmentStep {
	return func(payload interface{}) (Result, string, ConfidenceLevel) {
		if _, ok := payload.(stepNameProbe); ok {
			return Unknown, name, Undetermined
		}
		return Unknown, decodedStepMessage, Undetermined
	}
}

const decodedStepMessage = "assessment step was decoded from a log, which records step names only, and cannot be re-run"

// decodedStepPC is the code pointer every closure returned by decodedStep shares,
// which is how String tells a decoded step from one a consumer wrote. A toolchain
// that stopped sharing it would break that identification; the round-trip test
// is what catches it.
var decodedStepPC = reflect.ValueOf(decodedStep("")).Pointer()

// isDecoded reports whether the step came from a log rather than from a consumer.
func (as AssessmentStep) isDecoded() bool {
	return as != nil && reflect.ValueOf(as).Pointer() == decodedStepPC
}

// EvidenceCollector is an embeddable helper that gives a targetData payload the
// well-known evidence location and satisfies HasEvidence via method promotion,
// so the consumer writes no methods. Embed it in a payload struct:
//
//	type myTarget struct {
//		gemara.EvidenceCollector
//		Config SomeConfig
//	}
//
//	t := &myTarget{Config: cfg}   // pointer, so step mutations propagate
//	t.AddEvidence(ev)             // inside a step; may be called more than once
//	assessment.Run(t)             // copies evidence into assessment.Evidence,
//	                              // clearing the payload after each step
type EvidenceCollector struct {
	// Long name to avoid any possibility of collision
	gemaraStepEvidence []Evidence
}

// GetEvidence returns the evidence recorded since the last clear.
func (e *EvidenceCollector) GetEvidence() []Evidence {
	return e.gemaraStepEvidence
}

// AddEvidence appends a piece of evidence, so a single step may record evidence
// more than once. The accumulated evidence is copied into the log and cleared
// after each step runs.
func (e *EvidenceCollector) AddEvidence(evidence Evidence) {
	e.gemaraStepEvidence = append(e.gemaraStepEvidence, evidence)
}

// ClearEvidence empties the evidence location. The assessment calls it after
// copying each step's evidence into the log, so evidence does not linger
// in memory or get re-copied by a later step that records nothing.
func (e *EvidenceCollector) ClearEvidence() {
	e.gemaraStepEvidence = nil
}

// HasEvidence is the well-known interface a targetData payload may implement to
// surface Evidence collected while an assessment runs. A step records evidence
// into the payload (typically by mutating a shared field, which requires the
// payload to be passed by reference), and the AssessmentLog harvests it from
// this single location after running.
//
// Implementing the interface is optional: a payload that does not implement it
// simply contributes no evidence, which is not an error.
type HasEvidence interface {
	// GetEvidence returns the evidence recorded since the last clear.
	GetEvidence() []Evidence

	// AddEvidence appends a piece of evidence; it may be called more than once
	// within a single step.
	AddEvidence(evidence Evidence)

	// ClearEvidence empties the evidence location. The assessment calls it after
	// copying each step's evidence into the log, so a later step that records
	// nothing does not cause this step's evidence to be re-copied.
	ClearEvidence()
}

func (as AssessmentStep) String() string {
	// The recorded name lives inside the closure, not in its symbol.
	if as.isDecoded() {
		_, name, _ := as(stepNameProbe{})
		return name
	}
	// Get the function pointer correctly
	fn := runtime.FuncForPC(reflect.ValueOf(as).Pointer())
	if fn == nil {
		return "<unknown function>"
	}
	return fn.Name()
}

// UnmarshalJSON reads the step name the log recorded. The spec declares the wire
// type as a string (evaluationlog.cue: `#AssessmentStep: string`).
func (as *AssessmentStep) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	*as = decodedStep(name)
	return nil
}

// UnmarshalYAML is the YAML half of UnmarshalJSON, using the goccy/go-yaml
// BytesUnmarshaler signature the enums in this package already use.
func (as *AssessmentStep) UnmarshalYAML(data []byte) error {
	var name string
	if err := codec.UnmarshalYAML(data, &name); err != nil {
		return err
	}
	*as = decodedStep(name)
	return nil
}

// UnmarshalText lets yaml.v3-family decoders, which honor
// encoding.TextUnmarshaler for scalars rather than the signature above,
// deserialize a step name.
func (as *AssessmentStep) UnmarshalText(data []byte) error {
	*as = decodedStep(string(data))
	return nil
}

func (as AssessmentStep) MarshalJSON() ([]byte, error) {
	return json.Marshal(as.String())
}

func (as AssessmentStep) MarshalYAML() (interface{}, error) {
	return as.String(), nil
}

// NewAssessment creates a new AssessmentLog object and returns a pointer to it.
func NewAssessment(requirementId string, description string, applicability []string, steps []AssessmentStep) (*AssessmentLog, error) {
	a := &AssessmentLog{
		Requirement: EntryMapping{
			EntryId: requirementId,
		},
		Description:   description,
		Applicability: applicability,
		Result:        NotRun,
		Steps:         steps,
	}
	err := a.precheck()
	return a, err
}

// AddStep queues a new step in the AssessmentLog
func (a *AssessmentLog) AddStep(step AssessmentStep) {
	a.Steps = append(a.Steps, step)
}

func (a *AssessmentLog) runStep(targetData interface{}, step AssessmentStep) Result {
	a.StepsExecuted++
	result, message, confidence := step(targetData)
	a.Result = UpdateAggregateResult(a.Result, result)

	// Move any evidence the step recorded from the payload into the log: copy it
	// out, then clear the payload. The clear is required so a later step that
	// records nothing does not leave this step's evidence in place for the next
	// harvest to re-copy as a duplicate.
	if provider, ok := targetData.(HasEvidence); ok {
		a.Evidence = append(a.Evidence, provider.GetEvidence()...)
		provider.ClearEvidence()
	}

	// Always update message to show what steps have been run and their context.
	a.Message = message

	// Always use the confidence level from the last step executed.
	// This gives step implementers full control over how confidence builds
	// as steps are executed, allowing them to adapt confidence based on
	// the cumulative context of all previous steps.
	a.ConfidenceLevel = confidence

	return result
}

// Run executes the steps in order, halting early on the first step that returns
// Failed or NotApplicable. Every other result aggregates into a.Result via
// UpdateAggregateResult and execution continues.
func (a *AssessmentLog) Run(targetData interface{}) Result {
	a.Result = NotRun

	a.Start = Datetime(time.Now().Format(time.RFC3339))
	err := a.precheck()
	if err != nil {
		a.Result = Unknown
		a.ConfidenceLevel = Undetermined
		return a.Result
	}

	// Stamp the end time on every path that ran at least one step, including the
	// early returns below.
	defer func() {
		a.End = Datetime(time.Now().Format(time.RFC3339))
	}()

	for _, step := range a.Steps {
		switch a.runStep(targetData, step) {
		case Failed:
			// runStep has already aggregated Failed into a.Result, and nothing
			// outranks it.
			return a.Result
		case NotApplicable:
			// Do not continue if a step determines that the assessment is not applicable
			// UpdateAggregateResult treats NotApplicable as the
			// weakest non-NotRun value, so any prior result would absorb it. Correct for rollup for control
			// evaluation, but in a step-level scope guard it must win unconditionally.
			a.Result = NotApplicable
			return a.Result
		}
	}
	return a.Result
}

// precheck verifies that the assessment has all the required fields.
// It returns an error if the assessment is not valid.
func (a *AssessmentLog) precheck() error {
	if a.Requirement.EntryId == "" || a.Description == "" || a.Applicability == nil || a.Steps == nil || len(a.Applicability) == 0 || len(a.Steps) == 0 {
		message := fmt.Sprintf(
			"expected all AssessmentLog fields to have a value, but got: requirementId=len(%v), description=len=(%v), applicability=len(%v), steps=len(%v)",
			len(a.Requirement.EntryId), len(a.Description), len(a.Applicability), len(a.Steps),
		)
		a.Result = Unknown
		a.Message = message
		a.ConfidenceLevel = Undetermined
		return errors.New(message)
	}

	// A decoded log has step names but no functions to run.
	for _, step := range a.Steps {
		if step.isDecoded() {
			message := fmt.Sprintf("%s: %q", decodedStepMessage, step)
			a.Result = Unknown
			a.Message = message
			a.ConfidenceLevel = Undetermined
			return errors.New(message)
		}
	}

	return nil
}
