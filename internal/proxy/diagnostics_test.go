package proxy

import (
	"context"
	"testing"
)

type namedDiagnosticPlugin string

func (plugin namedDiagnosticPlugin) Name() string { return string(plugin) }

func (plugin namedDiagnosticPlugin) Dump(context.Context, diagnosticSnapshot) ([]diagnosticSection, error) {
	return nil, nil
}

func TestDiagnosticPluginsPutCoreFirstAndSortOptionalPlugins(t *testing.T) {
	plugins := orderedDiagnosticPlugins(applicationDiagnosticPlugin{}, map[string]diagnosticPlugin{
		"zeta":  namedDiagnosticPlugin("zeta"),
		"alpha": namedDiagnosticPlugin("alpha"),
		"beta":  namedDiagnosticPlugin("beta"),
	})
	if len(plugins) == 0 {
		t.Fatal("diagnostic plugin list is empty")
	}
	if got := plugins[0].Name(); got != "application" {
		t.Fatalf("first diagnostic plugin = %q, want application core", got)
	}
	for index, want := range []string{"application", "alpha", "beta", "zeta"} {
		if got := plugins[index].Name(); got != want {
			t.Fatalf("plugin[%d] = %q, want %q", index, got, want)
		}
	}
	for index := 2; index < len(plugins); index++ {
		if plugins[index-1].Name() > plugins[index].Name() {
			t.Fatalf("optional diagnostic plugins are not sorted: %q before %q", plugins[index-1].Name(), plugins[index].Name())
		}
	}
}
