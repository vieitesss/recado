package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/vieitesss/recado/internal/addr"
	"github.com/vieitesss/recado/internal/envelope"
)

func (a *app) newSendCmd() *cobra.Command {
	meta := metaFlag{}
	kind := envelope.KindMessage
	cmd := &cobra.Command{
		Use:   "send TO TEXT",
		Short: "Send a message to another agent",
		Long: `Send a message to another agent and return: recado owns delivery from then on.

TEXT may be "-" to read the body from stdin. A bare TO means this machine;
recado://<machine>/<id> is accepted too, but machines are not supported yet.`,
		Args: cobra.ExactArgs(2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			body, err := readBody(cmd, args[1])
			if err != nil {
				return err
			}
			return a.send(cmd, args[0], kind, body, meta)
		}),
	}
	cmd.Flags().StringVar(&kind, "kind", envelope.KindMessage, "message kind: message, question, answer, complete, followup, ...")
	cmd.Flags().Var(meta, "meta", "metadata pair k=v (repeatable)")
	return cmd
}

func (a *app) newAskCmd() *cobra.Command {
	meta := metaFlag{}
	cmd := &cobra.Command{
		Use:   "ask TO QUESTION",
		Short: "Ask another agent a question",
		Long: `Ask another agent a question as kind "question" and return.

It does not block: end your turn and the answer arrives as your next turn.`,
		Args: cobra.ExactArgs(2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			body, err := readBody(cmd, args[1])
			if err != nil {
				return err
			}
			return a.send(cmd, args[0], envelope.KindQuestion, body, meta)
		}),
	}
	cmd.Flags().Var(meta, "meta", "metadata pair k=v (repeatable)")
	return cmd
}

func (a *app) newReplyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reply ID TEXT",
		Short: "Answer a message you received",
		Long: `Answer message ID as kind "answer".

The answer goes to that message's sender, carries its id in reply_to, and
copies its meta. Always name the message: never "the last question".`,
		Args: cobra.ExactArgs(2),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			body, err := readBody(cmd, args[1])
			if err != nil {
				return err
			}
			from, err := a.whoami()
			if err != nil {
				return err
			}
			svc, err := a.service()
			if err != nil {
				return err
			}
			res, err := svc.Reply(from, args[0], body)
			if err != nil {
				return err
			}
			return printSent(cmd, view(res))
		}),
	}
}

func (a *app) send(cmd *cobra.Command, toArg, kind, body string, meta map[string]string) error {
	from, err := a.whoami()
	if err != nil {
		return err
	}
	to, err := addr.Parse(toArg)
	if err != nil {
		return usage(err)
	}
	svc, err := a.service()
	if err != nil {
		return err
	}
	env, err := envelope.New(from, to, kind, body, meta)
	if err != nil {
		return usage(err)
	}
	res, err := svc.Deliver(env)
	if err != nil {
		return err
	}
	return printSent(cmd, view(res))
}

func (a *app) newDeliverCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "deliver",
		Short:  "Deliver envelopes read from stdin",
		Hidden: true,
		Long: `Read finished envelopes (one JSON object per line) on stdin and deliver each.

This is the one entry point for remote sends, the relay and other software.`,
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			svc, err := a.service()
			if err != nil {
				return err
			}
			in := bufio.NewReader(cmd.InOrStdin())
			delivered := []sent{}
			for {
				line, rerr := in.ReadBytes('\n')
				if len(bytes.TrimSpace(line)) > 0 {
					env, err := envelope.Decode(line)
					if err != nil {
						return usage(err)
					}
					res, err := svc.Deliver(env)
					if err != nil {
						return err
					}
					delivered = append(delivered, view(res))
				}
				if rerr != nil {
					if errors.Is(rerr, io.EOF) {
						break
					}
					return rerr
				}
			}
			if jsonFlag(cmd) {
				return printJSON(cmd, struct {
					Delivered []sent `json:"delivered"`
				}{delivered})
			}
			for _, r := range delivered {
				if _, err := fmt.Fprintln(cmd.OutOrStdout(), r.Short); err != nil {
					return err
				}
			}
			return nil
		}),
	}
}
