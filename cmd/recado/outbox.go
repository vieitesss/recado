package main

import (
	"encoding/json"
	"time"

	"github.com/spf13/cobra"
	"github.com/vieitesss/recado/internal/addr"
)

func (a *app) newOutboxCmd() *cobra.Command {
	var host string
	var after int64
	var follow bool
	cmd := &cobra.Command{
		Use: "outbox", Short: "Read upstream messages for a host", Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			if _, err := addr.Parse("recado+upstream://" + host + "/agent"); err != nil {
				return usagef("invalid --host %q", host)
			}
			st, err := a.store()
			if err != nil {
				return err
			}
			for {
				records, end, err := st.ReadOutbox(after, host)
				if err != nil {
					return err
				}
				enc := json.NewEncoder(cmd.OutOrStdout())
				for _, record := range records {
					if jsonFlag(cmd) {
						err = enc.Encode(record)
					} else {
						err = enc.Encode(record.Envelope)
					}
					if err != nil {
						return err
					}
				}
				after = end
				if !follow {
					return nil
				}
				if jsonFlag(cmd) {
					if err := enc.Encode(struct {
						Offset   int64 `json:"offset"`
						CaughtUp bool  `json:"caught_up"`
					}{after, true}); err != nil {
						return err
					}
				}
				timer := time.NewTimer(200 * time.Millisecond)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return nil
				case <-timer.C:
				}
			}
		}),
	}
	cmd.Flags().StringVar(&host, "host", "", "upstream host to read")
	cmd.Flags().Int64Var(&after, "after", 0, "start after this byte offset")
	cmd.Flags().BoolVar(&follow, "follow", false, "stream new outbox lines")
	return cmd
}
