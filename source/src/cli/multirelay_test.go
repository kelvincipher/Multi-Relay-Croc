package cli

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	framework "github.com/schollz/croc/v11/internal/cli"
	"github.com/schollz/croc/v11/src/croc"
)

func TestMultiRelayCLISelection(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", directory)
	t.Setenv("CROC_MULTIRELAY_CONFIG", "")
	data, err := os.ReadFile("../../examples/multirelay/relays.yaml")
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, "relays.yaml")
	if err = os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	set.String("multirelay-config", name, "")
	ctx := framework.NewContext(newApp(), set, nil)
	options := croc.Options{Transport: croc.TransportAuto}
	enabled, err := configureMultiRelay(ctx, &options)
	if err != nil || !enabled {
		t.Fatal(enabled, err)
	}
	if options.RelayAddress != "127.0.0.1:9009" || !options.DisableLocal || options.MultiRelayConfig != name || options.PublicRelay {
		t.Fatal("CLI did not select private pool", options)
	}
	options.Transport = croc.TransportDERP
	if _, err = configureMultiRelay(ctx, &options); err == nil {
		t.Fatal("strict derp silently downgraded")
	}
	if err = rejectMultiRelayStore(ctx); err == nil {
		t.Fatal("stored upload bypassed pool")
	}
}
