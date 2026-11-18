package runtime

import (
	"path/filepath"
	"testing"

	"go.temporal.io/sdk/worker"
)

func TestWorkflowReplay(t *testing.T) {
	fixtures := []string{
		"approval-waiting",
		"approval-resumed",
		"approval-versioned",
		"loop-continue",
		"retried-run",
		"preview",
		"scheduled-run",
		"wait",
	}
	for _, name := range fixtures {
		t.Run(name, func(t *testing.T) {
			replayer := worker.NewWorkflowReplayer()
			replayer.RegisterWorkflow(RunWorkflow)
			replayer.RegisterWorkflow(ScheduledWorkflow)
			if err := replayer.ReplayWorkflowHistoryFromJSONFile(nil, filepath.Join("testdata", "history", name+".json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}
