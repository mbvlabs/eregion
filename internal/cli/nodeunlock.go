package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/contract"
)

// runNodeUnlockSSH is `eregion node unlock-ssh`, the break-glass command an
// operator runs as root from the provider console. It is intentionally outside
// the operator command tree: the saved context reaches the node over WireGuard,
// so this path exists only when that channel is unavailable.
func runNodeUnlockSSH(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("node unlock-ssh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	minutes := flags.Int("minutes", 15, "Minutes to keep public SSH open (1–240)")
	relock := flags.Bool("relock", false, "Internal: close public SSH after the break-glass window")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: eregion node unlock-ssh [--minutes N]")
		return 2
	}
	fail := func(code contract.Code, message, hint string) int {
		fmt.Fprintf(stderr, "error [%s]: %s\n", code, message)
		if hint != "" {
			fmt.Fprintf(stderr, "hint: %s\n", hint)
		}
		return contract.ExitCode(contract.NewFault(code, message, hint))
	}
	if runtime.GOOS != "linux" {
		return fail(contract.PreconditionFailed, "break-glass SSH unlock runs on the node only", "Run it on the Linux node from the provider console.")
	}
	if os.Geteuid() != 0 {
		return fail(contract.Forbidden, "break-glass SSH unlock must run as root", "Log in as root from the provider console.")
	}
	var err error
	if *relock {
		err = access.Relock(stdout, stderr)
	} else {
		err = access.Unlock(*minutes, stdout, stderr)
	}
	if err != nil {
		return fail(contract.Unavailable, "the break-glass access change failed", err.Error())
	}
	return 0
}
