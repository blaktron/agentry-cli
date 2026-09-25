package policy

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

// Concurrent first runs on one fresh runner home (a CLI run and a desktop
// run started together) each get the parsed default and one digest: no
// caller reads the file half-written (agentry-cli#299).
func TestEnsureDefaultConcurrentFirstRuns(t *testing.T) {
	for round := range 100 {
		path := filepath.Join(t.TempDir(), "policy.yaml")
		const callers = 8
		var wg sync.WaitGroup
		pols := make([]*Policy, callers)
		shas := make([]string, callers)
		errs := make([]error, callers)
		start := make(chan struct{})
		for i := range callers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				pols[i], shas[i], errs[i] = EnsureDefault(path)
			}()
		}
		close(start)
		wg.Wait()
		for i := range callers {
			if errs[i] != nil {
				t.Fatalf("round %d caller %d: %v", round, i, errs[i])
			}
			if !reflect.DeepEqual(pols[i], Default()) {
				t.Fatalf("round %d caller %d: policy is not the default", round, i)
			}
			if shas[i] != shas[0] {
				t.Fatalf("round %d: digests differ: %s vs %s", round, shas[i], shas[0])
			}
		}
	}
}
