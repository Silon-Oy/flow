package egresship

import (
	"context"

	"github.com/Silon-Oy/flow/internal/centralclient"
)

// CentralSink adapts a centralclient.Client to the Sink interface — the runner
// composes the two in cmd/flow-runner/main.go.
type CentralSink struct {
	Client *centralclient.Client
}

// ShipEgress converts the parsed entries to the wire shape and posts them.
func (s CentralSink) ShipEgress(ctx context.Context, entries []Entry) error {
	if s.Client == nil || len(entries) == 0 {
		return nil
	}
	out := make([]centralclient.EgressEntry, len(entries))
	for i, e := range entries {
		out[i] = centralclient.EgressEntry{
			Host:    e.Host,
			Allowed: e.Allowed,
			TS:      e.TS,
		}
	}
	return s.Client.ShipEgress(ctx, out)
}
