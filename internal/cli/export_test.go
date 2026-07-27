package cli

import (
	"context"
	"io"
)

// RunForTest runs the command with explicit arguments and streams and returns
// the exit code.
//
// It exists so the exit codes, which other things branch on, can be tested
// without starting a process. Being in a _test file keeps it out of the shipped
// binary.
func RunForTest(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := New("test", "test")
	if args == nil {
		// cobra falls back to os.Args when handed a nil slice, which would let the
		// test runner's own arguments reach the command.
		args = []string{}
	}
	cmd.SetArgs(args)
	cmd.SetIn(stdin)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	return execute(ctx, cmd)
}
