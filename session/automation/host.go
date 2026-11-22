package automation

// Host-field JSON round-tripping for Task and Run.
//
// The core owns its own fields but a host often has extra per-task state (a
// model, workspace, permission, conversation binding, display summary, ...).
// Host is an opaque map: the core never reads it, and its entries are written as
// top-level JSON keys, so a host's existing registry document stays readable and
// keeps its fields across a core rewrite.

import "encoding/json"

// taskCoreKeys are the registry keys the core itself writes.
var taskCoreKeys = map[string]struct{}{
	"id": {}, "name": {}, "prompt": {}, "schedule": {}, "enabled": {},
	"createdAt": {}, "updatedAt": {}, "lastRun": {}, "nextRun": {},
	"lastError": {}, "runHistory": {},
}

// runCoreKeys are the registry keys a run record owns itself.
var runCoreKeys = map[string]struct{}{
	"id": {}, "startedAt": {}, "deliveredAt": {}, "completedAt": {},
	"status": {}, "error": {}, "deliveryId": {}, "durationMs": {},
}

// taskJSON is Task without its methods (avoids JSON recursion).
type taskJSON Task

// MarshalJSON writes the core fields plus the host fields as top-level keys.
// Host keys that collide with a core key are ignored (the core value wins).
func (t Task) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(taskJSON(t))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	for key, value := range t.Host {
		if _, reserved := taskCoreKeys[key]; reserved {
			continue
		}
		fields[key] = value
	}
	return json.Marshal(fields)
}

// UnmarshalJSON reads the core fields and captures every unknown top-level key
// into Host, so a host's extra fields survive a load/save round trip.
func (t *Task) UnmarshalJSON(data []byte) error {
	var alias taskJSON
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	host := make(map[string]json.RawMessage)
	for key, value := range fields {
		if _, known := taskCoreKeys[key]; known {
			continue
		}
		host[key] = value
	}
	*t = Task(alias)
	if len(host) > 0 {
		t.Host = host
	} else {
		t.Host = nil
	}
	return nil
}

// runJSON is Run without its methods (avoids JSON recursion).
type runJSON Run

// MarshalJSON writes the core run fields plus the host fields.
func (r Run) MarshalJSON() ([]byte, error) {
	base, err := json.Marshal(runJSON(r))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(base, &fields); err != nil {
		return nil, err
	}
	for key, value := range r.Host {
		if _, reserved := runCoreKeys[key]; reserved {
			continue
		}
		fields[key] = value
	}
	return json.Marshal(fields)
}

// UnmarshalJSON reads the core run fields and captures unknown keys into Host
// (for example a host's summary/sessionId or a legacy finishedAt timestamp).
func (r *Run) UnmarshalJSON(data []byte) error {
	var alias runJSON
	if err := json.Unmarshal(data, &alias); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	host := make(map[string]json.RawMessage)
	for key, value := range fields {
		if _, known := runCoreKeys[key]; known {
			continue
		}
		host[key] = value
	}
	*r = Run(alias)
	if len(host) > 0 {
		r.Host = host
	} else {
		r.Host = nil
	}
	return nil
}

// copyHost returns an independent copy of a host-field map.
func copyHost(host map[string]json.RawMessage) map[string]json.RawMessage {
	if host == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(host))
	for key, value := range host {
		out[key] = append(json.RawMessage(nil), value...)
	}
	return out
}
