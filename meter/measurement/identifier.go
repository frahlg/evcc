package measurement

import (
	"context"
	"fmt"
	"strings"

	"github.com/evcc-io/evcc/plugin"
)

type Identifier struct {
	Identifier *plugin.Config // optional
}

func (cc *Identifier) Configure(ctx context.Context) (func() (string, error), error) {
	if cc.Identifier == nil {
		return nil, nil
	}

	identifierG, err := cc.Identifier.StringGetter(ctx)
	if err != nil {
		return nil, fmt.Errorf("identifier: %w", err)
	}

	// Wrap the identifier getter to trim NULs and whitespace
	return func() (string, error) {
		id, err := identifierG()
		if err != nil {
			return "", err
		}
		
		// Trim NUL bytes and whitespace
		id = strings.TrimSpace(id)
		id = strings.Trim(id, "\x00")
		
		return id, nil
	}, nil
}