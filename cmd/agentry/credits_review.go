package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/blaktron/agentry-cli/pkg/drafter"
	"github.com/blaktron/agentry-cli/pkg/managed"
)

// creditsPay opens, for one command, the operations an organization's
// credits pay for AI Review and web search with (drafter.Pay): with the
// person's sign-in to the registry the managed settings came from, saying
// each one and what it cost. end ends every one it opened; the command
// defers it.
func creditsPay(errOut io.Writer) (pay drafter.Pay, end func()) {
	var ops []*managed.Operation
	pay = func(ctx context.Context, reg, purpose string) (*managed.Operation, error) {
		reg = strings.TrimRight(cmpReg(reg), "/")
		signIn, err := credentialFor(reg)
		if err != nil {
			return nil, err
		}
		op, err := managed.OpenOperation(ctx, httpClient, reg, signIn, purpose)
		if err != nil {
			return nil, err
		}
		ops = append(ops, op)
		what := "AI Review"
		if purpose == managed.PurposeSearch {
			what = "Web search, with AI Review reading every result,"
		}
		_, _ = fmt.Fprintf(errOut, "%s is paid with your organization's credits: an operation on %s, %s.\n", what, reg, creditsWord(op.Credits))
		return op, nil
	}
	end = func() {
		for _, op := range ops {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			if err := op.End(ctx); err != nil {
				_, _ = fmt.Fprintf(errOut, "The AI Review operation could not be ended (%s); its token lapses at %s.\n",
					safeTerminalText(err.Error(), 200), op.ExpiresAt.Local().Format(time.Kitchen))
			}
			cancel()
		}
		ops = nil
	}
	return pay, end
}

// cmpReg is the registry an operation is opened on: the managed
// settings' own, else this client's.
func cmpReg(reg string) string {
	if reg != "" {
		return reg
	}
	return registryURL()
}

func creditsWord(n int) string {
	if n == 1 {
		return "1 credit"
	}
	return fmt.Sprintf("%d credits", n)
}
