package approvals

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// Spec is the parsed, normalized approval-flow definition stored in
// approval_defs.spec_json. Keep the shape flat and self-describing —
// this JSON is what admins POST via /api/approvals/definitions.
type Spec struct {
	Start  string           `json:"start"`  // state key to enter on Start()
	States map[string]State `json:"states"` // key -> node
}

// State is one node in the machine.
//
// Kind indexes into the handler registry ("system", "approve", "end",
// plus any plugin-registered kinds).
//
// On maps event names to next-state keys. Well-known events:
//
//	success  — system-kind handler default success
//	approve  — human resolved with "approve" choice
//	reject   — human resolved with "reject" choice
//	timeout  — deadline elapsed; sweeper fired trigger="timeout"
//
// Custom triggers land here unchanged.
//
// TimeoutSec sets the run's deadline_at when entering this state. Zero
// means no deadline for this step.
//
// With is handler-specific config, opaque to the runner.
type State struct {
	Kind       string            `json:"kind"`
	Assignee   string            `json:"assignee,omitempty"`
	Prompt     string            `json:"prompt,omitempty"`
	Choices    []string          `json:"choices,omitempty"`
	TimeoutSec int64             `json:"timeout_sec,omitempty"`
	On         map[string]string `json:"on,omitempty"`
	With       map[string]any    `json:"with,omitempty"`
}

// stateKeyPattern constrains state keys to identifier-ish tokens. Keeps
// the JSON pickable in logs + URL-safe if a future endpoint exposes
// them. Length capped so pathological specs can't blow the audit log.
var stateKeyPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]{0,63}$`)

// assigneePattern accepts "user:<int>" or "role:<slug>" — nothing else.
// Delegation chains are a v2 concern; the skeleton stores an opaque
// string but still validates the format so bad data can't reach SQL.
var assigneePattern = regexp.MustCompile(`^(document_owner|user:[1-9][0-9]{0,18}|role:[a-z][a-z0-9_\-]{0,31})$`)

// Validate returns nil when the spec is internally consistent: start
// exists, every On target exists, every referenced Kind is a
// registered handler, approve-kind states have an assignee + choices.
//
// The handler-kind check needs a registry; a runtime call site (Engine)
// re-runs Validate with its own registry before persisting. This method
// alone is a pure structural check.
func (s Spec) Validate() error {
	if s.Start == "" {
		return fmt.Errorf("spec.validate: start is required")
	}
	if len(s.States) == 0 {
		return fmt.Errorf("spec.validate: at least one state is required")
	}
	if _, ok := s.States[s.Start]; !ok {
		return fmt.Errorf("spec.validate: start state %q not found in states", s.Start)
	}
	for key, st := range s.States {
		if !stateKeyPattern.MatchString(key) {
			return fmt.Errorf("spec.validate: state key %q must match [a-zA-Z][a-zA-Z0-9_]{0,63}", key)
		}
		if st.Kind == "" {
			return fmt.Errorf("spec.validate: state %q has empty kind", key)
		}
		if st.TimeoutSec < 0 {
			return fmt.Errorf("spec.validate: state %q has negative timeout_sec", key)
		}
		if st.Assignee != "" && !assigneePattern.MatchString(st.Assignee) {
			return fmt.Errorf("spec.validate: state %q has bad assignee %q; want document_owner, user:N, or role:slug",
				key, st.Assignee)
		}
		// approve-kind gates: needs assignee + at least one choice, and
		// every choice must have a mapped transition in On.
		if st.Kind == "approve" {
			if st.Assignee == "" {
				return fmt.Errorf("spec.validate: state %q kind=approve requires assignee", key)
			}
			if len(st.Choices) == 0 {
				return fmt.Errorf("spec.validate: state %q kind=approve requires at least one choice", key)
			}
			for _, c := range st.Choices {
				if _, ok := st.On[c]; !ok {
					return fmt.Errorf("spec.validate: state %q choice %q has no transition in on{}",
						key, c)
				}
			}
		}
		// end-kind: terminal. Must not declare transitions.
		if st.Kind == "end" && len(st.On) > 0 {
			return fmt.Errorf("spec.validate: state %q kind=end must not have transitions", key)
		}
		// Every On target must reference a real state.
		for event, next := range st.On {
			if event == "" {
				return fmt.Errorf("spec.validate: state %q has empty event name in on{}", key)
			}
			if _, ok := s.States[next]; !ok {
				return fmt.Errorf("spec.validate: state %q on[%q] -> unknown state %q",
					key, event, next)
			}
		}
	}
	return nil
}

// EncodeSpec marshals a Spec to canonical JSON for storage. Kept as a
// helper so the store package doesn't need to know about Spec's shape.
func EncodeSpec(s Spec) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeSpec parses spec_json back into a Spec.
func DecodeSpec(raw string) (Spec, error) {
	var s Spec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return Spec{}, err
	}
	return s, nil
}
