package proxy

import (
	"github.com/grimir/golieipp/internal/stats"
	"github.com/grimir/golieipp/internal/urf"
)

type translationResources struct {
	span   *stats.Span
	output *stats.Resource
}

func (r translationResources) AcquireBuffer(component string, size int64) urf.ResourceLease {
	return r.span.AcquireBuffer(component, size)
}
func (r translationResources) AcquireTemp(_ string, _ int64) urf.ResourceLease {
	return retainedOutput{r.output}
}

// Translate owns its scratch leases, but the caller retains the staged file
// through upstream dispatch. Its temporary resource is released by Close.
type retainedOutput struct{ *stats.Resource }

func (retainedOutput) Release() {}
