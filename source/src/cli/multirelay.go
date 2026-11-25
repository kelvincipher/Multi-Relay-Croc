package cli

import (
	"errors"
	"os"
	"path/filepath"

	"github.com/schollz/croc/v11/internal/cli"
	"github.com/schollz/croc/v11/src/croc"
	"github.com/schollz/croc/v11/src/multirelay"
)

// Select the configured pool before CLI public-relay discovery. New validates
// it again and owns the authenticated configuration used for the transfer.
func configureMultiRelay(c *cli.Context, options *croc.Options) (bool, error) {
	selected := c.String("multirelay-config")
	config, err := multirelay.LoadSelected(selected)
	if err != nil {
		return false, err
	}
	if config == nil {
		return false, nil
	}
	if options.Transport == croc.TransportDERP {
		return false, errors.New("multirelay cannot use --transport derp")
	}
	if selected == "" {
		selected = os.Getenv("CROC_MULTIRELAY_CONFIG")
	}
	if selected == "" {
		selected, err = multirelay.DefaultPath()
		if err != nil {
			return false, err
		}
	}
	selected, err = filepath.Abs(selected)
	if err != nil {
		return false, err
	}
	options.MultiRelayConfig = selected
	options.RelayAddress = config.Pairs[0].Relays[0].ControlAddress()
	options.RelayAddress6 = ""
	options.DisableLocal = true
	options.PublicRelay = false
	return true, nil
}
func rejectMultiRelayStore(c *cli.Context) error {
	config, err := multirelay.LoadSelected(c.String("multirelay-config"))
	if err != nil {
		return err
	}
	if config != nil {
		return errors.New("stored transfers cannot use the configured two-relay protocol")
	}
	return nil
}
