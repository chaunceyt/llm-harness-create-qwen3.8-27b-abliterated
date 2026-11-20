package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"harness/internal/sandbox"
)

// Step is one workflow node. Exactly one of Command or Task is set: Command
// executes sandboxed directly; Task runs a nested agent task. Later steps may
// reference earlier outputs as {stepID}.
type Step struct {
	ID      string   `json:"id"`
	Needs   []string `json:"needs,omitempty"`
	Command string   `json:"command,omitempty"`
	Task    string   `json:"task,omitempty"`
}

// Workflow is a DAG of steps.
type Workflow struct {
	Name  string `json:"name,omitempty"`
	Steps []Step `json:"steps"`
}

// StepReport is one executed step's outcome.
type StepReport struct {
	ID     string
	Status string // ok|failed
	Output string
	Err    string
}

// WorkflowReport summarizes one RunWorkflow invocation. Failed names the
// aborting step; empty when every step succeeded.
type WorkflowReport struct {
	Name   string
	Steps  []StepReport
	Failed string
}

// Validate checks structural soundness: unique IDs, exactly one executor per
// step, known dependencies, no cycles.
func (wf *Workflow) Validate() error {
	ids := make(map[string]bool, len(wf.Steps))
	for _, s := range wf.Steps {
		if s.ID == "" {
			return fmt.Errorf("workflow: step with empty id")
		}
		if ids[s.ID] {
			return fmt.Errorf("workflow: duplicate step %q", s.ID)
		}
		ids[s.ID] = true
		if (s.Command == "") == (s.Task == "") {
			return fmt.Errorf("workflow: step %q must set exactly one of Command or Task", s.ID)
		}
	}
	// Dependency checks run after every ID is registered so backward
	// references are not misreported as unknown steps.
	for _, s := range wf.Steps {
		for _, n := range s.Needs {
			if !ids[n] {
				return fmt.Errorf("workflow: step %q depends on unknown step %q", s.ID, n)
			}
		}
	}
	if _, err := topoOrder(wf.Steps); err != nil {
		return err
	}
	return nil
}

// topoOrder is Kahn's algorithm, declaration-order stable among ready nodes.
func topoOrder(steps []Step) ([]string, error) {
	indeg := make(map[string]int, len(steps))
	dependents := make(map[string][]string, len(steps))
	for _, s := range steps {
		indeg[s.ID] = len(s.Needs)
	}
	for _, s := range steps {
		for _, n := range s.Needs {
			dependents[n] = append(dependents[n], s.ID)
		}
	}
	var ready, order []string
	for _, s := range steps {
		if indeg[s.ID] == 0 {
			ready = append(ready, s.ID)
		}
	}
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		for _, nxt := range dependents[id] {
			indeg[nxt]--
			if indeg[nxt] == 0 {
				ready = append(ready, nxt)
			}
		}
	}
	if len(order) != len(steps) {
		return nil, fmt.Errorf("workflow: dependency cycle")
	}
	return order, nil
}

// RunWorkflow executes the DAG in dependency order. Command steps run
// sandboxed; Task steps run nested agent tasks. A failing step aborts the
// remainder and is reported (not returned as a Go error); structural faults
// are errors.
func (a *Agent) RunWorkflow(ctx context.Context, wf Workflow) (WorkflowReport, error) {
	ag := *a
	return ag.runWorkflow(ctx, wf)
}

func (a *Agent) runWorkflow(ctx context.Context, wf Workflow) (WorkflowReport, error) {
	if err := a.normalize(); err != nil {
		return WorkflowReport{}, err
	}
	if err := wf.Validate(); err != nil {
		return WorkflowReport{}, err
	}
	order, _ := topoOrder(wf.Steps)
	byID := make(map[string]Step, len(wf.Steps))
	for _, s := range wf.Steps {
		byID[s.ID] = s
	}
	writable := a.Mode != ModeReviewer

	report := WorkflowReport{Name: wf.Name}
	outputs := make(map[string]string, len(wf.Steps))
	abort := func(id, errMsg, partialOutput string) WorkflowReport {
		report.Steps = append(report.Steps, StepReport{
			ID: id, Status: "failed", Output: partialOutput, Err: errMsg,
		})
		report.Failed = id
		return report
	}

	for _, id := range order {
		if err := ctx.Err(); err != nil {
			report.Failed = id
			return report, fmt.Errorf("workflow: canceled at %q: %w", id, err)
		}
		st := byID[id]
		switch {
		case st.Command != "":
			cmd := substitute(st.Command, outputs)
			cfg := sandbox.Config{
				Workspace:  a.Env.Workspace,
				AllowWrite: writable && a.Env.AllowWrite,
				AllowNet:   a.Env.AllowNet,
			}
			res, err := a.Env.Sandbox.Run(ctx, cfg, []string{"/bin/sh", "-c", cmd})
			if err != nil {
				return abort(id, fmt.Sprintf("sandbox: %v", err), ""), nil
			}
			if res.ExitCode != 0 {
				return abort(id, "", fmt.Sprintf("[exit %d]\n%s%s", res.ExitCode, res.Stdout, res.Stderr)), nil
			}
			outputs[id] = strings.TrimSuffix(string(res.Stdout), "\n")
			report.Steps = append(report.Steps, StepReport{ID: id, Status: "ok", Output: outputs[id]})
		default:
			task := substitute(st.Task, outputs)
			rep, err := a.runTask(ctx, task)
			if err != nil {
				return abort(id, err.Error(), ""), nil
			}
			outputs[id] = rep.Answer
			report.Steps = append(report.Steps, StepReport{ID: id, Status: "ok", Output: rep.Answer})
		}
	}
	return report, nil
}

// runTask executes one nested agent task with a fresh session. Resume state
// never propagates into nested tasks: each starts its own clean session.
func (a *Agent) runTask(ctx context.Context, task string) (Report, error) {
	sub := *a
	sub.Seed = nil
	sub.ResumeSession = ""
	return sub.Run(ctx, task)
}

// substitute replaces {stepID} tokens with completed step outputs.
func substitute(s string, outputs map[string]string) string {
	keys := make([]string, 0, len(outputs))
	for k := range outputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s = strings.ReplaceAll(s, "{"+k+"}", outputs[k])
	}
	return s
}
